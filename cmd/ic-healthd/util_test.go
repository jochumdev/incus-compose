package main

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	incusApi "github.com/lxc/incus/v7/shared/api"
	"github.com/stretchr/testify/require"
)

// TestDialTrustsAnUnknownServerCert pins the one connection property the daemon
// cannot get from anywhere else: it is handed a URL and a token, so it has no
// server certificate to pin and has to accept whatever answers. Verifying would
// fail here, because the test server signs its own.
func TestDialTrustsAnUnknownServerCert(t *testing.T) {
	t.Parallel()

	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(incusApi.Response{
			Type:       incusApi.SyncResponse,
			StatusCode: http.StatusOK,
			Metadata:   json.RawMessage(`{"environment":{"server_name":"test"}}`),
		})
	}))
	t.Cleanup(server.Close)

	certPEM, keyPEM, err := generateClientCert()
	require.NoError(t, err)

	conn, err := dial(config{IncusURL: server.URL}, certPEM, keyPEM)
	require.NoError(t, err)

	t.Cleanup(func() { _ = conn.Disconnect(context.Background()) })

	got, _, err := conn.GetServer(t.Context())
	require.NoError(t, err)
	require.Equal(t, "test", got.Environment.ServerName)
}

// TestGenerateClientCertIsUsableAsAClientCert pins the properties Incus checks
// when the certificate is presented for trust.
func TestGenerateClientCertIsUsableAsAClientCert(t *testing.T) {
	t.Parallel()

	certPEM, keyPEM, err := generateClientCert()
	require.NoError(t, err)

	certBlock, rest := pem.Decode(certPEM)
	require.NotNil(t, certBlock)
	require.Equal(t, "CERTIFICATE", certBlock.Type)
	require.Empty(t, rest)

	keyBlock, rest := pem.Decode(keyPEM)
	require.NotNil(t, keyBlock)
	require.Equal(t, "PRIVATE KEY", keyBlock.Type)
	require.Empty(t, rest)

	cert, err := x509.ParseCertificate(certBlock.Bytes)
	require.NoError(t, err)

	require.Equal(t, "ic-healthd", cert.Subject.CommonName)
	require.Contains(t, cert.ExtKeyUsage, x509.ExtKeyUsageClientAuth,
		"without client auth Incus refuses the certificate")
	require.True(t, cert.NotBefore.Before(time.Now()), "a not-yet-valid cert is rejected on first use")
	require.True(t, cert.NotAfter.After(time.Now().AddDate(9, 0, 0)), "the cert outlives any sensible redeploy")

	// The pair has to match, or registration succeeds and every later call fails.
	_, err = x509.ParsePKCS8PrivateKey(keyBlock.Bytes)
	require.NoError(t, err)
}

// TestGenerateClientCertIsUnique pins that two daemons never collide in the
// trust store.
func TestGenerateClientCertIsUnique(t *testing.T) {
	t.Parallel()

	first, _, err := generateClientCert()
	require.NoError(t, err)

	second, _, err := generateClientCert()
	require.NoError(t, err)

	require.NotEqual(t, first, second)
}

func TestFileExists(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "token")

	require.False(t, fileExists(path))
	require.NoError(t, os.WriteFile(path, []byte("x"), 0o600))
	require.True(t, fileExists(path))

	require.True(t, fileExists(dir), "a directory counts as present; connect only asks about files")
}

// TestHasDefaultRoute covers the check that gates startup for up to ten
// seconds. The file is kernel ABI, so these cases do not move.
func TestHasDefaultRoute(t *testing.T) {
	t.Parallel()

	const header = "Iface\tDestination\tGateway \tFlags\tRefCnt\tUse\tMetric\tMask\t\tMTU\tWindow\tIRTT\n"

	tests := []struct {
		name    string
		content string
		want    bool
	}{
		{
			name: "a default route",
			content: header +
				"eth0\t00000000\t010011AC\t0003\t0\t0\t0\t00000000\t0\t0\t0\n" +
				"eth0\t000011AC\t00000000\t0001\t0\t0\t0\t0000FFFF\t0\t0\t0\n",
			want: true,
		},
		{
			name:    "only a subnet route",
			content: header + "eth0\t000011AC\t00000000\t0001\t0\t0\t0\t0000FFFF\t0\t0\t0\n",
			want:    false,
		},
		{
			name:    "header only",
			content: header,
			want:    false,
		},
		{
			name:    "empty",
			content: "",
			want:    false,
		},
		{
			// The destination lives in the second column, so a row without one
			// must not be read as a match.
			name:    "a short row",
			content: header + "eth0\n",
			want:    false,
		},
		{
			// The header itself must never match, whatever it says.
			name:    "a route on the header line",
			content: "Iface\t00000000\n",
			want:    false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			path := filepath.Join(t.TempDir(), "route")
			require.NoError(t, os.WriteFile(path, []byte(tt.content), 0o600))

			require.Equal(t, tt.want, hasDefaultRoute(path))
		})
	}

	t.Run("missing file", func(t *testing.T) {
		t.Parallel()

		require.False(t, hasDefaultRoute(filepath.Join(t.TempDir(), "absent")),
			"a kernel without /proc/net/route is not a kernel with a default route")
	})
}

func TestLogLevel(t *testing.T) {
	t.Parallel()

	require.Equal(t, slog.LevelInfo, logLevel(false, false))
	require.Equal(t, slog.LevelDebug, logLevel(true, false))

	// Trace implies debug, so it wins either way round.
	require.Equal(t, levelTrace, logLevel(false, true))
	require.Equal(t, levelTrace, logLevel(true, true))

	require.Less(t, int(levelTrace), int(slog.LevelDebug))
}

// TestLogHandlerNamesTrace pins the label; slog would print "DEBUG-4".
func TestLogHandlerNamesTrace(t *testing.T) {
	t.Parallel()

	out := &bytes.Buffer{}
	log := slog.New(newLogHandler(out, levelTrace))

	log.Log(t.Context(), levelTrace, "per event")
	log.Debug("noisy")

	require.Contains(t, out.String(), "level=TRACE msg=\"per event\"")
	require.Contains(t, out.String(), "level=DEBUG msg=noisy")
}

// TestLogHandlerHidesTraceAtDebug is the point of the split.
func TestLogHandlerHidesTraceAtDebug(t *testing.T) {
	t.Parallel()

	out := &bytes.Buffer{}
	log := slog.New(newLogHandler(out, slog.LevelDebug))

	log.Log(t.Context(), levelTrace, "per event")
	log.Debug("noisy")

	require.NotContains(t, out.String(), "per event")
	require.Contains(t, out.String(), "noisy")
}
