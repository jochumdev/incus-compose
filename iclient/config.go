package iclient

import (
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"

	"github.com/lxc/incus/v7/shared/util"
)

// ConfigRemoteTLS holds inline TLS material for a remote.
type ConfigRemoteTLS struct {
	Certificate string `json:"certificate"`
	Key         string `json:"key"`
	CA          string `json:"ca"`
}

// ConfigRemote holds the details for communicating with one Incus daemon.
type ConfigRemote struct {
	Addrs           []string         `yaml:"-"`
	LastWorkingAddr string           `yaml:"last_working_address,omitempty"`
	AuthType        string           `yaml:"auth_type,omitempty"`
	KeepAlive       int              `yaml:"keepalive,omitempty"`
	Project         string           `yaml:"project,omitempty"`
	Protocol        string           `yaml:"protocol,omitempty"`
	CredHelper      string           `yaml:"credentials_helper,omitempty"`
	Public          bool             `yaml:"public"`
	Global          bool             `yaml:"-"`
	Static          bool             `yaml:"-"`
	TLS             *ConfigRemoteTLS `yaml:"-"`
}

// UnmarshalYAML reads `addr` as a comma-separated list into Addrs.
func (r *ConfigRemote) UnmarshalYAML(unmarshal func(any) error) error {
	type R ConfigRemote

	tmp := struct {
		*R    `yaml:",inline"`
		Addrs string `yaml:"addr"`
	}{
		R: (*R)(r),
	}

	err := unmarshal(&tmp)
	if err != nil {
		return err
	}

	r.Addrs = util.SplitNTrimSpace(tmp.Addrs, ",", -1, true)
	if r.Addrs == nil {
		return errors.New("remotes must have at least one address")
	}

	return nil
}

// rollingAddrs returns the addresses starting at the last known working one.
func (r ConfigRemote) rollingAddrs() []string {
	if len(r.Addrs) == 0 {
		return nil
	}

	start := 0
	if r.LastWorkingAddr != "" {
		for i, addr := range r.Addrs {
			if addr == r.LastWorkingAddr {
				start = i
				break
			}
		}
	}

	out := make([]string, 0, len(r.Addrs))
	for i := range r.Addrs {
		out = append(out, r.Addrs[(start+i)%len(r.Addrs)])
	}

	return out
}

// Config is the parsed Incus CLI configuration.
type Config struct {
	DefaultRemote string                  `yaml:"default-remote"`
	Remotes       map[string]ConfigRemote `yaml:"remotes"`

	// ConfigDir is where the configuration was read from; the certificate
	// paths hang off it.
	ConfigDir string `yaml:"-"`

	// ProjectOverride beats a remote's own Project.
	ProjectOverride string `yaml:"-"`

	// UserAgent is sent with every request.
	UserAgent string `yaml:"-"`
}

// ConfigLocalRemote is the unix socket remote, which cannot be removed.
var ConfigLocalRemote = ConfigRemote{
	Addrs:    []string{"unix://"},
	Static:   true,
	Public:   false,
	Protocol: "incus",
}

// ConfigImagesRemote is the community image server.
var ConfigImagesRemote = ConfigRemote{
	Addrs:    []string{"https://images.linuxcontainers.org"},
	Public:   true,
	Protocol: "simplestreams",
}

// ConfigDefaultRemotes is the set used when there is no configuration file.
var ConfigDefaultRemotes = map[string]ConfigRemote{
	"images": ConfigImagesRemote,
	"local":  ConfigLocalRemote,
}

// ConfigDefaultConfig returns the configuration used when no file exists.
func ConfigDefaultConfig() *Config {
	return &Config{
		Remotes:       maps.Clone(ConfigDefaultRemotes),
		DefaultRemote: "local",
	}
}

// configGlobalPath joins the system-wide configuration directory.
func configGlobalPath(paths ...string) string {
	dir := "/etc/incus"
	v, ok := os.LookupEnv("INCUS_GLOBAL_CONF")
	if ok {
		dir = v
	}

	return filepath.Join(append([]string{dir}, paths...)...)
}

// Path joins the configuration directory with the given elements.
func (c *Config) Path(paths ...string) string {
	return filepath.Join(append([]string{c.ConfigDir}, paths...)...)
}

// ServerCertPath returns where the remote's pinned server certificate lives.
func (c *Config) ServerCertPath(remote string) string {
	if c.Remotes[remote].Global {
		return configGlobalPath("servercerts", fmt.Sprintf("%s.crt", remote))
	}

	return c.Path("servercerts", fmt.Sprintf("%s.crt", remote))
}

// clientCertPaths returns the certificate, key and CA paths for a remote,
// preferring a per-remote set over the shared one.
func (c *Config) clientCertPaths(remote string) (cert, key, ca string) {
	perRemote := c.Path("clientcerts", fmt.Sprintf("%s.crt", remote))
	if util.PathExists(perRemote) {
		return perRemote,
			c.Path("clientcerts", fmt.Sprintf("%s.key", remote)),
			c.Path("clientcerts", fmt.Sprintf("%s.ca", remote))
	}

	return c.Path("client.crt"), c.Path("client.key"), c.Path("client.ca")
}

// readIfPresent returns the file's contents, or "" when it does not exist.
func readIfPresent(path string) (string, error) {
	if !util.PathExists(path) {
		return "", nil
	}

	content, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("reading %s: %w", path, err)
	}

	return string(content), nil
}
