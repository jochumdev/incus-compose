package client

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
	skipLocal(t)

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
	owner, err := img.ResolveUser(ctx, "1000:1001")
	require.NoError(t, err)
	assert.Equal(t, &Owner{UID: 1000, GID: 1001}, owner)

	names, err := conn.GetInstanceNames(ctx, c.incusProject, nil)
	require.NoError(t, err)
	require.Empty(t, names, "a numeric user must not read the image")

	// A name without a group takes the user's own, as login would.
	owner, err = img.ResolveUser(ctx, "nginx")
	require.NoError(t, err)
	assert.Equal(t, &Owner{UID: 101, GID: 101}, owner)

	owner, err = img.ResolveUser(ctx, "nginx:root")
	require.NoError(t, err)
	assert.Equal(t, &Owner{UID: 101, GID: 0}, owner)

	owner, err = img.ResolveUser(ctx, "1000:root")
	require.NoError(t, err)
	assert.Equal(t, &Owner{UID: 1000, GID: 0}, owner)

	_, err = img.ResolveUser(ctx, "nosuchuser")
	require.ErrorIs(t, err, ErrNoSuchUser)

	_, err = img.ResolveUser(ctx, "nginx:nosuchgroup")
	require.ErrorIs(t, err, ErrNoSuchUser)

	// One instance served every lookup above.
	names, err = conn.GetInstanceNames(ctx, c.incusProject, nil)
	require.NoError(t, err)
	require.Len(t, names, 1)

	require.NoError(t, img.Done())
	require.NoError(t, img.Done(), "Done is called twice on the way out of up")

	names, err = conn.GetInstanceNames(ctx, c.incusProject, nil)
	require.NoError(t, err)
	assert.Empty(t, names)
}
