package iclient

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// writeConfig writes a config.yml plus any extra files into a fresh dir. The
// host's /etc/incus is still merged in, but never overrides what the fixture names.
func writeConfig(t *testing.T, config string, extra map[string]string) string {
	t.Helper()

	dir := t.TempDir()

	err := os.WriteFile(filepath.Join(dir, "config.yml"), []byte(config), 0o600)
	require.NoError(t, err)

	for name, content := range extra {
		path := filepath.Join(dir, name)
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
		require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
	}

	return filepath.Join(dir, "config.yml")
}

func TestReadConfigFillsDefaults(t *testing.T) {
	t.Parallel()

	path := writeConfig(t, `
default-remote: srv
remotes:
  srv:
    addr: https://one:8443,https://two:8443
    project: apps
  pub:
    addr: https://images.example.org
    public: true
    protocol: simplestreams
`, nil)

	c, err := ReadConfig(path)
	require.NoError(t, err)

	require.Equal(t, "srv", c.DefaultRemote)
	require.Equal(t, []string{"https://one:8443", "https://two:8443"}, c.Remotes["srv"].Addrs)

	// A private remote with no auth_type defaults to tls, a public one is left alone.
	require.Equal(t, "tls", c.Remotes["srv"].AuthType)
	require.Empty(t, c.Remotes["pub"].AuthType)

	// Protocol defaults to incus, an explicit one survives.
	require.Equal(t, "incus", c.Remotes["srv"].Protocol)
	require.Equal(t, "simplestreams", c.Remotes["pub"].Protocol)

	// local is static and always present.
	require.Equal(t, []string{"unix://"}, c.Remotes["local"].Addrs)
	require.True(t, c.Remotes["local"].Static)
}

func TestReadConfigMissingFileIsDefaults(t *testing.T) {
	t.Parallel()

	c, err := ReadConfig(filepath.Join(t.TempDir(), "nope.yml"))
	require.NoError(t, err)

	require.Equal(t, "local", c.DefaultRemote)
	require.Contains(t, c.Remotes, "local")
	require.Contains(t, c.Remotes, "images")
}

func TestRemoteInfosTLS(t *testing.T) {
	t.Parallel()

	path := writeConfig(t, `
default-remote: srv
remotes:
  srv:
    addr: https://one:8443,https://two:8443
    last_working_address: https://two:8443
    project: apps
`, map[string]string{
		"client.crt":          "CERT",
		"client.key":          "KEY",
		"servercerts/srv.crt": "SERVER",
	})

	c, err := ReadConfig(path)
	require.NoError(t, err)

	info, err := c.RemoteInfos("srv")
	require.NoError(t, err)

	// The last working address is tried first.
	require.Equal(t, []string{"https://two:8443", "https://one:8443"}, info.Addrs)

	require.Equal(t, "CERT", info.ClientCert)
	require.Equal(t, "KEY", info.ClientKey)
	require.Equal(t, "SERVER", info.ServerCert)

	// Absent is empty, not an error.
	require.Empty(t, info.ClientCA)
	require.False(t, info.Unix())
}

func TestRemoteInfosPerRemoteCertsWin(t *testing.T) {
	t.Parallel()

	path := writeConfig(t, `
remotes:
  srv:
    addr: https://one:8443
`, map[string]string{
		"client.crt":          "SHARED",
		"clientcerts/srv.crt": "MINE",
		"clientcerts/srv.key": "MINEKEY",
	})

	c, err := ReadConfig(path)
	require.NoError(t, err)

	info, err := c.RemoteInfos("srv")
	require.NoError(t, err)

	require.Equal(t, "MINE", info.ClientCert)
	require.Equal(t, "MINEKEY", info.ClientKey)
}

func TestRemoteInfosUnixHasNoTLS(t *testing.T) {
	t.Parallel()

	path := writeConfig(t, `
remotes: {}
`, map[string]string{"client.crt": "CERT"})

	c, err := ReadConfig(path)
	require.NoError(t, err)

	info, err := c.RemoteInfos("local")
	require.NoError(t, err)

	require.True(t, info.Unix())
	require.Empty(t, info.ClientCert)
	require.Empty(t, info.ServerCert)
}

func TestRemoteInfosDefault(t *testing.T) {
	t.Parallel()

	// `project` is no field of ours any more; a config carrying one still parses.
	path := writeConfig(t, `
default-remote: srv
remotes:
  srv:
    addr: https://one:8443
    project: apps
`, nil)

	c, err := ReadConfig(path)
	require.NoError(t, err)

	// An empty name resolves the default remote.
	info, err := c.RemoteInfos("")
	require.NoError(t, err)
	require.Equal(t, "srv", info.Name)
}

func TestRemoteInfosUnknown(t *testing.T) {
	t.Parallel()

	path := writeConfig(t, "remotes: {}\n", nil)

	c, err := ReadConfig(path)
	require.NoError(t, err)

	_, err = c.RemoteInfos("nope")
	require.ErrorIs(t, err, ErrConfigRemoteNotFound)
}
