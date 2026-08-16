package iclient

import (
	"net/http"
	"testing"

	"github.com/lxc/incus/v7/shared/api"
	"github.com/stretchr/testify/require"

	"github.com/lxc/incus-compose/internal/testlib"
)

func TestIncusProfileRequests(t *testing.T) {
	t.Parallel()

	ctx := t.Context()

	t.Run("GetProfile", func(t *testing.T) {
		t.Parallel()

		conn, seen := recordingServer(t, `{"name":"default"}`)

		profile, _, err := conn.GetProfile(ctx, "default")
		require.NoError(t, err)
		require.Equal(t, "default", profile.Name)

		req := seen.all()[0]
		require.Equal(t, http.MethodGet, req.method)
		require.Equal(t, "/1.0/profiles/default?project=myproject", req.uri())
	})

	t.Run("CreateProfile", func(t *testing.T) {
		t.Parallel()

		conn, seen := recordingServer(t, `{}`)

		require.NoError(t, conn.CreateProfile(ctx, api.ProfilesPost{Name: "web"}))

		req := seen.all()[0]
		require.Equal(t, http.MethodPost, req.method)
		require.Equal(t, "/1.0/profiles?project=myproject", req.uri())
		require.Contains(t, req.body, `"name":"web"`)
	})

	t.Run("UpdateProfile sends the ETag", func(t *testing.T) {
		t.Parallel()

		conn, seen := recordingServer(t, `{}`)

		err := conn.UpdateProfile(ctx, "web", api.ProfilePut{Description: "d"}, "the-etag")
		require.NoError(t, err)

		req := seen.all()[0]
		require.Equal(t, http.MethodPut, req.method)
		require.Equal(t, "/1.0/profiles/web?project=myproject", req.uri())
		require.Equal(t, "the-etag", req.etag)
	})

	t.Run("DeleteProfile", func(t *testing.T) {
		t.Parallel()

		conn, seen := recordingServer(t, `{}`)

		require.NoError(t, conn.DeleteProfile(ctx, "web"))

		req := seen.all()[0]
		require.Equal(t, http.MethodDelete, req.method)
		require.Equal(t, "/1.0/profiles/web?project=myproject", req.uri())
	})
}

func TestIncusProfileAgainstRealIncus(t *testing.T) {
	testlib.SkipLocal(t)
	t.Parallel()

	ctx := t.Context()
	conn := testConnection(t)

	// Every project has a default profile.
	profile, etag, err := conn.GetProfile(ctx, "default")
	require.NoError(t, err)
	require.Equal(t, "default", profile.Name)
	require.NotEmpty(t, etag, "GetProfile must return the ETag header")

	_, _, err = conn.GetProfile(ctx, "ic-iclient-no-such-profile")
	require.True(t, api.StatusErrorCheck(err, 404), "want a 404, got %v", err)
}
