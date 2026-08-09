package iclient

import (
	"fmt"
	"os"
	"os/user"
	"path/filepath"

	"github.com/lxc/incus/v7/shared/api"
	"github.com/lxc/incus/v7/shared/util"
	"go.yaml.in/yaml/v4"
)

// configDir returns the directory holding the Incus CLI configuration.
func configDir() (string, error) {
	if os.Getenv("INCUS_CONF") != "" {
		return os.Getenv("INCUS_CONF"), nil
	}

	base, err := os.UserConfigDir()
	if err != nil || base == "" {
		usr, err := user.Current()
		if err != nil {
			return "", err
		}

		if usr.HomeDir == "" {
			return "", nil
		}

		base = filepath.Join(usr.HomeDir, ".config")
	}

	return filepath.Join(base, "incus"), nil
}

// configPaths returns the configuration file and the directory holding it.
func configPaths() (string, string, error) {
	dir, err := configDir()
	if err != nil {
		return "", "", err
	}

	if dir == "" {
		return "", "", nil
	}

	path := os.ExpandEnv(filepath.Join(dir, "config.yml"))

	return path, filepath.Dir(path), nil
}

// ReadConfig reads the CLI configuration at path, falling back to the default
// location when path is empty and to the built-in defaults when no file exists.
func ReadConfig(path string) (*Config, error) {
	dir := filepath.Dir(path)

	if path == "" {
		var err error

		path, dir, err = configPaths()
		if err != nil {
			return nil, err
		}
	}

	if path == "" || !util.PathExists(path) {
		c := ConfigDefaultConfig()
		c.ConfigDir = dir

		return c, nil
	}

	content, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading the configuration file: %w", err)
	}

	c := &Config{ConfigDir: dir}

	err = yaml.Load(content, c)
	if err != nil {
		return nil, fmt.Errorf("decoding the configuration: %w", err)
	}

	if c.Remotes == nil {
		c.Remotes = map[string]ConfigRemote{}
	}

	for k, r := range c.Remotes {
		if !r.Public && r.AuthType == "" {
			r.AuthType = api.AuthenticationMethodTLS
			c.Remotes[k] = r
		}
	}

	err = c.addGlobalRemotes()
	if err != nil {
		return nil, err
	}

	for k, r := range c.Remotes {
		if r.Protocol == "" {
			r.Protocol = "incus"
			c.Remotes[k] = r
		}
	}

	c.Remotes["local"] = ConfigLocalRemote

	if c.DefaultRemote == "" {
		c.DefaultRemote = ConfigDefaultConfig().DefaultRemote
	}

	return c, nil
}

// addGlobalRemotes merges the system-wide remotes that the user's own
// configuration does not already name.
func (c *Config) addGlobalRemotes() error {
	path := configGlobalPath("config.yml")
	if !util.PathExists(path) {
		return nil
	}

	content, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("reading the global configuration: %w", err)
	}

	global := &Config{}

	err = yaml.Load(content, global)
	if err != nil {
		return fmt.Errorf("decoding the global configuration: %w", err)
	}

	for k, r := range global.Remotes {
		_, ok := c.Remotes[k]
		if ok {
			continue
		}

		r.Global = true
		c.Remotes[k] = r
	}

	return nil
}
