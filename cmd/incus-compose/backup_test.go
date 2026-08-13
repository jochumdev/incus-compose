package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	incusApi "github.com/lxc/incus/v7/shared/api"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lxc/incus-compose/client"
	"github.com/lxc/incus-compose/testlib"
)

func assertBackupVolumeExists(t *testing.T, c *client.Client, pool, name string) {
	t.Helper()

	conn, err := c.Connection()
	require.NoError(t, err)

	_, _, err = conn.GetStoragePoolVolume(t.Context(), pool, "custom", name, nil)
	require.NoError(t, err, "volume %q should exist in pool %q", name, pool)
}

func assertBackupSnapshotExists(t *testing.T, c *client.Client, pool, volumeName, snapshotName string) {
	t.Helper()

	conn, err := c.Connection()
	require.NoError(t, err)

	names, err := conn.GetStoragePoolVolumeSnapshotNames(t.Context(), pool, "custom", volumeName)
	require.NoError(t, err)

	assert.Contains(t, names, snapshotName, "on volume %s in pool %s", volumeName, pool)
}

func openBackupProject(ctx context.Context, t *testing.T, composeProject string) *client.Client {
	t.Helper()

	gc, err := client.NewTestClient(ctx)
	require.NoError(t, err)

	err = gc.Connect()
	require.NoError(t, err)

	backupProject := composeProject + "-backup"
	c, err := gc.EnsureProject(backupProject)
	require.NoError(t, err)

	err = c.Open()
	require.NoError(t, err)

	return c
}

func deleteBackupProject(ctx context.Context, _ *testing.T, composeProject string) {
	gc, err := client.NewTestClient(ctx)
	if err != nil {
		return
	}

	_ = gc.Connect()
	_ = gc.DeleteProject(composeProject+"-backup", true)
}

// backupManifestIncusName resolves the manifest volume's Incus name. It goes
// through Resource(), so it carries the vol- prefix that the mirrors, copied
// under a raw name, do not.
func backupManifestIncusName(t *testing.T, c *client.Client) string {
	t.Helper()

	r, err := c.Resource(client.KindStorageVolume, backupManifestVolume, &client.StorageVolumeConfig{})
	require.NoError(t, err)

	return r.IncusName()
}

// backupManifestDir lists the manifest volume, which holds both the per-run
// manifests and the lock files. A missing volume reads as empty.
func backupManifestDir(t *testing.T, c *client.Client) []string {
	t.Helper()

	conn, err := c.Connection()
	require.NoError(t, err)

	name := backupManifestIncusName(t, c)
	_, _, err = conn.GetStoragePoolVolume(t.Context(), c.Config().DefaultStoragePool, "custom", name, nil)
	if incusApi.StatusErrorCheck(err, http.StatusNotFound) {
		return nil
	}
	require.NoError(t, err)

	sc, err := conn.GetStoragePoolVolumeFileSFTP(t.Context(), c.Config().DefaultStoragePool, "custom", name)
	require.NoError(t, err)
	defer func() { _ = sc.Close() }()

	entries, err := sc.ReadDir("/")
	require.NoError(t, err)

	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}

	return names
}

// readBackupManifest returns every recorded backup run, oldest first.
func readBackupManifest(t *testing.T, c *client.Client) []backupManifest {
	t.Helper()

	conn, err := c.Connection()
	require.NoError(t, err)

	name := backupManifestIncusName(t, c)
	sc, err := conn.GetStoragePoolVolumeFileSFTP(t.Context(), c.Config().DefaultStoragePool, "custom", name)
	require.NoError(t, err)
	defer func() { _ = sc.Close() }()

	manifests := []backupManifest{}
	for _, manifestName := range backupManifestDir(t, c) {
		if !strings.HasSuffix(manifestName, ".json") {
			continue
		}

		data, err := sc.Open(manifestName)
		require.NoError(t, err)

		m := backupManifest{}
		err = json.NewDecoder(data).Decode(&m)
		_ = data.Close()
		require.NoError(t, err, "decoding manifest %q", manifestName)

		manifests = append(manifests, m)
	}

	// RFC3339Nano trims trailing zeros, so file order is not run order.
	slices.SortFunc(manifests, func(a, b backupManifest) int {
		return backupTime(t, a).Compare(backupTime(t, b))
	})

	return manifests
}

