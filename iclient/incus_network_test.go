package iclient

import (
	"net/http"
	"testing"

	"github.com/lxc/incus/v7/shared/api"
	"github.com/stretchr/testify/require"

	"github.com/lxc/incus-compose/testlib"
)

func TestIncusNetworkRequests(t *testing.T) {
	t.Parallel()

	ctx := t.Context()

	t.Run("GetNetwork", func(t *testing.T) {
		t.Parallel()

		conn, seen := recordingServer(t, `{"name":"br0"}`)

		network, etag, err := conn.GetNetwork(ctx, "br0")
		require.NoError(t, err)
		require.Equal(t, "br0", network.Name)
		require.Equal(t, "test-etag", etag, "GetNetwork must return the ETag header")

		req := seen.all()[0]
		require.Equal(t, http.MethodGet, req.method)
		require.Equal(t, "/1.0/networks/br0?project=myproject", req.uri())
	})

	t.Run("GetNetworkNames", func(t *testing.T) {
		t.Parallel()

		conn, seen := recordingServer(t, `["/1.0/networks/br0","/1.0/networks/ic-abc"]`)

		names, err := conn.GetNetworkNames(ctx)
		require.NoError(t, err)
		require.Equal(t, []string{"br0", "ic-abc"}, names)
		require.Equal(t, []string{"/1.0/networks?project=myproject"}, seen.uris())
	})

	t.Run("GetNetworks", func(t *testing.T) {
		t.Parallel()

		conn, seen := recordingServer(t, `[{"name":"br0","project":"default","managed":true}]`)

		networks, err := conn.GetNetworks(ctx)
		require.NoError(t, err)
		require.Len(t, networks, 1)
		require.Equal(t, "br0", networks[0].Name)
		require.Equal(t, "default", networks[0].Project, "the owning project comes back with the network")
		require.True(t, networks[0].Managed)
		require.Equal(t, []string{"/1.0/networks?project=myproject&recursion=1"}, seen.uris())
	})

	t.Run("CreateNetwork", func(t *testing.T) {
		t.Parallel()

		conn, seen := recordingServer(t, `{}`)

		require.NoError(t, conn.CreateNetwork(ctx, api.NetworksPost{Name: "br0"}))

		req := seen.all()[0]
		require.Equal(t, http.MethodPost, req.method)
		require.Equal(t, "/1.0/networks?project=myproject", req.uri())
		require.Contains(t, req.body, `"name":"br0"`)
	})

	t.Run("UpdateNetwork sends the ETag", func(t *testing.T) {
		t.Parallel()

		conn, seen := recordingServer(t, `{}`)

		err := conn.UpdateNetwork(ctx, "br0", api.NetworkPut{Description: "d"}, "the-etag")
		require.NoError(t, err)

		req := seen.all()[0]
		require.Equal(t, http.MethodPut, req.method)
		require.Equal(t, "/1.0/networks/br0?project=myproject", req.uri())
		require.Equal(t, "the-etag", req.etag, "a conditional update must carry If-Match")
	})

	t.Run("DeleteNetwork", func(t *testing.T) {
		t.Parallel()

		conn, seen := recordingServer(t, `{}`)

		require.NoError(t, conn.DeleteNetwork(ctx, "br0"))

		req := seen.all()[0]
		require.Equal(t, http.MethodDelete, req.method)
		require.Equal(t, "/1.0/networks/br0?project=myproject", req.uri())
	})

	t.Run("name is escaped", func(t *testing.T) {
		t.Parallel()

		conn, seen := recordingServer(t, `{}`)

		_, _, err := conn.GetNetwork(ctx, "a/b")
		require.NoError(t, err)
		require.Equal(t, []string{"/1.0/networks/a%2Fb?project=myproject"}, seen.uris())
	})
}

func TestIncusNetworkAgainstRealIncus(t *testing.T) {
	testlib.SkipLocal(t)
	t.Parallel()

	ctx := t.Context()
	conn := testConnection(t)

	names, err := conn.GetNetworkNames(ctx)
	require.NoError(t, err)

	for _, name := range names {
		require.NotContains(t, name, "/", "GetNetworkNames must strip the URL prefix")
	}

	if len(names) == 0 {
		t.Skip("no network on the remote to read")
	}

	network, _, err := conn.GetNetwork(ctx, names[0])
	require.NoError(t, err)
	require.Equal(t, names[0], network.Name)

	_, _, err = conn.GetNetwork(ctx, "ic-iclient-no-such-network")
	require.True(t, api.StatusErrorCheck(err, 404), "want a 404, got %v", err)
}
