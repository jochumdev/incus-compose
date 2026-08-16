package iclient

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/lxc/incus/v7/shared/api"
	"github.com/stretchr/testify/require"

	"github.com/lxc/incus-compose/internal/testlib"
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

		// The root of the API, with the project still applied.
		require.Equal(t, []string{"/1.0?project=myproject"}, seen.uris())
	})

	t.Run("RawQuery is verbatim", func(t *testing.T) {
		t.Parallel()

		conn, seen := recordingServer(t, `{}`)

		_, _, err := conn.RawQuery(ctx, http.MethodPatch, "/1.0/instances/web-1?project=other", nil, "")
		require.NoError(t, err)

		// No second project parameter and no doubled prefix.
		require.Equal(t, []string{"/1.0/instances/web-1?project=other"}, seen.uris())
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

func TestIncusGetConnectionInfo(t *testing.T) {
	t.Parallel()

	conn, _ := recordingServer(t, `{"environment":{"server_name":"node1","addresses":["10.0.0.5:8443",":8443"]}}`)

	info, err := conn.GetConnectionInfo(t.Context())
	require.NoError(t, err)

	require.Equal(t, "incus", info.Protocol)
	require.Equal(t, "myproject", info.Project)
	require.Equal(t, "node1", info.Target)
	require.Empty(t, info.SocketPath, "an http remote has no socket")

	// A wildcard address names no host, so it is dropped.
	require.Contains(t, info.Addresses, "https://10.0.0.5:8443")
	require.NotContains(t, info.Addresses, "https://:8443")
}

func TestIncusGetConnectionInfoDefaultProject(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"type":"sync","status_code":200,"metadata":{}}`)
	}))
	t.Cleanup(server.Close)

	conn, err := NewConnection(&ConfigRemoteInfo{Name: "noproject", Addrs: []string{server.URL}})
	require.NoError(t, err)

	info, err := conn.GetConnectionInfo(t.Context())
	require.NoError(t, err)
	require.Equal(t, api.ProjectDefaultName, info.Project)
}

func TestIncusServerAgainstRealIncus(t *testing.T) {
	testlib.SkipLocal(t)
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
