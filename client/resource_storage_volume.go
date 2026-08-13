package client

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"net/http"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	incusApi "github.com/lxc/incus/v7/shared/api"
	"github.com/lxc/incus/v7/shared/util"
)

// StorageVolumeConfig configures storage volume creation.
type StorageVolumeConfig struct {
	// Pool is the storage pool to create the volume in.
	// Defaults to ClientProject.Config.DefaultStoragePool.
	Pool string

	// Shifted enables UID/GID shifting for the volume.
	Shifted bool

	// UID/GID for shifting ImageResource will overwrite this if given.
	UID uint64
	GID uint64

	// ImageResource to take UID/GID from for shifting, only
	// needed if shifting is true.
	ImageResource Resource

	// HostPath, when set, seeds the volume with the local directory contents on first creation.
	HostPath string

	// Extensions contains additional volume configuration options.
	Extensions map[string]string
}

// GetConfig returns the configuration.
func (c *StorageVolumeConfig) GetConfig() any {
	return c
}

// StorageVolume represents a custom storage volume with optional UID/GID shifting.
// Storage volumes provide persistent storage that can be attached to instances.
type StorageVolume struct {
	*BaseResource

	client    *Client
	incusName string
	created   bool
	Config    StorageVolumeConfig

	// mu serializes the actions; every image in a batch shares the lock volume.
	// Nothing the actions call may take it again - it is not reentrant.
	mu sync.Mutex

	// state is swapped whole, so a reader never sees a half-updated volume.
	state atomic.Pointer[StorageVolumeState]
}

// StorageVolumeState is what the last fetch read back from Incus.
type StorageVolumeState struct {
	// IncusVolume is nil until the volume is ensured.
	IncusVolume *incusApi.StorageVolume
	ETag        string
}

// newStorageVolume returns an existing StorageVolume resource or creates a new one.
// The volume name is automatically prefixed with the project name for isolation.
func newStorageVolume(c *Client, name string, configGetter Config) (*StorageVolume, error) {
	if configGetter == nil {
		return nil, ErrUnknownConfig.WithKindName(KindStorageVolume, name)
	}

	var config *StorageVolumeConfig
	cConfig, ok := configGetter.GetConfig().(*StorageVolumeConfig)
	if !ok {
		return nil, ErrUnknownConfig.WithKindName(KindStorageVolume, name)
	}
	config = cConfig

	// Set defaults
	if config.Pool == "" {
		config.Pool = c.Config().DefaultStoragePool
	}

	shifted, ok := config.Extensions["security.shifted"]
	if ok && !util.IsTrue(shifted) {
		config.Shifted = false
	}

	vol := &StorageVolume{
		BaseResource: NewBaseResource(KindStorageVolume, name, PriorityVolume),
		client:       c,
		Config:       *config,
	}

	vol.incusName = "vol-" + SanitizeIncusName(name, MaxIncusNameLen-4)

	// Every accessor dereferences this, so it must never be nil.
	vol.state.Store(&StorageVolumeState{})

	return vol, nil
}

// String is for debugging.
func (r *StorageVolume) String() string {
	return fmt.Sprintf("%v(%v)", r.kind, r.incusName)
}

// IncusName returns the prefixed volume name used in Incus.
func (r *StorageVolume) IncusName() string {
	return r.incusName
}

// IsEnsured returns true if the volume has been fetched/created.
func (r *StorageVolume) IsEnsured() bool {
	return r.State().IncusVolume != nil
}

// State returns the volume state as of the last fetch. It is replaced whole,
// never written into, so the result stays consistent for as long as it is held.
func (r *StorageVolume) State() *StorageVolumeState {
	return r.state.Load()
}

// clearState forgets the fetched volume.
func (r *StorageVolume) clearState() {
	r.state.Store(&StorageVolumeState{})
}

// Created returns true if the volume was created during the last Ensure call.
func (r *StorageVolume) Created() bool {
	return r.created
}

