package iclient

import (
	"net/http"
	"os"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestIncusServerRequestURLs(t *testing.T) {
	t.Parallel()

	ctx := t.Context()

	t.Run("GetServer", func(t *testing.T) {
		t.Parallel()

		conn, seen := recordingServer(t, `{"api_extensions":["instance_get_full"]}`)

		server, etag, err := conn.GetServer(ctx)
		require.NoError(t, err)
		require.Equal(t, []string{"instance_get_full"}, server.APIExtensions)
		require.Equal(t, "test-etag", etag)

		// The root of the API, which is server-level and carries no project.
		require.Equal(t, []string{"/1.0"}, seen.uris())
	})

	t.Run("RawQuery is verbatim", func(t *testing.T) {
		t.Parallel()

		conn, seen := recordingServer(t, `{}`)

		_, _, err := conn.RawQuery(ctx, http.MethodPatch, "/1.0/instances/web-1?project=other", nil, "")
		require.NoError(t, err)

		// No second project parameter and no doubled prefix.
		require.Equal(t, []string{"/1.0/instances/web-1?project=other"}, seen.uris())
	})

	t.Run("GetClusterMembers", func(t *testing.T) {
		t.Parallel()

		conn, seen := recordingServer(t, `[{"server_name":"node1","status":"Online"},{"server_name":"node2","status":"Online"}]`)

		members, err := conn.GetClusterMembers(ctx)
		require.NoError(t, err)
		require.Len(t, members, 2)
		require.Equal(t, "node1", members[0].ServerName)
		require.Equal(t, "node2", members[1].ServerName)
		require.Equal(t, []string{"/1.0/cluster/members?recursion=1"}, seen.uris())
	})
}

func TestIncusHasExtension(t *testing.T) {
	t.Parallel()

	conn, _ := recordingServer(t, `{"api_extensions":["instance_get_full","storage"]}`)

	has, err := conn.HasExtension(t.Context(), "storage")
	require.NoError(t, err)
	require.True(t, has)

	has, err = conn.HasExtension(t.Context(), "no_such_extension")
	require.NoError(t, err)
	require.False(t, has)
}

func TestIncusIsIncusOS(t *testing.T) {
	t.Parallel()

	t.Run("IncusOS answers through the proxy", func(t *testing.T) {
		t.Parallel()

		conn, seen := recordingServer(t, `{}`)

		incusOS, err := conn.IsIncusOS(t.Context())
		require.NoError(t, err)
		require.True(t, incusOS)
		require.Equal(t, []string{"/os/1.0"}, seen.uris())
	})

	t.Run("refused by incusd", func(t *testing.T) {
		t.Parallel()

		conn := rawServer(t, http.StatusBadRequest, `{"type":"error","error_code":400,"error":"System isn't running IncusOS"}`)

		incusOS, err := conn.IsIncusOS(t.Context())
		require.NoError(t, err)
		require.False(t, incusOS)
	})

	t.Run("server without the proxy", func(t *testing.T) {
		t.Parallel()

		conn := rawServer(t, http.StatusNotFound, `{"type":"error","error_code":404,"error":"not found"}`)

		incusOS, err := conn.IsIncusOS(t.Context())
		require.NoError(t, err)
		require.False(t, incusOS)
	})

	t.Run("forbidden", func(t *testing.T) {
		t.Parallel()

		conn := rawServer(t, http.StatusForbidden, `{"type":"error","error_code":403,"error":"not authorized"}`)

		_, err := conn.IsIncusOS(t.Context())
		require.Error(t, err)
	})
}

func TestIncusGetConnectionInfo(t *testing.T) {
	t.Parallel()

	conn, _ := recordingServer(t, `{"environment":{"server_name":"node1","certificate_fingerprint":"a1b2c3d4","addresses":["10.0.0.5:8443",":8443"]}}`)

	info, err := conn.GetConnectionInfo(t.Context())
	require.NoError(t, err)

	require.Equal(t, "incus", info.Protocol)
	require.Equal(t, "node1", info.Target)
	require.Equal(t, "a1b2c3d4", info.CertificateFingerprint)
	require.Empty(t, info.SocketPath, "an http remote has no socket")

	// A wildcard address names no host, so it is dropped.
	require.Contains(t, info.Addresses, "https://10.0.0.5:8443")
	require.NotContains(t, info.Addresses, "https://:8443")
}

func TestIncusServerAgainstRealIncus(t *testing.T) {
	skipLocal(t)
	t.Parallel()

	ctx := t.Context()
	conn := testConnection(t)

	server, _, err := conn.GetServer(ctx)
	require.NoError(t, err)
	require.NotEmpty(t, server.APIExtensions, "a real server always advertises extensions")

	// Pinned against the server we are actually talking to.
	has, err := conn.HasExtension(ctx, "instance_get_full")
	require.NoError(t, err)
	require.True(t, has)

	has, err = conn.HasExtension(ctx, "ic-no-such-extension")
	require.NoError(t, err)
	require.False(t, has)

	info, err := conn.GetConnectionInfo(ctx)
	require.NoError(t, err)
	require.Equal(t, "incus", info.Protocol)
	require.NotEmpty(t, info.Target)

	if os.Getenv("INCUS_REMOTE") != "" {
		require.NotEmpty(t, info.URL)
	}
}
