package iclient

import (
	"context"
	"net/http"
	"net/url"

	"github.com/lxc/incus/v7/shared/api"
)

// incusStoragePoolsPath is the collection every storage call hangs off.
const incusStoragePoolsPath = "/storage-pools"

// incusVolumesPath is the volume collection of one pool and type.
func incusVolumesPath(pool string, volType string, suffix ...string) string {
	path := incusStoragePoolsPath + "/" + url.PathEscape(pool) + "/volumes"

	if volType != "" {
		path += "/" + url.PathEscape(volType)
	}

	for _, part := range suffix {
		path += "/" + url.PathEscape(part)
	}

	return path
}

// GetStoragePool returns one storage pool and its ETag. Pools are server-wide,
// so this takes no project.
func (c *Connection) GetStoragePool(ctx context.Context, name string) (*api.StoragePool, string, error) {
	pool := api.StoragePool{}

	etag, err := c.getStruct(ctx, "", incusStoragePoolsPath+"/"+url.PathEscape(name), nil, &pool)
	if err != nil {
		return nil, "", err
	}

	return &pool, etag, nil
}

// GetStoragePoolNames returns the names of every storage pool. Pools are
// server-wide, so this takes no project.
func (c *Connection) GetStoragePoolNames(ctx context.Context) ([]string, error) {
	uris := []string{}

	_, err := c.getStruct(ctx, "", incusStoragePoolsPath, nil, &uris)
	if err != nil {
		return nil, err
	}

	return resourceNames(incusStoragePoolsPath, uris)
}

// GetStoragePoolVolume returns one custom volume and its ETag. Without
// args.Full the snapshots and backups are zero.
func (c *Connection) GetStoragePoolVolume(ctx context.Context, project string, pool string, volType string, name string, args *GetStoragePoolVolumeArgs) (*api.StorageVolumeFull, string, error) {
	if args == nil {
		args = &GetStoragePoolVolumeArgs{}
	}

	var query url.Values

	if args.Full || args.Target != "" {
		query = url.Values{}

		if args.Full {
			query.Set("recursion", "1")
		}

		if args.Target != "" {
			query.Set("target", args.Target)
		}
	}

	volume := api.StorageVolumeFull{}

	etag, err := c.getStruct(ctx, project, incusVolumesPath(pool, volType, name), query, &volume)
	if err != nil {
		return nil, "", err
	}

	return &volume, etag, nil
}

// CreateStoragePoolVolume adds a volume to a pool.
func (c *Connection) CreateStoragePoolVolume(ctx context.Context, project string, pool string, volume api.StorageVolumesPost, args *CreateStoragePoolVolumeArgs) error {
	var query url.Values

	if args != nil && args.Target != "" {
		query = url.Values{}
		query.Set("target", args.Target)
	}

	_, _, err := c.do(ctx, project, http.MethodPost, incusVolumesPath(pool, volume.Type), query, volume, "")

	return err
}

// CopyStoragePoolVolume copies a volume into the pool and follows the operation.
// The volume's Source names the pool, type and project the copy comes from.
func (c *Connection) CopyStoragePoolVolume(ctx context.Context, project string, pool string, volume api.StorageVolumesPost) (<-chan api.Operation, error) {
	return c.asyncOperation(ctx, project, http.MethodPost, incusVolumesPath(pool, volume.Type), nil, volume, "")
}

// GetStoragePoolVolumeSnapshotNames returns a volume's snapshot names.
func (c *Connection) GetStoragePoolVolumeSnapshotNames(ctx context.Context, project string, pool string, volType string, name string) ([]string, error) {
	uris := []string{}

	collection := incusVolumesPath(pool, volType, name) + "/snapshots"
	_, err := c.getStruct(ctx, project, collection, nil, &uris)
	if err != nil {
		return nil, err
	}

	return resourceNames(collection, uris)
}

// CreateStoragePoolVolumeSnapshot snapshots a volume and follows the operation.
func (c *Connection) CreateStoragePoolVolumeSnapshot(ctx context.Context, project string, pool string, volType string, name string, snap api.StorageVolumeSnapshotsPost) (<-chan api.Operation, error) {
	return c.asyncOperation(ctx, project, http.MethodPost, incusVolumesPath(pool, volType, name)+"/snapshots", nil, snap, "")
}

// DeleteStoragePoolVolumeSnapshot removes a volume snapshot and follows the operation.
func (c *Connection) DeleteStoragePoolVolumeSnapshot(ctx context.Context, project string, pool string, volType string, name string, snap string) (<-chan api.Operation, error) {
	return c.asyncOperation(ctx, project, http.MethodDelete, incusVolumesPath(pool, volType, name)+"/snapshots/"+url.PathEscape(snap), nil, nil, "")
}

// GetStoragePoolVolumeState returns a volume's live state, which carries its usage.
func (c *Connection) GetStoragePoolVolumeState(ctx context.Context, project string, pool string, volType string, name string) (*api.StorageVolumeState, error) {
	state := api.StorageVolumeState{}

	_, err := c.getStruct(ctx, project, incusVolumesPath(pool, volType, name)+"/state", nil, &state)
	if err != nil {
		return nil, err
	}

	return &state, nil
}

// DeleteStoragePoolVolume removes a volume from a pool.
func (c *Connection) DeleteStoragePoolVolume(ctx context.Context, project string, pool string, volType string, name string, args *DeleteStoragePoolVolumeArgs) error {
	var query url.Values

	if args != nil && args.Target != "" {
		query = url.Values{}
		query.Set("target", args.Target)
	}

	_, _, err := c.do(ctx, project, http.MethodDelete, incusVolumesPath(pool, volType, name), query, nil, "")

	return err
}