// Ensure retrieves an existing storage volume or creates a new one if Create option is set.
func (r *StorageVolume) Ensure(ctx context.Context, opts ...Option) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	options := NewOptions(opts...)

	if err := r.client.hookBefore(ctx, ActionEnsure, r, options, nil); err != nil {
		return err
	}

	_, err := r.client.Connection()
	if err != nil {
		return r.client.hookAfter(ctx, ActionEnsure, r, options, err)
	}

	err = r.get(ctx)
	if err != nil {
		if options.Create && errors.Is(err, ErrNotFound) {
			err = r.create(ctx)

			// Incus reports a lost create race either as "already exists" or as a
			// raw unique-constraint failure, so adopt whatever is there instead of
			// matching messages.
			if err != nil && r.get(ctx) == nil {
				err = nil
			}
		}
	}

	err = r.client.hookAfter(ctx, ActionEnsure, r, options, err)

	return err
}

func (r *StorageVolume) get(ctx context.Context) error {
	// r.client.LogDebug("getting volume", "pool", r.Config.Pool, "volume", r.incusName)

	conn, err := r.client.Connection()
	if err != nil {
		return err
	}

	// Try to get existing volume
	volume, eTag, err := conn.GetStoragePoolVolume(ctx, r.Config.Pool, "custom", r.incusName, nil)
	if err != nil {
		r.clearState()
		return ErrNotFound.Wrap(err)
	}

	r.state.Store(&StorageVolumeState{IncusVolume: &volume.StorageVolume, ETag: eTag})

	return nil
}

// Start validates the storage volume.
func (r *StorageVolume) Start(_ context.Context, _ ...Option) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if !r.IsEnsured() || !r.Config.Shifted {
		return nil
	}

	// If the user overwrites security.shifted do not validate against the image.
	_, ok := r.Config.Extensions["security.shifted"]
	if ok {
		return nil
	}

	// if r.Config.ImageResource == nil {
	// 	return ErrVolumeMismatch.WithText("no image resource given")
	// }

	var errs error

	// Check shifted is enabled
	volume := r.State().IncusVolume
	if !util.IsTrue(volume.Config["security.shifted"]) {
		errs = errors.Join(errors.New("expected security.shifted=true"))
	}

	expectedUID := strconv.FormatUint(r.Config.UID, 10)
	expectedGID := strconv.FormatUint(r.Config.GID, 10)
	if r.Config.UID == 0 && r.Config.GID == 0 && r.Config.ImageResource != nil {
		img, ok := r.Config.ImageResource.(*Image)
		if !ok {
			errs = errors.Join(errs, ErrUnknownResource.WithResource(r.Config.ImageResource))
			return errs
		}

		if !img.IsEnsured() {
			errs = errors.Join(errs, ErrNotEnsured.WithResource(img))
			return errs
		}

		// Check UID/GID match
		imageState := img.State()
		expectedUID = strconv.FormatUint(imageState.UID, 10)
		expectedGID = strconv.FormatUint(imageState.GID, 10)
	}

	if volume.Config["initial.uid"] != expectedUID {
		errs = errors.Join(errs, fmt.Errorf("UID mismatch, expected %s got %s", expectedUID, volume.Config["initial.uid"]))
	}

	if volume.Config["initial.gid"] != expectedGID {
		errs = errors.Join(errs, fmt.Errorf("GID mismatch, expected %s got %s", expectedGID, volume.Config["initial.gid"]))
	}

	if errs != nil {
		return ErrVolumeMismatch.Wrap(errs)
	}

	return nil
}

