package main

import (
	"context"
	"io"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lxc/incus-compose/client"
	"github.com/lxc/incus-compose/internal/testlib"
)

// writeVolumeMarker puts a known string into the compose volume.
func writeVolumeMarker(t *testing.T, c *client.Client, volume string, content string) {
	t.Helper()

	conn, err := c.Connection()
	require.NoError(t, err)

	sc, err := conn.GetStoragePoolVolumeFileSFTP(t.Context(), c.IncusProject(), c.Config().DefaultStoragePool, "custom", volume)
	require.NoError(t, err)
	defer func() { _ = sc.Close() }()

	f, err := sc.Create("marker.txt")
	require.NoError(t, err)

	_, err = io.WriteString(f, content)
	require.NoError(t, err)
	require.NoError(t, f.Close())
}

// readVolumeMarker reads back what writeVolumeMarker wrote.
func readVolumeMarker(t *testing.T, c *client.Client, volume string) string {
	t.Helper()

	conn, err := c.Connection()
	require.NoError(t, err)

	sc, err := conn.GetStoragePoolVolumeFileSFTP(t.Context(), c.IncusProject(), c.Config().DefaultStoragePool, "custom", volume)
	require.NoError(t, err)
	defer func() { _ = sc.Close() }()

	f, err := sc.Open("marker.txt")
	require.NoError(t, err)
	defer func() { _ = f.Close() }()

	content, err := io.ReadAll(f)
	require.NoError(t, err)

	return string(content)
}

func TestE2EBackupVerify(t *testing.T) {
	testlib.SkipE2E(t)
	testlib.SkipLocal(t)
	t.Parallel()

	compose := testlib.Fixture(t, "with-backup", "compose.yaml")
	projectDir := t.TempDir()

	ctx := t.Context()
	pn := t.Name()

	t.Cleanup(func() {
		_, err := testlib.RunCompose(context.Background(), t, pn, "", nil, "-f", compose, "down", "--project")
		if err != nil {
			t.Errorf("cleaning up project %s: %v", pn, err)
		}

		deleteBackupProject(context.Background(), t, pn)
	})

	_, err := testlib.RunCompose(ctx, t, pn, "", nil, "--project-directory", projectDir, "-f", compose, "up", "--detach")
	require.NoError(t, err)

	_, err = testlib.RunCompose(ctx, t, pn, "", nil, "--project-directory", projectDir, "-f", compose, "backup", "create")
	require.NoError(t, err)

	stdout, err := testlib.RunCompose(ctx, t, pn, "", nil, "--project-directory", projectDir, "-f", compose, "backup", "verify")
	require.NoError(t, err)
	assert.Contains(t, stdout, "vol-data")
	assert.Contains(t, stdout, backupVerifyOK)

	// A timestamp no run carries is an error, not an empty report.
	_, err = testlib.RunCompose(ctx, t, pn, "", nil, "--project-directory", projectDir, "-f", compose, "backup", "verify", "2020-01-01T00:00:00Z")
	require.Error(t, err)
}

func TestE2EBackupDeleteKeepLast(t *testing.T) {
	testlib.SkipE2E(t)
	testlib.SkipLocal(t)
	t.Parallel()

	compose := testlib.Fixture(t, "with-backup", "compose.yaml")
	projectDir := t.TempDir()

	ctx := t.Context()
	pn := t.Name()

	t.Cleanup(func() {
		_, err := testlib.RunCompose(context.Background(), t, pn, "", nil, "-f", compose, "down", "--project")
		if err != nil {
			t.Errorf("cleaning up project %s: %v", pn, err)
		}

		deleteBackupProject(context.Background(), t, pn)
	})

	_, err := testlib.RunCompose(ctx, t, pn, "", nil, "--project-directory", projectDir, "-f", compose, "up", "--detach")
	require.NoError(t, err)

	for _, name := range []string{"first", "second"} {
		_, err = testlib.RunCompose(ctx, t, pn, "", nil, "--project-directory", projectDir, "-f", compose, "backup", "create", "--name", name)
		require.NoError(t, err)
	}

	bp := openBackupProject(ctx, t, pn)
	defer func() { _ = bp.Done() }()

	require.Len(t, readBackupManifest(t, bp), 2)

	_, err = testlib.RunCompose(ctx, t, pn, "", nil, "--project-directory", projectDir, "-f", compose, "backup", "delete", "--keep-last", "1")
	require.NoError(t, err)

	kept := readBackupManifest(t, bp)
	require.Len(t, kept, 1)
	assert.Equal(t, "second", kept[0].Name, "the newest run is the one that survives")

	snapshots, err := client.BackupSnapshots(ctx, bp, bp.Config().DefaultStoragePool, "ic-backup-data")
	require.NoError(t, err)
	assert.Equal(t, []string{kept[0].Timestamp}, snapshots, "the pruned run's restore point must be gone")

	// The mirror is the base every later refresh diffs against, so it stays.
	assertBackupVolumeExists(t, bp, bp.Config().DefaultStoragePool, "ic-backup-data")

	// Neither a timestamp nor --keep-last is a usage error.
	_, err = testlib.RunCompose(ctx, t, pn, "", nil, "--project-directory", projectDir, "-f", compose, "backup", "delete")
	require.Error(t, err)
}

