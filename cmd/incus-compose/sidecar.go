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

// sidecarEnsureNetwork brings up the network the sidecar attaches to.
func sidecarEnsureNetwork(ctx context.Context, c *client.Client, ref sidecarNetworkRef, sidecarName string) (*client.Network, error) {
	var netRes client.Resource
	var err error

	switch {
	case ref.deflt:
		cfg := &client.NetworkConfig{OverrideName: ref.incusName}
		if ovn, _ := c.Global().DetectOVN(); ovn {
			cfg.Type = "ovn"
		}

		netRes, err = c.Resource(client.KindNetwork, ref.name, cfg)
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

	if network.IncusName() == globalHealthdNetwork {
		patch := map[string]string{}
		cfg := network.State().IncusNetwork.Config
		if cfg == nil {
			cfg = map[string]string{}
		}

		if addr := cfg["ipv4.address"]; addr != "" && addr != "none" && cfg["ipv4.dhcp.ranges"] == "" {
			dhcpRange, err := client.CalcIPv4DHCPRange(addr)
			if err != nil {
				return nil, fmt.Errorf("calculating IPv4 DHCP range for %s: %w", network.IncusName(), err)
			}

			patch["ipv4.dhcp.ranges"] = dhcpRange
		}

		if network.State().IncusNetwork.Type == "bridge" {
			if addr := cfg["ipv6.address"]; addr != "" && addr != "none" && cfg["ipv6.dhcp.ranges"] == "" {
				dhcpRange, err := client.CalcIPv6DHCPRange(addr)
				if err != nil {
					return nil, fmt.Errorf("calculating IPv6 DHCP range for %s: %w", network.IncusName(), err)
				}

				patch["ipv6.dhcp.ranges"] = dhcpRange
				if cfg["ipv6.dhcp.stateful"] == "" {
					patch["ipv6.dhcp.stateful"] = "true"
				}
			}
		}

		if len(patch) > 0 {
			err = network.PatchConfig(ctx, patch)
			if err != nil {
				return nil, fmt.Errorf("configuring DHCP ranges for %s: %w", network.IncusName(), err)
			}
		}
	}

	return network, nil
}

// sidecarIncusURL is the endpoint the sidecar dials: override, then
// core.https_address once it names a host, then the network gateway.
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

		// An OVN network's gateway is on the logical router, not the host, so the
		// sidecar dials the endpoint the host is already reachable on.
		if network.State().IncusNetwork.Type != "ovn" {
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
	}

	// The sidecar resolves names in its own container, where the daemon's own
	// hostname is the container itself: a named remote lands on loopback there.
	// Resolve it here, where the name still means what it does to us.
	if host := u.Hostname(); net.ParseIP(host) == nil {
		ips, err := net.DefaultResolver.LookupHost(context.Background(), host)
		if err != nil {
			return nil, fmt.Errorf("resolving the Incus endpoint %q: %w", u.Host, err)
		}

		resolved := ""
		for _, ip := range ips {
			parsed := net.ParseIP(ip)
			if parsed != nil && parsed.To4() != nil {
				resolved = ip

				break
			}
		}

		if resolved == "" {
			return nil, fmt.Errorf("the Incus endpoint %q has no IPv4 address the %s container can dial", u.Host, sidecarName)
		}

		u.Host = net.JoinHostPort(resolved, u.Port())
	}

	if ip := net.ParseIP(u.Hostname()); ip != nil && ip.IsLoopback() {
		return nil, fmt.Errorf(
			"the Incus endpoint %q is a loopback address the %s container cannot reach; "+
				"bind core.https_address to a reachable address, or set %s", u.Host, sidecarName, envFlag)
	}

	return u, nil
}

// isLegacyGlobal reports whether the global project or icompose0 network is in
// a legacy non-OVN configuration on an OVN-capable host.
func isLegacyGlobal(ctx context.Context, gc *client.GlobalClient) (bool, error) {
	conn, err := gc.Connection()
	if err != nil {
		return false, err
	}

	net, _, err := conn.GetNetwork(ctx, "default", globalHealthdNetwork)
	if err == nil && net.Type != "ovn" {
		return true, nil
	}

	proj, _, err := conn.GetProject(ctx, globalProject)
	if err == nil {
		if proj.Config["features.networks"] != "true" {
			return true, nil
		}

		net, _, err = conn.GetNetwork(ctx, globalProject, globalHealthdNetwork)
		if err == nil && net.Type != "ovn" {
			return true, nil
		}
	}

	return false, nil
}

// upgradeGlobalProject upgrades the shared global project and icompose0 network
// to OVN networking if the host supports OVN and legacy bridge resources exist.
func upgradeGlobalProject(ctx context.Context, gc *client.GlobalClient, network string) (func(), error) {
	if network != "" && network != globalHealthdNetwork {
		return nil, nil
	}

	ovn, err := gc.DetectOVN()
	if err != nil {
		return nil, fmt.Errorf("detecting OVN support: %w", err)
	}
	if !ovn {
		return nil, nil
	}

	legacy, err := isLegacyGlobal(ctx, gc)
	if err != nil {
		return nil, fmt.Errorf("checking legacy global state: %w", err)
	}
	if !legacy {
		return nil, nil
	}

	_, _ = gc.EnsureProject(globalProject, client.EnsureProjectWithCreate())

	release, err := gc.LockGlobalProject(ctx)
	if err != nil {
		return nil, fmt.Errorf("acquiring global upgrade lock: %w", err)
	}

	legacy, err = isLegacyGlobal(ctx, gc)
	if err != nil {
		release()
		return nil, fmt.Errorf("checking legacy global state: %w", err)
	}

	if !legacy {
		return release, nil
	}

	// Release the lock before deleting the project that holds it.
	release()

	gc.LogInfo("Upgrading incus-compose to OVN networking; recreating global project and network")

	conn, err := gc.Connection()
	if err != nil {
		return nil, err
	}

	certs, err := conn.GetCertificates(ctx)
	if err == nil {
		want := healthdCertName(globalProject, true)
		for _, cert := range certs {
			if cert.Name == want {
				_ = conn.DeleteCertificate(ctx, cert.Fingerprint)
			}
		}
	}

	_ = gc.DeleteProject(globalProject, true)
	_ = conn.DeleteNetwork(ctx, "default", globalHealthdNetwork)
	gc.InvalidateNetworkType("default", globalHealthdNetwork)
	gc.InvalidateNetworkType(globalProject, globalHealthdNetwork)

	return upgradeGlobalProject(ctx, gc, network)
}
