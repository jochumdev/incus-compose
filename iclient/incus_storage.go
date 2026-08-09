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

// GetStoragePoolNames returns the names of every storage pool.
func (c *Connection) GetStoragePoolNames(ctx context.Context) ([]string, error) {
	uris := []string{}

	_, err := c.getStruct(ctx, incusStoragePoolsPath, nil, &uris)
	if err != nil {
		return nil, err
	}

	return resourceNames(incusStoragePoolsPath, uris)
}

// GetStoragePoolVolume returns one custom volume and its ETag. Without
// args.Full the snapshots and backups are zero.
func (c *Connection) GetStoragePoolVolume(ctx context.Context, pool string, volType string, name string, args *GetStoragePoolVolumeArgs) (*api.StorageVolumeFull, string, error) {
	if args == nil {
		args = &GetStoragePoolVolumeArgs{}
	}

	var query url.Values

	if args.Full {
		query = url.Values{}
		query.Set("recursion", "1")
	}

	volume := api.StorageVolumeFull{}

	etag, err := c.getStruct(ctx, incusVolumesPath(pool, volType, name), query, &volume)
	if err != nil {
		return nil, "", err
	}

	return &volume, etag, nil
}

// CreateStoragePoolVolume adds a volume to a pool.
func (c *Connection) CreateStoragePoolVolume(ctx context.Context, pool string, volume api.StorageVolumesPost) error {
	_, _, err := c.do(ctx, http.MethodPost, incusVolumesPath(pool, volume.Type), nil, volume, "")

	return err
}

// DeleteStoragePoolVolume removes a volume from a pool.
func (c *Connection) DeleteStoragePoolVolume(ctx context.Context, pool string, volType string, name string) error {
	_, _, err := c.do(ctx, http.MethodDelete, incusVolumesPath(pool, volType, name), nil, nil, "")

	return err
}
