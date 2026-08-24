package iclient

import (
	"io"
	"os"
	"testing"

	"github.com/lxc/incus/v7/shared/api"
	"github.com/stretchr/testify/require"
)

// TestIncusVolumeSFTP writes a file into a custom volume and reads it back,
// which is the path volume seeding and the image lock both take.
func TestIncusVolumeSFTP(t *testing.T) {
	skipLocal(t)
	t.Parallel()

	ctx := t.Context()
	conn := testConnection(t)

	pools, err := conn.GetStoragePoolNames(ctx)
	require.NoError(t, err)
	require.NotEmpty(t, pools, "the server has no storage pool")

	pool := pools[0]

	project := testProject(t, conn, "iclient-sftp")

	const volume = "iclient-sftp-volume"

	err = conn.CreateStoragePoolVolume(ctx, project, pool, api.StorageVolumesPost{
		Name: volume,
		Type: "custom",
	})
	require.NoError(t, err)

	// The volume goes with the project, so no separate cleanup.
	stored, _, err := conn.GetStoragePoolVolume(ctx, project, pool, "custom", volume, nil)
	require.NoError(t, err)
	require.Equal(t, volume, stored.Name)

	client, err := conn.GetStoragePoolVolumeFileSFTP(ctx, project, pool, "custom", volume)
	require.NoError(t, err)

	defer func() { _ = client.Close() }()

	// O_EXCL is what the image lock relies on, so exercise that shape.
	file, err := client.OpenFile("/lock", os.O_CREATE|os.O_EXCL|os.O_WRONLY)
	require.NoError(t, err)

	_, err = file.Write([]byte("held"))
	require.NoError(t, err)
	require.NoError(t, file.Close())

	// A second exclusive create must lose.
	_, err = client.OpenFile("/lock", os.O_CREATE|os.O_EXCL|os.O_WRONLY)
	require.Error(t, err, "O_EXCL must refuse an existing file")

	read, err := client.Open("/lock")
	require.NoError(t, err)

	defer func() { _ = read.Close() }()

	content, err := io.ReadAll(read)
	require.NoError(t, err)
	require.Equal(t, "held", string(content))
}

// TestIncusInstanceSFTPNotFound pins the refused-upgrade path: the server's
// own error comes back, mapped like every other call's.
func TestIncusInstanceSFTPNotFound(t *testing.T) {
	skipLocal(t)
	t.Parallel()

	conn := testConnection(t)

	_, err := conn.GetInstanceFileSFTP(t.Context(), "", "ic-iclient-does-not-exist")
	require.Error(t, err)
	require.True(t, api.StatusErrorCheck(err, 404), "want a 404 StatusError, got %v", err)
}
