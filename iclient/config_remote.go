package iclient

import (
	"fmt"
	"slices"
	"strings"

	"github.com/lxc/incus/v7/shared/api"
)

// ConfigRemoteInfo is everything needed to dial one remote, read off disk once.
type ConfigRemoteInfo struct {
	Name string

	// Addrs starts at the remote's last known working address.
	Addrs    []string
	Protocol string
	AuthType string

	// Username and Password reach an "oci" registry, from its credentials
	// helper or else from its address. They are deliberately not left in
	// Addrs, which is logged and formatted into errors.
	Username string
	Password string

	Public    bool
	KeepAlive int
	UserAgent string

	// InsecureSkipVerify accepts any server certificate, for registering
	// against one whose certificate is not known yet.
	InsecureSkipVerify bool

	// TLS material, all empty for a unix-socket-only remote.
	ServerCert string
	ClientCert string
	ClientKey  string
	ClientCA   string
}

// Unix reports whether every address is the local unix socket.
func (i *ConfigRemoteInfo) Unix() bool {
	return len(i.Addrs) > 0 && !slices.ContainsFunc(i.Addrs, func(addr string) bool {
		return !strings.HasPrefix(addr, "unix:")
	})
}

// RemoteInfos resolves a remote into what it takes to connect to it. An empty
// name means the default remote.
func (c *Config) RemoteInfos(remote string) (*ConfigRemoteInfo, error) {
	if remote == "" {
		remote = c.DefaultRemote
	}

	r, ok := c.Remotes[remote]
	if !ok {
		return nil, fmt.Errorf("%q: %w", remote, ErrConfigRemoteNotFound)
	}

	info := &ConfigRemoteInfo{
		Name:      remote,
		Addrs:     r.rollingAddrs(),
		Protocol:  r.Protocol,
		AuthType:  r.AuthType,
		Public:    r.Public,
		KeepAlive: r.KeepAlive,
		UserAgent: c.UserAgent,
	}

	if len(info.Addrs) == 0 {
		return nil, fmt.Errorf("%q: remote has no address", remote)
	}

	if r.Protocol == "oci" {
		err := c.applyCredentials(remote, r, info)
		if err != nil {
			return nil, err
		}
	}

	// A unix socket carries no TLS; only a private incus remote has a client certificate.
	if info.Unix() {
		return info, nil
	}

	var err error

	info.ServerCert, err = readIfPresent(c.ServerCertPath(remote))
	if err != nil {
		return nil, err
	}

	if r.Protocol != "incus" || r.AuthType == api.AuthenticationMethodOIDC {
		return info, nil
	}

	if r.TLS != nil {
		info.ClientCert, info.ClientKey, info.ClientCA = r.TLS.Certificate, r.TLS.Key, r.TLS.CA

		return info, nil
	}

	certPath, keyPath, caPath := c.clientCertPaths(remote)

	for _, f := range []struct {
		path string
		into *string
	}{
		{certPath, &info.ClientCert},
		{keyPath, &info.ClientKey},
		{caPath, &info.ClientCA},
	} {
		*f.into, err = readIfPresent(f.path)
		if err != nil {
			return nil, err
		}
	}

	return info, nil
}