func TestE2EBackupRestore(t *testing.T) {
	testlib.SkipE2E(t)
	testlib.SkipLocal(t)
	t.Parallel()

	compose := testlib.Fixture(t, "with-backup", "compose.yaml")
	projectDir := t.TempDir()

	ctx := t.Context()
	pn := t.Name()

	t.Cleanup(func() {
		_, err := testlib.RunCompose(context.Background(), t, pn, "", nil, "-f", compose, "down", "--project")
		if err != nil {
			t.Errorf("cleaning up project %s: %v", pn, err)
		}

		deleteBackupProject(context.Background(), t, pn)
	})

	_, err := testlib.RunCompose(ctx, t, pn, "", nil, "--project-directory", projectDir, "-f", compose, "up", "--detach")
	require.NoError(t, err)

	c := projectClient(ctx, t, pn)
	writeVolumeMarker(t, c, "vol-data", "backed-up")

	_, err = testlib.RunCompose(ctx, t, pn, "", nil, "--project-directory", projectDir, "-f", compose, "backup", "create", "--live")
	require.NoError(t, err)

	writeVolumeMarker(t, c, "vol-data", "written-after-the-backup")

	// A running service is what the backup would be restored out from under.
	_, err = testlib.RunCompose(ctx, t, pn, "", nil, "--project-directory", projectDir, "-f", compose, "backup", "restore", "--yes")
	require.Error(t, err)
	assert.Equal(t, "written-after-the-backup", readVolumeMarker(t, c, "vol-data"))

	stdout, err := testlib.RunCompose(ctx, t, pn, "", nil, "--project-directory", projectDir, "-f", compose, "backup", "restore", "--dry-run")
	require.NoError(t, err)
	assert.Contains(t, stdout, "vol-data")
	assert.Equal(t, "written-after-the-backup", readVolumeMarker(t, c, "vol-data"),
		"a dry run must not touch the volume")

	_, err = testlib.RunCompose(ctx, t, pn, "", nil, "--project-directory", projectDir, "-f", compose, "stop")
	require.NoError(t, err)

	_, err = testlib.RunCompose(ctx, t, pn, "", nil, "--project-directory", projectDir, "-f", compose, "backup", "restore", "--yes")
	require.NoError(t, err)

	assert.Equal(t, "backed-up", readVolumeMarker(t, c, "vol-data"))
}

func TestE2EBackupRestoreUnknownTimestamp(t *testing.T) {
	testlib.SkipE2E(t)
	testlib.SkipLocal(t)
	t.Parallel()

	compose := testlib.Fixture(t, "with-backup", "compose.yaml")
	projectDir := t.TempDir()

	ctx := t.Context()
	pn := t.Name()

	t.Cleanup(func() {
		_, err := testlib.RunCompose(context.Background(), t, pn, "", nil, "-f", compose, "down", "--project")
		if err != nil {
			t.Errorf("cleaning up project %s: %v", pn, err)
		}

		deleteBackupProject(context.Background(), t, pn)
	})

	_, err := testlib.RunCompose(ctx, t, pn, "", nil, "--project-directory", projectDir, "-f", compose, "up", "--detach")
	require.NoError(t, err)

	_, err = testlib.RunCompose(ctx, t, pn, "", nil, "--project-directory", projectDir, "-f", compose, "backup", "create")
	require.NoError(t, err)

	_, err = testlib.RunCompose(ctx, t, pn, "", nil, "--project-directory", projectDir, "-f", compose, "stop")
	require.NoError(t, err)

	// An unknown leading argument reads as a service name, and no volume matches it.
	stdout, err := testlib.RunCompose(ctx, t, pn, "", nil, "--project-directory", projectDir, "-f", compose, "backup", "restore", "--yes", "nonexistent")
	require.NoError(t, err)
	assert.False(t, strings.Contains(stdout, "restore storage-volume"), "nothing should be restored")
}