func backupTime(t *testing.T, m backupManifest) time.Time {
	t.Helper()

	ts, err := time.Parse(time.RFC3339Nano, m.Timestamp)
	require.NoError(t, err, "manifest timestamp %q", m.Timestamp)

	return ts
}

// assertNoLockFile checks that both lock kinds - the metadata lock and the
// per-volume ones - were released.
func assertNoLockFile(t *testing.T, c *client.Client) {
	t.Helper()

	for _, name := range backupManifestDir(t, c) {
		if strings.HasSuffix(name, ".lock") {
			t.Errorf("lock file %q should not exist after backup completes", name)
		}
	}
}

func TestE2EBackupCreate(t *testing.T) {
	testlib.SkipE2E(t)
	testlib.SkipLocal(t)
	t.Parallel()

	compose := "../../test/fixtures/with-backup/compose.yaml"
	projectDir := t.TempDir()

	ctx := t.Context()
	pn := t.Name()

	t.Cleanup(func() {
		_, _ = runCommand(context.Background(), t, pn, "-f", compose, "down", "--project")
		deleteBackupProject(context.Background(), t, pn)
	})

	_, err := runCommand(ctx, t, pn, "--project-directory", projectDir, "-f", compose, "up", "--detach")
	require.NoError(t, err)

	_, err = runCommand(ctx, t, pn, "--project-directory", projectDir, "-f", compose, "backup", "create")
	require.NoError(t, err)

	bp := openBackupProject(ctx, t, pn)
	defer func() { _ = bp.Done() }()

	entries := readBackupManifest(t, bp)
	require.Len(t, entries, 1)
	require.Len(t, entries[0].Volumes, 1)
	assert.Equal(t, "vol-data", entries[0].Volumes[0].Source.Name)
	assert.Equal(t, "ic-backup-data", entries[0].Volumes[0].Backup.Name)

	pool := entries[0].Volumes[0].Backup.Pool
	require.NotEmpty(t, pool, "the manifest must name the pool the backup volume lives in")
	assertBackupVolumeExists(t, bp, pool, "ic-backup-data")
	assertBackupSnapshotExists(t, bp, pool, "ic-backup-data", entries[0].Timestamp)
	assertNoLockFile(t, bp)

	c := projectClient(ctx, t, pn)
	exists, err := c.InstanceExists("app-1")
	require.NoError(t, err)
	assert.True(t, exists, "service should be restarted after consistent backup")
}

func TestE2EBackupCreateLive(t *testing.T) {
	testlib.SkipE2E(t)
	testlib.SkipLocal(t)
	t.Parallel()

	compose := "../../test/fixtures/with-backup/compose.yaml"
	projectDir := t.TempDir()

	ctx := t.Context()
	pn := t.Name()

	t.Cleanup(func() {
		_, _ = runCommand(context.Background(), t, pn, "-f", compose, "down", "--project")
		deleteBackupProject(context.Background(), t, pn)
	})

	_, err := runCommand(ctx, t, pn, "--project-directory", projectDir, "-f", compose, "up", "--detach")
	require.NoError(t, err)

	_, err = runCommand(ctx, t, pn, "--project-directory", projectDir, "-f", compose, "backup", "create", "--live")
	require.NoError(t, err)

	bp := openBackupProject(ctx, t, pn)
	defer func() { _ = bp.Done() }()

	entries := readBackupManifest(t, bp)
	require.Len(t, entries, 1)
	require.Len(t, entries[0].Volumes, 1)

	pool := entries[0].Volumes[0].Backup.Pool
	require.NotEmpty(t, pool, "the manifest must name the pool the backup volume lives in")
	assertBackupVolumeExists(t, bp, pool, "ic-backup-data")
	assertBackupSnapshotExists(t, bp, pool, "ic-backup-data", entries[0].Timestamp)
	assertNoLockFile(t, bp)

	c := projectClient(ctx, t, pn)
	exists, err := c.InstanceExists("app-1")
	require.NoError(t, err)
	assert.True(t, exists, "a live backup must not stop the service")
}

