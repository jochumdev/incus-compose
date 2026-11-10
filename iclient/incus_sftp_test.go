package iclient

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"

	"github.com/lxc/incus/v7/shared/api"
	"github.com/pkg/sftp"
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
	}, nil)
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

// TestIncusSFTPRetriesAnEarlyClose: a stream that ends before the version
// packet is opened again, and a server that never recovers gives up.
func TestIncusSFTPRetriesAnEarlyClose(t *testing.T) {
	t.Parallel()

	for name, drops := range map[string]int{"recovers": 2, "never recovers": 1000} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			var mu sync.Mutex

			opened := 0

			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				mu.Lock()
				opened++
				drop := opened <= drops
				mu.Unlock()

				stream, buf, err := w.(http.Hijacker).Hijack()
				if err != nil {
					return
				}

				_, _ = buf.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: sftp\r\nConnection: Upgrade\r\n\r\n")
				_ = buf.Flush()

				if drop {
					_ = stream.Close()

					return
				}

				srv, err := sftp.NewServer(stream)
				if err != nil {
					return
				}

				_ = srv.Serve()
			}))
			t.Cleanup(server.Close)

			conn, err := NewConnection(&ConfigRemoteInfo{Name: "sftp", Addrs: []string{server.URL}, InsecureSkipVerify: true})
			require.NoError(t, err)

			client, err := conn.sftpClient(t.Context(), "", "/sftp")

			if drops < incusSFTPAttempts {
				require.NoError(t, err)
				require.NoError(t, client.Close())
			} else {
				require.ErrorIs(t, err, io.ErrUnexpectedEOF)
			}

			mu.Lock()
			defer mu.Unlock()
			require.Equal(t, min(drops+1, incusSFTPAttempts), opened)
		})
	}
}
