package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func testGenerateCertKey(t *testing.T) (string, string) {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	require.NoError(t, err)

	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	require.NoError(t, err)

	now := time.Now()
	template := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "test-client"},
		NotBefore:             now.Add(-time.Minute),
		NotAfter:              now.AddDate(1, 0, 0),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}

	certDER, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	require.NoError(t, err)

	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	require.NoError(t, err)

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})

	return string(certPEM), string(keyPEM)
}

func TestDialManual(t *testing.T) {
	t.Parallel()

	certPEM, keyPEM := testGenerateCertKey(t)
	validFP := "a1b2c3d4e5f60718293a4b5c6d7e8f90a1b2c3d4e5f60718293a4b5c6d7e8f90"

	t.Run("missing server fingerprint errors", func(t *testing.T) {
		t.Parallel()

		conn, err := dialManual("https://192.168.1.10:8443", certPEM, keyPEM, "")
		require.Error(t, err)
		require.Nil(t, conn)
		require.Contains(t, err.Error(), "server fingerprint is required")
	})

	t.Run("valid credentials with server fingerprint", func(t *testing.T) {
		t.Parallel()

		conn, err := dialManual("https://192.168.1.10:8443", certPEM, keyPEM, validFP)
		require.NoError(t, err)
		require.NotNil(t, conn)
	})

	t.Run("invalid client cert PEM", func(t *testing.T) {
		t.Parallel()

		conn, err := dialManual("https://192.168.1.10:8443", "invalid-pem", keyPEM, validFP)
		require.Error(t, err)
		require.Nil(t, conn)
	})

	t.Run("invalid server fingerprint hex", func(t *testing.T) {
		t.Parallel()

		conn, err := dialManual("https://192.168.1.10:8443", certPEM, keyPEM, "not-valid-hex")
		require.Error(t, err)
		require.Nil(t, conn)
		require.Contains(t, err.Error(), "invalid server certificate fingerprint")
	})

	t.Run("invalid server fingerprint length", func(t *testing.T) {
		t.Parallel()

		conn, err := dialManual("https://192.168.1.10:8443", certPEM, keyPEM, "abcd")
		require.Error(t, err)
		require.Nil(t, conn)
		require.Contains(t, err.Error(), "invalid server certificate fingerprint")
	})

	t.Run("empty address", func(t *testing.T) {
		t.Parallel()

		conn, err := dialManual("", certPEM, keyPEM, validFP)
		require.Error(t, err)
		require.Nil(t, conn)
		require.Contains(t, err.Error(), "server URL cannot be empty")
	})
}

func TestDialManualFiles(t *testing.T) {
	t.Parallel()

	certPEM, keyPEM := testGenerateCertKey(t)
	validFP := "a1b2c3d4e5f60718293a4b5c6d7e8f90a1b2c3d4e5f60718293a4b5c6d7e8f90"

	dir := t.TempDir()
	certFile := filepath.Join(dir, "client.crt")
	keyFile := filepath.Join(dir, "client.key")

	err := os.WriteFile(certFile, []byte(certPEM), 0o600)
	require.NoError(t, err)

	err = os.WriteFile(keyFile, []byte(keyPEM), 0o600)
	require.NoError(t, err)

	t.Run("valid files with server fingerprint", func(t *testing.T) {
		t.Parallel()

		conn, err := dialManualFiles("https://192.168.1.10:8443", certFile, keyFile, validFP)
		require.NoError(t, err)
		require.NotNil(t, conn)
	})

	t.Run("missing server fingerprint", func(t *testing.T) {
		t.Parallel()

		conn, err := dialManualFiles("https://192.168.1.10:8443", certFile, keyFile, "")
		require.Error(t, err)
		require.Nil(t, conn)
		require.Contains(t, err.Error(), "server fingerprint is required")
	})

	t.Run("missing client cert argument", func(t *testing.T) {
		t.Parallel()

		conn, err := dialManualFiles("https://192.168.1.10:8443", "", keyFile, validFP)
		require.Error(t, err)
		require.Nil(t, conn)
		require.Contains(t, err.Error(), "both client certificate and key files are required")
	})

	t.Run("missing client key argument", func(t *testing.T) {
		t.Parallel()

		conn, err := dialManualFiles("https://192.168.1.10:8443", certFile, "", validFP)
		require.Error(t, err)
		require.Nil(t, conn)
		require.Contains(t, err.Error(), "both client certificate and key files are required")
	})

	t.Run("non-existent client cert file", func(t *testing.T) {
		t.Parallel()

		conn, err := dialManualFiles("https://192.168.1.10:8443", filepath.Join(dir, "missing.crt"), keyFile, validFP)
		require.Error(t, err)
		require.Nil(t, conn)
		require.Contains(t, err.Error(), "reading client certificate")
	})

	t.Run("non-existent client key file", func(t *testing.T) {
		t.Parallel()

		conn, err := dialManualFiles("https://192.168.1.10:8443", certFile, filepath.Join(dir, "missing.key"), validFP)
		require.Error(t, err)
		require.Nil(t, conn)
		require.Contains(t, err.Error(), "reading client key")
	})
}