func TestE2EBackupCreateNamed(t *testing.T) {
	testlib.SkipE2E(t)
	testlib.SkipLocal(t)
	t.Parallel()

	compose := "../../test/fixtures/with-backup/compose.yaml"
	projectDir := t.TempDir()

	ctx := t.Context()
	pn := t.Name()

	t.Cleanup(func() {
		_, _ = runCommand(context.Background(), t, pn, "-f", compose, "down", "--project")
		deleteBackupProject(context.Background(), t, pn)
	})

	_, err := runCommand(ctx, t, pn, "--project-directory", projectDir, "-f", compose, "up", "--detach")
	require.NoError(t, err)

	_, err = runCommand(ctx, t, pn, "--project-directory", projectDir, "-f", compose, "backup", "create", "--name", "daily")
	require.NoError(t, err)

	bp := openBackupProject(ctx, t, pn)
	defer func() { _ = bp.Done() }()

	entries := readBackupManifest(t, bp)
	require.Len(t, entries, 1)
	assert.Equal(t, "daily", entries[0].Name)
	assertNoLockFile(t, bp)
}

func TestE2EBackupCreateIncremental(t *testing.T) {
	testlib.SkipE2E(t)
	testlib.SkipLocal(t)
	t.Parallel()

	compose := "../../test/fixtures/with-backup/compose.yaml"
	projectDir := t.TempDir()

	ctx := t.Context()
	pn := t.Name()

	t.Cleanup(func() {
		_, _ = runCommand(context.Background(), t, pn, "-f", compose, "down", "--project")
		deleteBackupProject(context.Background(), t, pn)
	})

	_, err := runCommand(ctx, t, pn, "--project-directory", projectDir, "-f", compose, "up", "--detach")
	require.NoError(t, err)

	_, err = runCommand(ctx, t, pn, "--project-directory", projectDir, "-f", compose, "backup", "create", "--name", "first")
	require.NoError(t, err)

	_, err = runCommand(ctx, t, pn, "--project-directory", projectDir, "-f", compose, "backup", "create", "--name", "second")
	require.NoError(t, err)

	bp := openBackupProject(ctx, t, pn)
	defer func() { _ = bp.Done() }()

	entries := readBackupManifest(t, bp)
	require.Len(t, entries, 2)
	assert.Equal(t, "first", entries[0].Name)
	assert.Equal(t, "second", entries[1].Name)
	assert.NotEqual(t, entries[0].Timestamp, entries[1].Timestamp)
	assertNoLockFile(t, bp)

	// The second run refreshes the same mirror, so both snapshots sit on it.
	require.Len(t, entries[0].Volumes, 1)
	pool := entries[0].Volumes[0].Backup.Pool
	require.NotEmpty(t, pool, "the manifest must name the pool the backup volume lives in")
	assertBackupVolumeExists(t, bp, pool, "ic-backup-data")
	assertBackupSnapshotExists(t, bp, pool, "ic-backup-data", entries[0].Timestamp)
	assertBackupSnapshotExists(t, bp, pool, "ic-backup-data", entries[1].Timestamp)
}

