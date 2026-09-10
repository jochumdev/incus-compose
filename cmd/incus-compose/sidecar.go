package main

import (
	"context"
	"fmt"
	"net"
	"net/url"

	"github.com/lxc/incus-compose/client"
)

type sidecarNetworkRef struct {
	project   string
	name      string
	deflt     bool
	incusName string
}

// sidecarEnsureNetwork brings up the bridge the sidecar attaches to.
func sidecarEnsureNetwork(ctx context.Context, c *client.Client, ref sidecarNetworkRef, sidecarName string) (*client.Network, error) {
	var netRes client.Resource
	var err error

	switch {
	case ref.deflt:
		netRes, err = c.Resource(client.KindNetwork, ref.name, &client.NetworkConfig{OverrideName: ref.incusName})
	case ref.project != "" && ref.project != c.Project():
		var nc *client.Client
		nc, err = c.Global().EnsureProject(ref.project)
		if err != nil {
			return nil, fmt.Errorf("failed to fetch the %s network: %w", sidecarName, err)
		}

		netRes, err = nc.Resource(client.KindNetwork, ref.name, &client.NetworkConfig{External: true})
	default:
		netRes, err = c.Resource(client.KindNetwork, ref.name, &client.NetworkConfig{External: true})
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get a %s network: %w", sidecarName, err)
	}

	err = client.RunAction(ctx, netRes, client.ActionEnsure, client.OptionCreate())
	if err != nil {
		return nil, fmt.Errorf("failed to ensure a network for %s: %w", sidecarName, err)
	}

	network, ok := netRes.(*client.Network)
	if !ok {
		return nil, client.ErrUnknown.WithResource(netRes).WithText("failed to cast")
	}

	if !network.IsEnsured() {
		return nil, client.ErrNotEnsured.WithResource(network)
	}

	return network, nil
}

// sidecarIncusURL is the endpoint the sidecar dials: override, then
// core.https_address once it names a host, then the bridge gateway.
func sidecarIncusURL(c *client.Client, incusOverride *url.URL, network *client.Network, sidecarName, envFlag string) (*url.URL, error) {
	u := incusOverride

	if u == nil {
		addr, err := c.Global().HTTPSAddress()
		if err == nil {
			host, port, splitErr := net.SplitHostPort(addr)

			if ip := net.ParseIP(host); ip != nil && ip.IsUnspecified() {
				host = ""
			}

			if splitErr == nil && host != "" && port != "" {
				parsed, parseErr := url.Parse(fmt.Sprintf("https://%s:%s", host, port))
				if parseErr == nil {
					u = parsed
				}
			}
		}
	}

	if u == nil {
		if !c.IsRemote() {
			return nil, fmt.Errorf("%s works only with a https connection, provide one with %s", sidecarName, envFlag)
		}

		var err error

		u, err = c.Global().URL()
		if err != nil {
			return nil, fmt.Errorf("failed to get the url: %w", err)
		}

		cidr := network.State().IncusNetwork.Config["ipv4.address"]
		if cidr == "" {
			return nil, fmt.Errorf("ip of network %q is empty", network.Name())
		}

		ip, _, err := net.ParseCIDR(cidr)
		if err != nil {
			return nil, fmt.Errorf("parsing the address of network %q: %w", network.Name(), err)
		}

		u.Host = net.JoinHostPort(ip.String(), u.Port())
	}

	if ip := net.ParseIP(u.Hostname()); ip != nil && ip.IsLoopback() {
		return nil, fmt.Errorf(
			"the Incus endpoint %q is a loopback address the %s container cannot reach; "+
				"bind core.https_address to a reachable address, or set %s", u.Host, sidecarName, envFlag)
	}

	return u, nil
}
