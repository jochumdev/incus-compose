package incustrust

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// pair writes a certificate and key into dir and reports their paths. Generated
// rather than hard-coded, because a certificate literal expires.
func pair(t *testing.T, dir, certName, keyName string) (string, string) {
	t.Helper()

	certPEM, keyPEM, err := generate("test")
	require.NoError(t, err)

	certPath := filepath.Join(dir, certName)
	keyPath := filepath.Join(dir, keyName)

	require.NoError(t, os.MkdirAll(dir, 0o700))
	require.NoError(t, os.WriteFile(certPath, certPEM, 0o600))
	require.NoError(t, os.WriteFile(keyPath, keyPEM, 0o600))

	return certPath, keyPath
}

// TestConnectPrefersAnExplicitPair pins the first rule of the order: a
// configured certificate is used as it stands, whatever else is lying around.
func TestConnectPrefersAnExplicitPair(t *testing.T) {
	dir := t.TempDir()

	certPath, keyPath := pair(t, dir, "given.crt", "given.key")

	// Everything else is present and none of it is reached: a token would enroll
	// against an endpoint that is not there.
	_, _ = pair(t, filepath.Join(dir, "data"), CertFile, KeyFile)
	require.NoError(t, os.WriteFile(filepath.Join(dir, TokenFile), []byte("tok"), 0o600))

	conn, err := Connect(t.Context(), Config{
		Name:       "test",
		URL:        "https://10.0.0.1:8443",
		ClientCert: certPath,
		ClientKey:  keyPath,
		DataDir:    filepath.Join(dir, "data"),
		SecretsDir: dir,
	})
	require.NoError(t, err)
	assert.NotNil(t, conn)
}

// TestConnectUsesTheEnrolledPair pins the second rule: a pair enrolled earlier
// beats the token that earned it, because redeeming a spent token fails.
func TestConnectUsesTheEnrolledPair(t *testing.T) {
	dir := t.TempDir()
	data := filepath.Join(dir, "data")

	_, _ = pair(t, data, CertFile, KeyFile)
	require.NoError(t, os.WriteFile(filepath.Join(dir, TokenFile), []byte("tok"), 0o600))

	conn, err := Connect(t.Context(), Config{
		Name:       "test",
		URL:        "https://10.0.0.1:8443",
		DataDir:    data,
		SecretsDir: dir,
	})
	require.NoError(t, err)
	assert.NotNil(t, conn)
}

func TestConnectRefusals(t *testing.T) {
	cases := []struct {
		name string
		cfg  func(dir string) Config

		wantErr string
		wantIs  error
	}{
		{
			// Half a pair is a deployment that forgot a secret; falling through
			// to a token would connect as an identity it did not mean.
			name: "a certificate with no key",
			cfg: func(dir string) Config {
				certPath, _ := pair(t, dir, "given.crt", "given.key")

				return Config{Name: "test", URL: "https://10.0.0.1:8443", ClientCert: certPath}
			},
			wantErr: "client certificate and key are both or neither",
		},
		{
			// A mounted secret present but empty meant to supply one.
			name: "a token file that is empty",
			cfg: func(dir string) Config {
				require.NoError(t, os.WriteFile(filepath.Join(dir, TokenFile), []byte("  \n"), 0o600))

				return Config{
					Name: "test", URL: "https://10.0.0.1:8443",
					DataDir: filepath.Join(dir, "data"), SecretsDir: dir,
				}
			},
			wantErr: "the trust token file is empty",
		},
		{
			name: "nothing to connect with at all",
			cfg: func(dir string) Config {
				return Config{
					Name: "test", URL: "https://10.0.0.1:8443",
					DataDir: filepath.Join(dir, "data"), SecretsDir: dir,
				}
			},
			wantIs: ErrNoCredentials,
		},
		{
			// Configuration that cannot work, refused now rather than at the
			// first read.
			name: "a token but no endpoint",
			cfg: func(dir string) Config {
				return Config{Name: "test", Token: "tok", DataDir: filepath.Join(dir, "data")}
			},
			wantErr: "no Incus endpoint",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			conn, err := Connect(t.Context(), tc.cfg(t.TempDir()))
			require.Error(t, err)
			assert.Nil(t, conn)

			if tc.wantIs != nil {
				require.ErrorIs(t, err, tc.wantIs)

				return
			}

			assert.Contains(t, err.Error(), tc.wantErr)
		})
	}
}

// TestConnectDoesNotReachTheRemoteUnlessAsked pins the guard on the developer
// path: a lost certificate fails rather than using a home directory's identity.
func TestConnectDoesNotReachTheRemoteUnlessAsked(t *testing.T) {
	dir := t.TempDir()

	_, err := Connect(t.Context(), Config{
		Name:       "test",
		DataDir:    filepath.Join(dir, "data"),
		SecretsDir: dir,
	})
	require.ErrorIs(t, err, ErrNoCredentials)
}

// TestTrusted pins what counts as "the daemon already has this certificate". It
// is decided by message, so a wrong answer persists a certificate nothing trusts.
func TestTrusted(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{name: "already trusted", err: errString("Certificate already trusted"), want: true},
		{name: "already exists", err: errString("record already exists"), want: true},
		{name: "a real refusal", err: errString("Invalid trust token"), want: false},
		{name: "an endpoint that is not there", err: errString("connection refused"), want: false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, trusted(tc.err))
		})
	}
}

type errString string

func (e errString) Error() string { return string(e) }

// TestGenerate pins that the generated pair is one Incus will take.
func TestGenerate(t *testing.T) {
	certPEM, keyPEM, err := generate("ic-dns")
	require.NoError(t, err)

	assert.Contains(t, string(certPEM), "BEGIN CERTIFICATE")
	assert.Contains(t, string(keyPEM), "BEGIN PRIVATE KEY")

	// Written and read back through the same paths the daemon uses.
	dir := t.TempDir()
	require.NoError(t, writePair(dir, certPEM, keyPEM))

	gotCert, gotKey, err := readPair(filepath.Join(dir, CertFile), filepath.Join(dir, KeyFile))
	require.NoError(t, err)
	assert.Equal(t, certPEM, gotCert)
	assert.Equal(t, keyPEM, gotKey)

	// The key is not world-readable, which a wrong umask would quietly change.
	info, err := os.Stat(filepath.Join(dir, KeyFile))
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
}

// TestConnectIgnoresAnEmptyDataDir pins that no directory means no enrolled
// pair: filepath.Join("", CertFile) is relative, and would read the cwd's.
func TestConnectIgnoresAnEmptyDataDir(t *testing.T) {
	dir := t.TempDir()

	// A pair in the working directory, under the names Connect looks for.
	t.Chdir(dir)
	_, _ = pair(t, dir, CertFile, KeyFile)

	_, err := Connect(t.Context(), Config{
		Name: "test",
		URL:  "https://10.0.0.1:8443",
	})
	require.ErrorIs(t, err, ErrNoCredentials)
}