func TestDialManualTLSFingerprintVerification(t *testing.T) {
	t.Parallel()

	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintln(w, `{"type":"sync","status":"Success","status_code":200,"metadata":{"api_version":"1.0"}}`)
	}))
	defer ts.Close()

	serverCert := ts.Certificate()
	expectedFP := fmt.Sprintf("%x", sha256.Sum256(serverCert.Raw))
	certPEM, keyPEM := testGenerateCertKey(t)

	t.Run("matching fingerprint connects successfully", func(t *testing.T) {
		conn, err := dialManual(ts.URL, certPEM, keyPEM, expectedFP)
		require.NoError(t, err)
		require.NotNil(t, conn)

		server, _, err := conn.GetServer(context.Background())
		require.NoError(t, err)
		require.NotNil(t, server)
	})

	t.Run("matching fingerprint with colons and uppercase connects successfully", func(t *testing.T) {
		var colonFP strings.Builder
		for i := 0; i < len(expectedFP); i += 2 {
			if i > 0 {
				colonFP.WriteString(":")
			}
			colonFP.WriteString(strings.ToUpper(expectedFP[i : i+2]))
		}

		conn, err := dialManual(ts.URL, certPEM, keyPEM, colonFP.String())
		require.NoError(t, err)
		require.NotNil(t, conn)

		server, _, err := conn.GetServer(context.Background())
		require.NoError(t, err)
		require.NotNil(t, server)
	})

	t.Run("mismatched fingerprint fails handshake", func(t *testing.T) {
		mismatchedFP := strings.Repeat("0", 64)
		conn, err := dialManual(ts.URL, certPEM, keyPEM, mismatchedFP)
		require.NoError(t, err)
		require.NotNil(t, conn)

		_, _, err = conn.GetServer(context.Background())
		require.Error(t, err)
		require.Contains(t, err.Error(), "server certificate fingerprint mismatch")
	})
}