func TestE2EBackupCreateFiltered(t *testing.T) {
	testlib.SkipE2E(t)
	testlib.SkipLocal(t)
	t.Parallel()

	projectDir := testlib.WriteTempFiles(t, map[string]string{
		"compose.yaml": `
services:
  db:
    image: docker.io/library/busybox:glibc
    entrypoint: ["/bin/sh", "-c", "mkdir -p /www && httpd -f -v -p 8080 -h /www"]
    volumes:
      - type: volume
        source: db-data
        target: /data
  app:
    image: docker.io/library/busybox:glibc
    entrypoint: ["/bin/sh", "-c", "mkdir -p /www && httpd -f -v -p 8080 -h /www"]
    volumes:
      - type: volume
        source: app-data
        target: /data

volumes:
  db-data:
  app-data:
`})
	ctx := t.Context()
	pn := t.Name()

	t.Cleanup(func() {
		_, _ = runCommand(context.Background(), t, pn, "--project-directory", projectDir, "down", "--project")
		deleteBackupProject(context.Background(), t, pn)
	})

	_, err := runCommand(ctx, t, pn, "--project-directory", projectDir, "up", "--detach")
	require.NoError(t, err)

	_, err = runCommand(ctx, t, pn, "--project-directory", projectDir, "backup", "create", "db")
	require.NoError(t, err)

	bp := openBackupProject(ctx, t, pn)
	defer func() { _ = bp.Done() }()

	entries := readBackupManifest(t, bp)
	require.Len(t, entries, 1)
	require.Len(t, entries[0].Volumes, 1, "app-data must not be backed up")
	assert.Equal(t, "vol-db-data", entries[0].Volumes[0].Source.Name)
	assert.Equal(t, "ic-backup-db-data", entries[0].Volumes[0].Backup.Name)
	assertNoLockFile(t, bp)

	conn, err := bp.Connection()
	require.NoError(t, err)

	_, _, err = conn.GetStoragePoolVolume(t.Context(), bp.Config().DefaultStoragePool, "custom", "ic-backup-app-data", nil)
	assert.True(t, incusApi.StatusErrorCheck(err, http.StatusNotFound), "app-data must not have a backup mirror")
}

func TestE2EBackupCreateNoVolumes(t *testing.T) {
	testlib.SkipE2E(t)
	testlib.SkipLocal(t)
	t.Parallel()

	compose := "../../test/fixtures/with-backup/compose.yaml"
	projectDir := t.TempDir()

	ctx := t.Context()
	pn := t.Name()

	t.Cleanup(func() {
		_, _ = runCommand(context.Background(), t, pn, "-f", compose, "down", "--project")
		deleteBackupProject(context.Background(), t, pn)
	})

	_, err := runCommand(ctx, t, pn, "--project-directory", projectDir, "-f", compose, "up", "--detach")
	require.NoError(t, err)

	_, err = runCommand(ctx, t, pn, "--project-directory", projectDir, "-f", compose, "backup", "create", "nonexistent")
	require.NoError(t, err)

	bp := openBackupProject(ctx, t, pn)
	defer func() { _ = bp.Done() }()

	assertBackupVolumeExists(t, bp, bp.Config().DefaultStoragePool, backupManifestIncusName(t, bp))
	entries := readBackupManifest(t, bp)
	require.Empty(t, entries)
	assertNoLockFile(t, bp)
}

func TestE2EBackupCreateDefaultPool(t *testing.T) {
	testlib.SkipE2E(t)
	testlib.SkipLocal(t)
	t.Parallel()

	projectDir := testlib.WriteTempFiles(t, map[string]string{
		"compose.yaml": `
services:
  app:
    image: docker.io/library/busybox:glibc
    entrypoint: ["/bin/sh", "-c", "mkdir -p /www && httpd -f -v -p 8080 -h /www"]
    volumes:
      - type: volume
        source: data
        target: /data

volumes:
  data:
`})

	ctx := t.Context()
	pn := t.Name()

	t.Cleanup(func() {
		_, _ = runCommand(context.Background(), t, pn, "--project-directory", projectDir, "down", "--project")
		deleteBackupProject(context.Background(), t, pn)
	})

	_, err := runCommand(ctx, t, pn, "--project-directory", projectDir, "up", "--detach")
	require.NoError(t, err)

	_, err = runCommand(ctx, t, pn, "--project-directory", projectDir, "backup", "create")
	require.NoError(t, err)

	bp := openBackupProject(ctx, t, pn)
	defer func() { _ = bp.Done() }()

	entries := readBackupManifest(t, bp)
	require.Len(t, entries, 1)
	require.Len(t, entries[0].Volumes, 1)

	pool := bp.Config().DefaultStoragePool
	assert.Equal(t, pool, entries[0].Volumes[0].Backup.Pool, "an unconfigured pool must resolve to the default one")
	assertBackupVolumeExists(t, bp, pool, "ic-backup-data")
	assertNoLockFile(t, bp)
}

