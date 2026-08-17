package iclient

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// credHelper writes an executable stand-in for a docker credentials helper,
// recording each run's stdin - the host it was asked about - in a log file.
func credHelper(t *testing.T, body string) (helper string, logPath string) {
	t.Helper()

	dir := t.TempDir()
	helper = filepath.Join(dir, "docker-credential-fake")
	logPath = filepath.Join(dir, "runs")

	// One write per run: two helpers appending concurrently would otherwise
	// interleave a host and its newline, and the log would undercount.
	script := fmt.Sprintf("#!/bin/sh\nhost=$(cat)\necho \"$host\" >> %q\n%s\n", logPath, body)

	err := os.WriteFile(helper, []byte(script), 0o700)
	require.NoError(t, err)

	return helper, logPath
}

// runs returns the hosts the helper was handed, one per run.
func runs(t *testing.T, logPath string) []string {
	t.Helper()

	content, err := os.ReadFile(logPath)
	if os.IsNotExist(err) {
		return nil
	}

	require.NoError(t, err)

	return strings.Fields(string(content))
}

const credHelperOK = `echo '{"Username":"user","Secret":"s3cret"}'`

func TestRemoteInfosCredentials(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		name     string
		remote   string
		body     string
		wantUser string
		wantPass string
		wantAddr string
		wantRuns []string
	}{
		{
			name: "a helper supplies the login",
			remote: `
    addr: https://registry.example.com
    protocol: oci
    credentials_helper: %s
`,
			body:     credHelperOK,
			wantUser: "user",
			wantPass: "s3cret",
			wantAddr: "https://registry.example.com",
			wantRuns: []string{"registry.example.com"},
		},
		{
			name: "a helper beats an address carrying its own",
			remote: `
    addr: https://old:stale@registry.example.com
    protocol: oci
    credentials_helper: %s
`,
			body:     credHelperOK,
			wantUser: "user",
			wantPass: "s3cret",
			wantAddr: "https://registry.example.com",
			wantRuns: []string{"registry.example.com"},
		},
		{
			name: "without a helper the address' own login moves into the fields",
			remote: `
    addr: https://user:s3cret@registry.example.com
    protocol: oci
`,
			wantUser: "user",
			wantPass: "s3cret",
			wantAddr: "https://registry.example.com",
		},
		{
			name: "a token-only login keeps its empty username",
			remote: `
    addr: https://registry.example.com
    protocol: oci
    credentials_helper: %s
`,
			body:     `echo '{"Username":"","Secret":"identity-token"}'`,
			wantPass: "identity-token",
			wantAddr: "https://registry.example.com",
			wantRuns: []string{"registry.example.com"},
		},
		{
			name: "a registry with no login at all",
			remote: `
    addr: https://registry.example.com
    protocol: oci
`,
			wantAddr: "https://registry.example.com",
		},
		{
			name: "an incus remote is left alone, helper and all",
			remote: `
    addr: https://user:s3cret@incus.example.com:8443
    protocol: incus
    credentials_helper: %s
`,
			body:     credHelperOK,
			wantAddr: "https://user:s3cret@incus.example.com:8443",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			helper, logPath := credHelper(t, tt.body)

			remote := tt.remote
			if strings.Contains(remote, "%s") {
				remote = fmt.Sprintf(remote, helper)
			}

			c, err := ReadConfig(writeConfig(t, "remotes:\n  reg:"+remote, nil))
			require.NoError(t, err)

			info, err := c.RemoteInfos("reg")
			require.NoError(t, err)

			assert.Equal(t, tt.wantUser, info.Username)
			assert.Equal(t, tt.wantPass, info.Password)
			assert.Equal(t, []string{tt.wantAddr}, info.Addrs)
			assert.Equal(t, tt.wantRuns, runs(t, logPath))
		})
	}
}

func TestRemoteInfosCredentialsFails(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		name string
		body string
		errs string
	}{
		{
			name: "the helper has no credentials for the registry",
			body: "echo 'credentials not found' >&2\nexit 1",
			errs: "credentials not found",
		},
		{
			name: "the helper answers with something that is not JSON",
			body: "echo 'not json'",
			errs: "decoding its response",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			helper, _ := credHelper(t, tt.body)

			c, err := ReadConfig(writeConfig(t, fmt.Sprintf(`
remotes:
  reg:
    addr: https://registry.example.com
    protocol: oci
    credentials_helper: %s
`, helper), nil))
			require.NoError(t, err)

			info, err := c.RemoteInfos("reg")
			require.ErrorIs(t, err, ErrCredHelper)
			assert.Contains(t, err.Error(), tt.errs)
			assert.Nil(t, info)
		})
	}
}

func TestRemoteInfosCredentialsMissingHelper(t *testing.T) {
	t.Parallel()

	c, err := ReadConfig(writeConfig(t, `
remotes:
  reg:
    addr: https://registry.example.com
    protocol: oci
    credentials_helper: docker-credential-does-not-exist
`, nil))
	require.NoError(t, err)

	_, err = c.RemoteInfos("reg")
	require.ErrorIs(t, err, ErrCredHelper)
}

// A run resolving one registry for a dozen images must not spawn a dozen
// helpers - each would hit the keyring, and some prompt.
func TestRemoteInfosCredentialsRunOnce(t *testing.T) {
	t.Parallel()

	helper, logPath := credHelper(t, credHelperOK)

	c, err := ReadConfig(writeConfig(t, fmt.Sprintf(`
remotes:
  reg:
    addr: https://registry.example.com
    protocol: oci
    credentials_helper: %s
  other:
    addr: https://other.example.com
    protocol: oci
    credentials_helper: %s
`, helper, helper), nil))
	require.NoError(t, err)

	wg := sync.WaitGroup{}
	for _, remote := range []string{"reg", "other", "reg", "other", "reg", "other", "reg", "other"} {
		wg.Add(1)

		go func() {
			defer wg.Done()

			info, err := c.RemoteInfos(remote)
			assert.NoError(t, err)
			assert.Equal(t, "user", info.Username)
		}()
	}

	wg.Wait()

	// One run per remote, not one per call, and both remotes did resolve.
	hosts := runs(t, logPath)
	assert.ElementsMatch(t, []string{"registry.example.com", "other.example.com"}, hosts)
}