func TestManualConnectionFlags(t *testing.T) {
	dir := t.TempDir()
	certFile := filepath.Join(dir, "client.crt")
	keyFile := filepath.Join(dir, "client.key")

	err := os.WriteFile(certFile, []byte("dummy cert"), 0o600)
	require.NoError(t, err)

	err = os.WriteFile(keyFile, []byte("dummy key"), 0o600)
	require.NoError(t, err)

	t.Run("missing client cert and key files", func(t *testing.T) {
		cmd := newRootCommand()
		cmd.Writer = io.Discard
		cmd.ErrWriter = io.Discard

		err := cmd.Run(context.Background(), []string{
			"incus-compose",
			"--server-url=https://127.0.0.1:8443",
			"ps",
		})
		require.Error(t, err)
		require.Contains(t, err.Error(), "both client certificate and key files are required")
	})

	t.Run("missing key file only", func(t *testing.T) {
		cmd := newRootCommand()
		cmd.Writer = io.Discard
		cmd.ErrWriter = io.Discard

		err := cmd.Run(context.Background(), []string{
			"incus-compose",
			"--server-url=https://127.0.0.1:8443",
			"--client-cert-file=" + certFile,
			"ps",
		})
		require.Error(t, err)
		require.Contains(t, err.Error(), "both client certificate and key files are required")
	})

	t.Run("missing server fingerprint flag", func(t *testing.T) {
		cmd := newRootCommand()
		cmd.Writer = io.Discard
		cmd.ErrWriter = io.Discard

		err := cmd.Run(context.Background(), []string{
			"incus-compose",
			"--server-url=https://127.0.0.1:8443",
			"--client-cert-file=" + certFile,
			"--client-key-file=" + keyFile,
			"ps",
		})
		require.Error(t, err)
		require.Contains(t, err.Error(), "server fingerprint is required")
	})

	t.Run("non-existent client cert file", func(t *testing.T) {
		cmd := newRootCommand()
		cmd.Writer = io.Discard
		cmd.ErrWriter = io.Discard

		err := cmd.Run(context.Background(), []string{
			"incus-compose",
			"--server-url=https://127.0.0.1:8443",
			"--client-cert-file=" + filepath.Join(dir, "non-existent.crt"),
			"--client-key-file=" + keyFile,
			"--server-fingerprint=a1b2c3d4e5f60718293a4b5c6d7e8f90a1b2c3d4e5f60718293a4b5c6d7e8f90",
			"ps",
		})
		require.Error(t, err)
		require.Contains(t, err.Error(), "reading client certificate")
	})

	t.Run("non-existent client key file", func(t *testing.T) {
		cmd := newRootCommand()
		cmd.Writer = io.Discard
		cmd.ErrWriter = io.Discard

		err := cmd.Run(context.Background(), []string{
			"incus-compose",
			"--server-url=https://127.0.0.1:8443",
			"--client-cert-file=" + certFile,
			"--client-key-file=" + filepath.Join(dir, "non-existent.key"),
			"--server-fingerprint=a1b2c3d4e5f60718293a4b5c6d7e8f90a1b2c3d4e5f60718293a4b5c6d7e8f90",
			"ps",
		})
		require.Error(t, err)
		require.Contains(t, err.Error(), "reading client key")
	})

	t.Run("invalid server fingerprint flag", func(t *testing.T) {
		cmd := newRootCommand()
		cmd.Writer = io.Discard
		cmd.ErrWriter = io.Discard

		err := cmd.Run(context.Background(), []string{
			"incus-compose",
			"--server-url=https://127.0.0.1:8443",
			"--client-cert-file=" + certFile,
			"--client-key-file=" + keyFile,
			"--server-fingerprint=invalid-hex",
			"ps",
		})
		require.Error(t, err)
		require.Contains(t, err.Error(), "invalid server certificate fingerprint")
	})

	t.Run("explicit remote flag takes precedence over INCUS_SERVER_URL", func(t *testing.T) {
		t.Setenv("INCUS_SERVER_URL", "https://127.0.0.1:8443")

		cmd := newRootCommand()
		cmd.Writer = io.Discard
		cmd.ErrWriter = io.Discard

		// When --remote is explicitly passed, it should NOT attempt manual connection (which would fail on missing certs).
		// Instead, it dials the remote. With a non-existent remote name, it fails with "remote not found".
		err := cmd.Run(context.Background(), []string{
			"incus-compose",
			"--remote=non-existent-remote-xyz",
			"ps",
		})
		require.Error(t, err)
		require.Contains(t, err.Error(), `remote not found`)
	})

	t.Run("explicit remote flag takes precedence over --server-url CLI flag", func(t *testing.T) {
		cmd := newRootCommand()
		cmd.Writer = io.Discard
		cmd.ErrWriter = io.Discard

		err := cmd.Run(context.Background(), []string{
			"incus-compose",
			"--remote=non-existent-remote-xyz",
			"--server-url=https://127.0.0.1:8443",
			"ps",
		})
		require.Error(t, err)
		require.Contains(t, err.Error(), `remote not found`)
	})

	t.Run("INCUS_SERVER_URL takes precedence over INCUS_REMOTE", func(t *testing.T) {
		t.Setenv("INCUS_REMOTE", "non-existent-remote-xyz")
		t.Setenv("INCUS_SERVER_URL", "https://127.0.0.1:8443")

		cmd := newRootCommand()
		cmd.Writer = io.Discard
		cmd.ErrWriter = io.Discard

		// Because --remote is NOT explicitly passed on the CLI, INCUS_SERVER_URL triggers manual connection.
		// Without cert files, it fails with the manual connection error, not "remote not found".
		err := cmd.Run(context.Background(), []string{
			"incus-compose",
			"ps",
		})
		require.Error(t, err)
		require.Contains(t, err.Error(), "both client certificate and key files are required")
	})

	t.Run("env vars for client cert, key, and fingerprint", func(t *testing.T) {
		t.Setenv("INCUS_SERVER_URL", "https://127.0.0.1:8443")
		t.Setenv("INCUS_CLIENT_CERT_FILE", filepath.Join(dir, "missing-cert-from-env.crt"))
		t.Setenv("INCUS_CLIENT_KEY_FILE", keyFile)
		t.Setenv("INCUS_SERVER_FINGERPRINT", "invalid-fp")

		cmd := newRootCommand()
		cmd.Writer = io.Discard
		cmd.ErrWriter = io.Discard

		err := cmd.Run(context.Background(), []string{
			"incus-compose",
			"ps",
		})
		require.Error(t, err)
		require.Contains(t, err.Error(), "reading client certificate")
	})

	t.Run("missing INCUS_SERVER_FINGERPRINT env var", func(t *testing.T) {
		t.Setenv("INCUS_SERVER_URL", "https://127.0.0.1:8443")
		t.Setenv("INCUS_CLIENT_CERT_FILE", certFile)
		t.Setenv("INCUS_CLIENT_KEY_FILE", keyFile)

		cmd := newRootCommand()
		cmd.Writer = io.Discard
		cmd.ErrWriter = io.Discard

		err := cmd.Run(context.Background(), []string{
			"incus-compose",
			"ps",
		})
		require.Error(t, err)
		require.Contains(t, err.Error(), "server fingerprint is required")
	})
}
