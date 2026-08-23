package client

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"net/http"
	"time"

	incusApi "github.com/lxc/incus/v7/shared/api"
	"github.com/pkg/sftp"

	"github.com/lxc/incus-compose/iclient"
)

// VolumeInfos represents either a Source or a Backup in a BackupEntry.
type VolumeInfos struct {
	Project string `json:"project"`
	Pool    string `json:"pool"`
	Name    string `json:"name"`
}

// BackupVolume represents a single volume in a backup run.
type BackupVolume struct {
	Source VolumeInfos `json:"source"`
	Backup VolumeInfos `json:"backup"`
}

// BackupConfig holds backup configuration from x-incus-compose.backup.
type BackupConfig struct {
	MetaVolume string `json:"meta_volume" mapstructure:"meta_volume"`
	Timestamp  string `json:"timestamp"`
	Name       string `json:"name"`
	Pool       string `json:"pool"`
}

// BackupManifestVolume ensures the manifest volume exists and returns it.
func BackupManifestVolume(ctx context.Context, bc *Client, cfg BackupConfig) (*StorageVolume, error) {
	rBMVol, err := bc.Resource(KindStorageVolume, cfg.MetaVolume, &StorageVolumeConfig{Pool: cfg.Pool})
	if err != nil {
		return nil, err
	}

	err = RunAction(ctx, rBMVol, ActionEnsure, OptionCreate())
	if err != nil {
		return nil, err
	}

	bMVol, ok := rBMVol.(*StorageVolume)
	if !ok {
		return nil, ErrUnknownResource.WithText("while converting a backup resource to a StorageVolume")
	}

	return bMVol, nil
}

// BackupLock creates backupLock which you MUST release with Unlock().
// sc is owned by the caller; close it after Unlock.
func BackupLock(ctx context.Context, bc *Client, sc *sftp.Client, cfg BackupConfig, stale time.Duration, lockName string) (*VolumeLock, error) {
	bc.LogDebug("Locking", "cfg", cfg, "lock_name", lockName)

	bMVol, err := BackupManifestVolume(ctx, bc, cfg)
	if err != nil {
		return nil, err
	}

	return bMVol.Lock(ctx, sc, lockName, stale)
}

// BackupSnapshots returns the restore points a backup volume holds. A volume
// that is not there yet reads as none, which is what an unbacked-up volume is.
func BackupSnapshots(ctx context.Context, bc *Client, pool string, volume string) ([]string, error) {
	conn, err := bc.Connection()
	if err != nil {
		return nil, err
	}

	names, err := conn.GetStoragePoolVolumeSnapshotNames(ctx, bc.incusProject, pool, "custom", volume)
	if incusApi.StatusErrorCheck(err, http.StatusNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, ErrUnknown.Wrap(err)
	}

	return names, nil
}

// BackupVolumeUsage returns the bytes a backup volume occupies. The restore
// points on it share that space, so it is not a per-timestamp figure.
func BackupVolumeUsage(ctx context.Context, bc *Client, pool string, volume string) (int64, error) {
	conn, err := bc.Connection()
	if err != nil {
		return 0, err
	}

	state, err := conn.GetStoragePoolVolumeState(ctx, bc.incusProject, pool, "custom", volume)
	if incusApi.StatusErrorCheck(err, http.StatusNotFound) {
		return 0, nil
	}
	if err != nil {
		return 0, ErrUnknown.Wrap(err)
	}

	if state.Usage == nil {
		return 0, nil
	}

	return int64(state.Usage.Used), nil
}

// BackupDeleteSnapshot removes one restore point from a backup volume.
func BackupDeleteSnapshot(ctx context.Context, bc *Client, pool string, volume string, snapshot string) error {
	conn, err := bc.Connection()
	if err != nil {
		return err
	}

	op, err := conn.DeleteStoragePoolVolumeSnapshot(ctx, bc.incusProject, pool, "custom", volume, snapshot)
	if incusApi.StatusErrorCheck(err, http.StatusNotFound) {
		return nil
	}
	if err != nil {
		return ErrDelete.WithText(volume + "/" + snapshot).Wrap(err)
	}

	_, err = iclient.WaitOperation(ctx, op)
	if err != nil {
		return ErrDelete.WithText(volume + "/" + snapshot).Wrap(err)
	}

	return nil
}

// BackupDeleteManifest removes one run's manifest from the manifest volume.
func BackupDeleteManifest(ctx context.Context, bc *Client, cfg BackupConfig) error {
	bMVol, err := BackupManifestVolume(ctx, bc, cfg)
	if err != nil {
		return err
	}

	sc, err := bMVol.SFTP(ctx)
	if err != nil {
		return err
	}
	defer bc.WarnError(sc.Close, "Failed to close an SFTP connection")

	err = sc.Remove(cfg.Timestamp + ".json")
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}

	return err
}

// BackupWriteManifest writes one run's manifest into the manifest volume.
func BackupWriteManifest(ctx context.Context, bc *Client, cfg BackupConfig, data []byte) error {
	bMVol, err := BackupManifestVolume(ctx, bc, cfg)
	if err != nil {
		return err
	}

	sc, err := bMVol.SFTP(ctx)
	if err != nil {
		return err
	}
	defer bc.WarnError(sc.Close, "Failed to close an SFTP connection")

	return sftpCreateFile(bc, sc, cfg.Timestamp+".json", instanceFileArgs{Content: bytes.NewReader(data), Type: "file"}, true)
}
