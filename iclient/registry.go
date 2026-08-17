package iclient

import (
	"fmt"
	"net"
	"net/http"
	"net/url"
	"time"

	incusTLS "github.com/lxc/incus/v7/shared/tls"
	"oras.land/oras-go/v2/registry"
	"oras.land/oras-go/v2/registry/remote"
	"oras.land/oras-go/v2/registry/remote/auth"
	"oras.land/oras-go/v2/registry/remote/retry"
)

// A registry answers metadata promptly, where an Incus operation long-polls.
const registryResponseHeaderTimeout = 30 * time.Second

// NewRepository returns an OCI Distribution client for one image on the
// registry an "oci" remote points at. image carries the tag or digest,
// e.g. "library/redis:alpine".
//
// The registry is the remote's own address, so a mirror stands in for what it
// mirrors, and the proxy environment applies as it does to a Connection.
// Credentials are info's Username and Password, which RemoteInfos fills from
// the remote's credentials helper; an address still carrying its own is
// refused rather than silently reached anonymously.
func NewRepository(info *ConfigRemoteInfo, image string) (*remote.Repository, error) {
	if info.Protocol != "oci" {
		return nil, fmt.Errorf("%q: %w", info.Name, ErrRegistryProtocol)
	}

	if len(info.Addrs) == 0 {
		return nil, fmt.Errorf("%q: %w", info.Name, ErrConnectionNoAddress)
	}

	// The address is never echoed back: a hand-built info may still hold a
	// password in it, which is what the next check is about.
	addr, err := url.Parse(info.Addrs[0])
	if err != nil {
		return nil, fmt.Errorf("%q: parsing the registry address: %w", info.Name, err)
	}

	if addr.User != nil {
		return nil, fmt.Errorf("%q: %w", info.Name, ErrRegistryAddrCredentials)
	}

	ref, err := registry.ParseReference(addr.Host + "/" + image)
	if err != nil {
		return nil, fmt.Errorf("%q: %w", info.Name, err)
	}

	// A registry is handed no client certificate; only its own may be pinned.
	tlsConfig, err := incusTLS.GetTLSConfigMem("", "", info.ClientCA, info.ServerCert, info.InsecureSkipVerify)
	if err != nil {
		return nil, fmt.Errorf("%q: building the TLS config: %w", info.Name, err)
	}

	transport := incusTransport()
	transport.TLSClientConfig = tlsConfig
	transport.Proxy = http.ProxyFromEnvironment
	transport.DialContext = (&net.Dialer{Timeout: incusDialTimeout}).DialContext
	transport.ResponseHeaderTimeout = registryResponseHeaderTimeout

	client := &auth.Client{
		Client: &http.Client{Transport: retry.NewTransport(transport)},
		Cache:  auth.NewCache(),
	}

	if info.UserAgent != "" {
		client.SetUserAgent(info.UserAgent)
	}

	if info.Username != "" || info.Password != "" {
		// Keyed on Host: auth.Client maps docker.io to registry-1.docker.io first.
		client.Credential = auth.StaticCredential(ref.Host(), auth.Credential{
			Username: info.Username,
			Password: info.Password,
		})
	}

	return &remote.Repository{
		Client:    client,
		Reference: ref,
		PlainHTTP: addr.Scheme == "http",
	}, nil
}