func (r *StorageVolume) create(ctx context.Context) error {
	config := map[string]string{}

	if r.Config.Shifted {
		if r.Config.UID == 0 && r.Config.GID == 0 && r.Config.ImageResource != nil {
			img, ok := r.Config.ImageResource.(*Image)
			if !ok {
				return ErrUnknownResource.WithResource(r.Config.ImageResource)
			}

			imageState := img.State()
			r.Config.UID = imageState.UID
			r.Config.GID = imageState.GID
		}

		config["security.shifted"] = "true"
		config["initial.uid"] = strconv.FormatUint(r.Config.UID, 10)
		config["initial.gid"] = strconv.FormatUint(r.Config.GID, 10)
	}

	volReq := incusApi.StorageVolumesPost{
		Name:        r.incusName,
		Type:        "custom",
		ContentType: "filesystem",
		StorageVolumePut: incusApi.StorageVolumePut{
			Description: fmt.Sprintf(r.client.Config().DescriptionFormat, r.Name()),
			Config:      config,
		},
	}

	// Set users config afterwards this allows them to override the above.
	if r.Config.Extensions != nil {
		maps.Copy(config, r.Config.Extensions)
	}

	// r.client.LogDebug("creating volume", "pool", r.Config.Pool, "volume", r.incusName)

	conn, err := r.client.Connection()
	if err != nil {
		return err
	}

	if err := conn.CreateStoragePoolVolume(ctx, r.Config.Pool, volReq); err != nil {
		return ErrCreate.Wrap(err)
	}

	volume, eTag, err := conn.GetStoragePoolVolume(ctx, r.Config.Pool, "custom", r.incusName, nil)
	if err != nil {
		return ErrCreate.WithText("fetching created volume").Wrap(err)
	}

	r.state.Store(&StorageVolumeState{IncusVolume: &volume.StorageVolume, ETag: eTag})
	r.created = true

	if r.Config.HostPath != "" {
		if err := r.pushDirectoryContent(ctx); err != nil {
			return ErrCreate.WithText("seeding volume from " + r.Config.HostPath).Wrap(err)
		}
	}

	return nil
}

// pushDirectoryContent walks HostPath and copies every file and directory into
// the volume over SFTP. Only called on first creation.
func (r *StorageVolume) pushDirectoryContent(ctx context.Context) error {
	sftpConn, err := r.SFTP(ctx)
	if err != nil {
		return err
	}

	defer r.client.WarnError(sftpConn.Close, "Failed to close a sFTP connection")

	var uid, gid int64

	if r.Config.ImageResource != nil {
		if img, ok := r.Config.ImageResource.(*Image); ok {
			imageState := img.State()
			uid, gid = int64(imageState.UID), int64(imageState.GID)
		}
	}

	root, err := os.OpenRoot(r.Config.HostPath)
	if err != nil {
		return err
	}
	defer r.client.WarnError(root.Close, "Failure during close")

	return fs.WalkDir(root.FS(), ".", func(rel string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}

		args := instanceFileArgs{
			Mode: 0o644,
			UID:  uid,
			GID:  gid,
		}

		if d.IsDir() {
			args.Mode = 0o755
			args.Type = "directory"

			return sftpCreateFile(r.client, sftpConn, rel, args, false)
		}

		f, err := root.Open(rel)
		if err != nil {
			return err
		}

		defer r.client.WarnError(f.Close, "Failed to close a seed file")

		args.Type = "file"
		args.Content = f

		return sftpCreateFile(r.client, sftpConn, rel, args, true)
	})
}

// Delete removes the storage volume from Incus.
func (r *StorageVolume) Delete(ctx context.Context, opts ...Option) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if !r.IsEnsured() {
		r.clearState()

		r.client.resources.Remove(r)
		return nil
	}

	if err := r.get(ctx); err != nil {
		// Already gone server side
		r.client.resources.Remove(r)
		return err
	}

	options := NewOptions(opts...)

	if err := r.client.hookBefore(ctx, ActionDelete, r, options, nil); err != nil {
		r.clearState()

		r.client.resources.Remove(r)
		return err
	}

	conn, err := r.client.Connection()
	if err != nil {
		return err
	}

	err = conn.DeleteStoragePoolVolume(ctx, r.Config.Pool, "custom", r.incusName)
	err = r.client.hookAfter(ctx, ActionDelete, r, options, err)

	r.clearState()
	r.client.resources.Remove(r)

	return err
}

func (r *StorageVolume) backupName() string {
	return BackupVolumePrefix + SanitizeIncusName(r.Name(), MaxIncusNameLen-len(BackupVolumePrefix))
}

func (r *StorageVolume) BackupEntry(cfg BackupConfig, backupProject string) BackupVolume {
	return BackupVolume{
		Source: VolumeInfos{
			Project: r.client.Project(),
			Pool:    r.Config.Pool,
			Name:    r.IncusName(),
		},
		Backup: VolumeInfos{
			Project: backupProject,
			Pool:    cfg.Pool,
			Name:    r.backupName(),
		},
	}
}

