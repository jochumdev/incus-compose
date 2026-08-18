package client

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lxc/incus-compose/internal/testlib"
)

func TestIDFields(t *testing.T) {
	t.Parallel()

	const passwd = `# a comment
root:x:0:0:root:/root:/bin/sh

nginx:x:101:102:nginx user:/var/cache/nginx:/sbin/nologin
broken:x:notanumber:0:::
short:x
`

	uid, gid, ok := idFields(passwd, "nginx")
	assert.True(t, ok)
	assert.Equal(t, uint64(101), uid)
	assert.Equal(t, uint64(102), gid)

	_, _, ok = idFields(passwd, "broken")
	assert.False(t, ok, "a line without a numeric id is no entry")

	_, _, ok = idFields(passwd, "short")
	assert.False(t, ok, "a line without an id field is no entry")

	_, _, ok = idFields(passwd, "nosuchuser")
	assert.False(t, ok)

	// /etc/group carries its members where /etc/passwd carries the group.
	const group = `root:x:0:
www-data:x:33:nginx,www
`

	gid, _, ok = idFields(group, "www-data")
	assert.True(t, ok)
	assert.Equal(t, uint64(33), gid)
}

// TestImageResolveUser resolves the names only the image's own /etc/passwd and
// /etc/group know, and leaves numbers alone without reading it at all.
func TestImageResolveUser(t *testing.T) {
	t.Parallel()
	testlib.SkipLocal(t)

	ctx := t.Context()
	c := newRandomTestClient(t, "image-user-")

	res, err := c.Resource(KindImage, "docker.io/nginx:alpine", &ImageConfig{})
	require.NoError(t, err)
	require.NoError(t, RunAction(ctx, res, ActionEnsure, OptionCreate()))

	img, ok := res.(*Image)
	require.True(t, ok)

	conn, err := c.Connection()
	require.NoError(t, err)

	// Numbers need no lookup, so nothing is created to do one with.
	uid, gid, err := img.ResolveUser(ctx, "1000:1001")
	require.NoError(t, err)
	assert.Equal(t, uint64(1000), uid)
	assert.Equal(t, uint64(1001), gid)

	names, err := conn.GetInstanceNames(ctx, nil)
	require.NoError(t, err)
	require.Empty(t, names, "a numeric user must not read the image")

	// A name without a group takes the user's own, as login would.
	uid, gid, err = img.ResolveUser(ctx, "nginx")
	require.NoError(t, err)
	assert.Equal(t, uint64(101), uid)
	assert.Equal(t, uint64(101), gid)

	uid, gid, err = img.ResolveUser(ctx, "nginx:root")
	require.NoError(t, err)
	assert.Equal(t, uint64(101), uid)
	assert.Equal(t, uint64(0), gid)

	uid, gid, err = img.ResolveUser(ctx, "1000:root")
	require.NoError(t, err)
	assert.Equal(t, uint64(1000), uid)
	assert.Equal(t, uint64(0), gid)

	_, _, err = img.ResolveUser(ctx, "nosuchuser")
	require.ErrorIs(t, err, ErrNoSuchUser)

	_, _, err = img.ResolveUser(ctx, "nginx:nosuchgroup")
	require.ErrorIs(t, err, ErrNoSuchUser)

	// One instance served every lookup above.
	names, err = conn.GetInstanceNames(ctx, nil)
	require.NoError(t, err)
	require.Len(t, names, 1)

	require.NoError(t, img.Done())
	require.NoError(t, img.Done(), "Done is called twice on the way out of up")

	names, err = conn.GetInstanceNames(ctx, nil)
	require.NoError(t, err)
	assert.Empty(t, names)
}