func TestE2EBackupCreateParallel(t *testing.T) {
	testlib.SkipE2E(t)
	testlib.SkipLocal(t)
	t.Parallel()

	compose := "../../test/fixtures/with-backup/compose.yaml"
	projectDir := t.TempDir()

	ctx := t.Context()
	pn := t.Name()

	t.Cleanup(func() {
		_, _ = runCommand(context.Background(), t, pn, "-f", compose, "down", "--project")
		deleteBackupProject(context.Background(), t, pn)
	})

	_, err := runCommand(ctx, t, pn, "--project-directory", projectDir, "-f", compose, "up", "--detach")
	require.NoError(t, err)

	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = runCommand(ctx, t, pn, "--project-directory", projectDir, "-f", compose, "backup", "create", "--live")
		}(i)
	}
	wg.Wait()

	require.NoError(t, errs[0])
	require.NoError(t, errs[1])

	bp := openBackupProject(ctx, t, pn)
	defer func() { _ = bp.Done() }()

	entries := readBackupManifest(t, bp)
	require.Len(t, entries, 2)
	require.Len(t, entries[0].Volumes, 1)

	// Both runs refresh the one mirror, so the lock has to serialize them well
	// enough that neither snapshot is lost.
	pool := entries[0].Volumes[0].Backup.Pool
	require.NotEmpty(t, pool, "the manifest must name the pool the backup volume lives in")
	assertBackupSnapshotExists(t, bp, pool, "ic-backup-data", entries[0].Timestamp)
	assertBackupSnapshotExists(t, bp, pool, "ic-backup-data", entries[1].Timestamp)
	assertNoLockFile(t, bp)
}

func TestE2EBackupCreateDataIntegrity(t *testing.T) {
	testlib.SkipE2E(t)
	testlib.SkipLocal(t)
	t.Parallel()

	compose := "../../test/fixtures/with-backup/compose.yaml"
	projectDir := t.TempDir()

	ctx := t.Context()
	pn := t.Name()

	t.Cleanup(func() {
		_, _ = runCommand(context.Background(), t, pn, "-f", compose, "down", "--project")
		deleteBackupProject(context.Background(), t, pn)
	})

	_, err := runCommand(ctx, t, pn, "--project-directory", projectDir, "-f", compose, "up", "--detach")
	require.NoError(t, err)

	// Seed a file into the compose volume.
	c := projectClient(ctx, t, pn)
	conn, err := c.Connection()
	require.NoError(t, err)

	sc, err := conn.GetStoragePoolVolumeFileSFTP(t.Context(), c.Config().DefaultStoragePool, "custom", "vol-data")
	require.NoError(t, err)

	f, err := sc.Create("backup-test.txt")
	require.NoError(t, err)

	_, err = io.WriteString(f, "hello-backup")
	require.NoError(t, err)
	require.NoError(t, f.Close())
	require.NoError(t, sc.Close())

	_, err = runCommand(ctx, t, pn, "--project-directory", projectDir, "-f", compose, "backup", "create", "--live")
	require.NoError(t, err)

	bp := openBackupProject(ctx, t, pn)
	defer func() { _ = bp.Done() }()

	// The file must be present in the backup mirror.
	bConn, err := bp.Connection()
	require.NoError(t, err)

	bSc, err := bConn.GetStoragePoolVolumeFileSFTP(t.Context(), bp.Config().DefaultStoragePool, "custom", "ic-backup-data")
	require.NoError(t, err)
	defer func() { _ = bSc.Close() }()

	data, err := bSc.Open("backup-test.txt")
	require.NoError(t, err)
	defer func() { _ = data.Close() }()

	content, err := io.ReadAll(data)
	require.NoError(t, err)
	assert.Equal(t, "hello-backup", string(content))
}
