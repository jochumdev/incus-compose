package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

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

// sidecarHTTPResponse holds the HTTP response for an endpoint queried via port-forward.
type sidecarHTTPResponse struct {
	Endpoint   string
	StatusCode int
	Body       string
}

// sidecarQueryEndpoints tunnels to targetPort on instanceName via incus port-forward
// and queries the specified HTTP endpoints in order, returning their status codes and bodies.
func sidecarQueryEndpoints(ctx context.Context, instanceName, project string, targetPort int, endpoints []string) ([]sidecarHTTPResponse, error) {
	if len(endpoints) == 0 {
		return nil, nil
	}

	var lc net.ListenConfig
	listener, err := lc.Listen(ctx, "tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("allocating local port: %w", err)
	}

	tcpAddr, ok := listener.Addr().(*net.TCPAddr)
	if !ok {
		_ = listener.Close()
		return nil, errors.New("listener is not TCP")
	}

	localPort := strconv.Itoa(tcpAddr.Port)
	err = listener.Close()
	if err != nil {
		return nil, err
	}

	path, err := exec.LookPath("incus")
	if err != nil {
		return nil, errors.New("'incus' not found in PATH")
	}

	fCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	fCmd := exec.CommandContext(fCtx, path, "port-forward", instanceName, strconv.Itoa(targetPort), localPort, "--project", project) //nolint:gosec
	fCmd.Env = append(os.Environ(), "INCUS_PROJECT="+project)

	err = fCmd.Start()
	if err != nil {
		return nil, fmt.Errorf("starting port-forward: %w", err)
	}
	defer func() {
		cancel()
		_ = fCmd.Wait()
	}()

	dialer := &net.Dialer{Timeout: 50 * time.Millisecond}
	ready := false
	for range 50 {
		conn, dialErr := dialer.DialContext(ctx, "tcp", "127.0.0.1:"+localPort)
		if dialErr == nil {
			_ = conn.Close()
			ready = true
			break
		}

		if fCmd.ProcessState != nil && fCmd.ProcessState.Exited() {
			break
		}

		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}

	if !ready {
		return nil, errors.New("port-forward failed to open port")
	}

	httpClient := &http.Client{Timeout: 5 * time.Second}
	responses := make([]sidecarHTTPResponse, 0, len(endpoints))

	for _, ep := range endpoints {
		p := ep
		if !strings.HasPrefix(p, "/") {
			p = "/" + p
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://127.0.0.1:"+localPort+p, nil)
		if err != nil {
			return nil, fmt.Errorf("creating request for %s: %w", ep, err)
		}

		resp, err := httpClient.Do(req)
		if err != nil {
			return nil, fmt.Errorf("querying %s: %w", ep, err)
		}

		bodyBytes, err := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("reading response for %s: %w", ep, err)
		}

		responses = append(responses, sidecarHTTPResponse{
			Endpoint:   ep,
			StatusCode: resp.StatusCode,
			Body:       string(bodyBytes),
		})
	}

	return responses, nil
}

// sidecarStatusReport holds the status and endpoints for a sidecar.
type sidecarStatusReport struct {
	Status  string `json:"status"`
	IPv4    string `json:"ipv4"`
	IPv6    string `json:"ipv6"`
	Metrics string `json:"metrics,omitempty"`
}

// sidecarIPAddresses extracts global IPv4 and IPv6 addresses from state, falling back to configured devices.
func sidecarIPAddresses(state *client.InstanceState) (string, string) {
	var ipv4s, ipv6s []string
	if state.IncusInstanceState != nil {
		for _, nw := range state.IncusInstanceState.Network {
			if nw.Type == "loopback" {
				continue
			}

			for _, a := range nw.Addresses {
				if a.Scope != "global" || a.Address == "" {
					continue
				}

				switch a.Family {
				case "inet":
					ipv4s = append(ipv4s, a.Address)
				case "inet6":
					ipv6s = append(ipv6s, a.Address)
				}
			}
		}
	}

	if len(ipv4s) == 0 && state.IncusInstance != nil {
		for _, dev := range state.IncusInstance.Devices {
			if dev["type"] == "nic" || dev["network"] != "" {
				if addr := dev["ipv4.address"]; addr != "" && addr != "none" {
					ip, _, _ := strings.Cut(addr, "/")
					ipv4s = append(ipv4s, ip)
				}

				if addr := dev["ipv6.address"]; addr != "" && addr != "none" {
					ip, _, _ := strings.Cut(addr, "/")
					ipv6s = append(ipv6s, ip)
				}
			}
		}
	}

	return strings.Join(ipv4s, ", "), strings.Join(ipv6s, ", ")
}

// fetchSidecarStatus queries /ready (and optionally /metrics) on the sidecar via port-forward.
func fetchSidecarStatus(ctx context.Context, instanceName, project string, httpPort int, includeMetrics bool) (string, string) {
	status := "unready"
	var metricsBody string

	endpoints := []string{"/ready"}
	if includeMetrics {
		endpoints = append(endpoints, "/metrics")
	}

	resps, err := sidecarQueryEndpoints(ctx, instanceName, project, httpPort, endpoints)
	if err == nil && len(resps) >= 1 {
		readyResp := resps[0]
		readyBody := strings.TrimSpace(readyResp.Body)
		if readyBody != "" {
			status = readyBody
		} else if readyResp.StatusCode == http.StatusOK {
			status = "ready"
		}

		if includeMetrics && len(resps) == 2 {
			metricsResp := resps[1]
			if metricsResp.StatusCode == http.StatusOK {
				metricsBody = metricsResp.Body
			}
		}
	}

	return status, metricsBody
}

// renderSidecarStatus renders sidecar status in text or json format to w.
func renderSidecarStatus(w io.Writer, format, status, ipv4, ipv6, metricsBody string) error {
	switch format {
	case "json":
		report := sidecarStatusReport{
			Status:  status,
			IPv4:    ipv4,
			IPv6:    ipv6,
			Metrics: metricsBody,
		}
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(report)

	case "text":
		_, _ = fmt.Fprintf(w, "Status: %s\n", status)
		if ipv4 != "" {
			_, _ = fmt.Fprintf(w, "IPv4: %s\n", ipv4)
		} else {
			_, _ = fmt.Fprintln(w, "IPv4:")
		}

		if ipv6 != "" {
			_, _ = fmt.Fprintf(w, "IPv6: %s\n", ipv6)
		} else {
			_, _ = fmt.Fprintln(w, "IPv6:")
		}

		if metricsBody != "" {
			_, _ = fmt.Fprintln(w, "Metrics:")
			_, _ = fmt.Fprint(w, metricsBody)
			if !strings.HasSuffix(metricsBody, "\n") {
				_, _ = fmt.Fprintln(w)
			}
		}

		return nil

	default:
		return fmt.Errorf("invalid format: %s (must be text or json)", format)
	}
}
