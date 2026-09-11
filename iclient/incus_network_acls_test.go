package iclient

import (
	"net/http"
	"testing"

	"github.com/lxc/incus/v7/shared/api"
	"github.com/stretchr/testify/require"
)

func TestIncusNetworkACLRequests(t *testing.T) {
	t.Parallel()

	ctx := t.Context()

	t.Run("GetNetworkACL", func(t *testing.T) {
		t.Parallel()

		conn, seen := recordingServer(t, `{"name":"web"}`)

		acl, etag, err := conn.GetNetworkACL(ctx, "myproject", "web")
		require.NoError(t, err)
		require.Equal(t, "web", acl.Name)
		require.Equal(t, "test-etag", etag, "GetNetworkACL must return the ETag header")
		require.Equal(t, []string{"/1.0/network-acls/web?project=myproject"}, seen.uris())
	})

	t.Run("GetNetworkACLNames", func(t *testing.T) {
		t.Parallel()

		conn, seen := recordingServer(t, `["/1.0/network-acls/web","/1.0/network-acls/db"]`)

		names, err := conn.GetNetworkACLNames(ctx, "myproject")
		require.NoError(t, err)
		require.Equal(t, []string{"web", "db"}, names)
		require.Equal(t, []string{"/1.0/network-acls?project=myproject"}, seen.uris())
	})

	t.Run("CreateNetworkACL", func(t *testing.T) {
		t.Parallel()

		conn, seen := recordingServer(t, `{}`)

		acl := api.NetworkACLsPost{NetworkACLPost: api.NetworkACLPost{Name: "web"}}
		require.NoError(t, conn.CreateNetworkACL(ctx, "myproject", acl))

		req := seen.all()[0]
		require.Equal(t, http.MethodPost, req.method)
		require.Equal(t, "/1.0/network-acls?project=myproject", req.uri())
		require.Contains(t, req.body, `"name":"web"`)
	})

	t.Run("UpdateNetworkACL sends the ETag", func(t *testing.T) {
		t.Parallel()

		conn, seen := recordingServer(t, `{}`)

		err := conn.UpdateNetworkACL(ctx, "myproject", "web", api.NetworkACLPut{Description: "d"}, "the-etag")
		require.NoError(t, err)

		req := seen.all()[0]
		require.Equal(t, http.MethodPut, req.method)
		require.Equal(t, "/1.0/network-acls/web?project=myproject", req.uri())
		require.Equal(t, "the-etag", req.etag, "a conditional update must carry If-Match")
	})

	t.Run("DeleteNetworkACL", func(t *testing.T) {
		t.Parallel()

		conn, seen := recordingServer(t, `{}`)

		require.NoError(t, conn.DeleteNetworkACL(ctx, "myproject", "web"))
		require.Equal(t, []string{"/1.0/network-acls/web?project=myproject"}, seen.uris())
	})
}

func TestIncusNetworkPeerRequests(t *testing.T) {
	t.Parallel()

	ctx := t.Context()

	t.Run("GetNetworkPeer", func(t *testing.T) {
		t.Parallel()

		conn, seen := recordingServer(t, `{"name":"dns"}`)

		peer, etag, err := conn.GetNetworkPeer(ctx, "myproject", "br0", "dns")
		require.NoError(t, err)
		require.Equal(t, "dns", peer.Name)
		require.Equal(t, "test-etag", etag, "GetNetworkPeer must return the ETag header")
		require.Equal(t, []string{"/1.0/networks/br0/peers/dns?project=myproject"}, seen.uris())
	})

	t.Run("GetNetworkPeers", func(t *testing.T) {
		t.Parallel()

		conn, seen := recordingServer(t, `[{"name":"dns","target_network":"icompose0"}]`)

		peers, err := conn.GetNetworkPeers(ctx, "myproject", "br0")
		require.NoError(t, err)
		require.Len(t, peers, 1)
		require.Equal(t, "icompose0", peers[0].TargetNetwork)
		require.Equal(t, []string{"/1.0/networks/br0/peers?project=myproject&recursion=1"}, seen.uris())
	})

	t.Run("CreateNetworkPeer", func(t *testing.T) {
		t.Parallel()

		conn, seen := recordingServer(t, `{}`)

		peer := api.NetworkPeersPost{Name: "dns", TargetProject: "incus-compose", TargetNetwork: "icompose0"}
		require.NoError(t, conn.CreateNetworkPeer(ctx, "myproject", "br0", peer))

		req := seen.all()[0]
		require.Equal(t, http.MethodPost, req.method)
		require.Equal(t, "/1.0/networks/br0/peers?project=myproject", req.uri())
		require.Contains(t, req.body, `"target_network":"icompose0"`)
	})

	t.Run("DeleteNetworkPeer", func(t *testing.T) {
		t.Parallel()

		conn, seen := recordingServer(t, `{}`)

		require.NoError(t, conn.DeleteNetworkPeer(ctx, "myproject", "br0", "dns"))
		require.Equal(t, []string{"/1.0/networks/br0/peers/dns?project=myproject"}, seen.uris())
	})
}