func (r *StorageVolume) Backup(ctx context.Context, opts ...Option) error {
	if !r.IsEnsured() {
		return ErrNotEnsured
	}

	options := NewOptions(opts...)
	if options.BackupClient == nil {
		return ErrUnsupportedAction.WithText("backup without OptionBackup doesn't work")
	}

	err := r.client.hookBefore(ctx, ActionBackup, r, options, nil)
	if err != nil {
		return err
	}

	bc := options.BackupClient
	if options.BackupConfig.Pool == "" {
		options.BackupConfig.Pool = bc.Config().DefaultStoragePool
	}

	lockName := SanitizeIncusName(r.IncusName(), MaxIncusNameLen-5) + ".lock"
	bMVol, err := BackupManifestVolume(ctx, bc, options.BackupConfig)
	if err != nil {
		return r.client.hookAfter(ctx, ActionBackup, r, options, err)
	}

	sc, err := bMVol.SFTP(ctx)
	if err != nil {
		return r.client.hookAfter(ctx, ActionBackup, r, options, err)
	}

	lock, err := BackupLock(ctx, bc, sc, options.BackupConfig, 5*time.Minute, lockName)
	if err != nil {
		bc.WarnError(sc.Close, "Failed to close a backup lock sFTP connection")
		return r.client.hookAfter(ctx, ActionBackup, r, options, err)
	}
	defer func() {
		r.client.WarnError(lock.Unlock, "Failed to release a backup lock")
		r.client.WarnError(sc.Close, "Failed to close a backup lock sFTP connection")
	}()

	conn, err := options.BackupClient.Connection()
	if err != nil {
		return r.client.hookAfter(ctx, ActionBackup, r, options, ErrUnknown.Wrap(err))
	}

	backupName := r.backupName()
	volumeExists := false
	_, _, err = conn.GetStoragePoolVolume(ctx, options.BackupConfig.Pool, "custom", backupName, nil)
	if !incusApi.StatusErrorCheck(err, http.StatusNotFound) {
		volumeExists = true
	}

	source := r.State().IncusVolume
	req := incusApi.StorageVolumesPost{
		Name:        backupName,
		Type:        source.Type,
		ContentType: source.ContentType,
		Source: incusApi.StorageVolumeSource{
			Name:       source.Name,
			Type:       "copy",
			Pool:       r.Config.Pool,
			VolumeOnly: true,
			Refresh:    volumeExists,
		},
	}
	req.Config = source.Config
	req.Description = source.Description

	if r.client.Project() != bc.Project() {
		req.Source.Project = r.client.Project()
	}

	// Cluster-internal copies must name the member the source volume is on.
	if source.Location != "" && source.Location != "none" {
		req.Source.Location = source.Location
	}

	copyOp, err := conn.CopyStoragePoolVolume(ctx, options.BackupConfig.Pool, req)
	err = r.client.hookOperation(ctx, ActionBackup, r, options, copyOp, err)
	if err != nil {
		return r.client.hookAfter(ctx, ActionBackup, r, options, ErrBackupFailed.WithText("copy").Wrap(err))
	}

	snap := incusApi.StorageVolumeSnapshotsPost{
		Name: options.BackupConfig.Timestamp,
	}

	snapOp, err := conn.CreateStoragePoolVolumeSnapshot(ctx, options.BackupConfig.Pool, source.Type, backupName, snap)
	err = r.client.hookOperation(ctx, ActionBackup, r, options, snapOp, err)
	if err != nil {
		return r.client.hookAfter(ctx, ActionBackup, r, options, ErrBackupFailed.WithText("snapshot").Wrap(err))
	}

	return r.client.hookAfter(ctx, ActionBackup, r, options, nil)
}

var (
	_ Resource   = (*StorageVolume)(nil)
	_ EnsureAble = (*StorageVolume)(nil)
	_ StartAble  = (*StorageVolume)(nil)
	_ DeleteAble = (*StorageVolume)(nil)
	_ BackupAble = (*StorageVolume)(nil)
)
