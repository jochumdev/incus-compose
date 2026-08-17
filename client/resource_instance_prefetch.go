package client

import (
	"context"
	"path"
	"strings"

	"github.com/gosimple/slug"
)

// PrefetchVolumeServiceKey holds the service a volume belongs to, spelled as
// the instances spell their own labels.
const PrefetchVolumeServiceKey = "user.label.incus-compose.service"

// prefetchVolumeDevice is the device name a path's own volume is attached as.
func prefetchVolumeDevice(at string) string {
	return "imgvol-" + slug.Make(at)
}

// prefetchVolumeName is what a declared path's volume is called, which is
// derived rather than stored so an instance can be read back without its image.
func prefetchVolumeName(service string, at string) string {
	return "auto-" + service + "-" + slug.Make(at)
}

// prefetchVolumes attaches a volume to every path the image declares that no
// device already covers, so what an application writes there survives a stop.
func (r *Instance) prefetchVolumes(ctx context.Context, image *Image, uid uint64, gid uint64) error {
	if r.Config.NoAutoVolumes {
		return nil
	}

	for _, at := range image.State().Volumes {
		at = path.Clean(at)

		if at == "/" || !path.IsAbs(at) {
			r.client.LogWarn("Not giving a volume to a path that is not one", "resource", r, "path", at)

			continue
		}

		if r.devicePath(at) != "" {
			continue
		}

		volConfig := &StorageVolumeConfig{
			Shifted:       true,
			ImageResource: image,
			UID:           uid,
			GID:           gid,
			Pool:          r.client.Config().DefaultStoragePool,
			AlwaysHash:    true,
			Prefetch:      at,
			Extensions:    map[string]string{PrefetchVolumeServiceKey: r.Config.ServiceName},
		}

		res, err := r.client.Resource(KindStorageVolume, prefetchVolumeName(r.Config.ServiceName, at), volConfig)
		if err != nil {
			return err
		}

		vol, ok := res.(*StorageVolume)
		if !ok {
			return ErrUnknownResource.WithResource(res)
		}

		err = RunAction(ctx, vol, ActionEnsure, OptionCreate())
		if err != nil {
			return err
		}

		r.Config.Devices = append(r.Config.Devices, InstanceDevice{
			Name: prefetchVolumeDevice(at),
			Config: InstanceDeviceConfig{
				DeviceType: InstanceDeviceTypeDisk,
				Disk: InstanceDeviceDiskConfig{
					StorageVolumeConfig: volConfig,
					Source:              vol.IncusName(),
					Path:                at,
					Shift:               true,
				},
			},
		})
	}

	return nil
}

// devicePath returns the mount a device already has at or around at, empty when
// there is none. Disk devices are not nested inside each other.
func (r *Instance) devicePath(at string) string {
	for _, dev := range r.Config.Devices {
		mount := dev.Config.Disk.Path
		if dev.Config.DeviceType == InstanceDeviceTypeTmpfs {
			mount = dev.Config.Tmpfs.Path
		}

		if coversPath(mount, at) || coversPath(at, mount) {
			return mount
		}
	}

	for _, dev := range r.Config.ExtraDevices {
		if dev["type"] != "disk" {
			continue
		}

		if coversPath(dev["path"], at) || coversPath(at, dev["path"]) {
			return dev["path"]
		}
	}

	return ""
}

// prefetchDevices returns the instance's own disk devices for declared paths.
func (r *Instance) prefetchDevices() []map[string]string {
	info := r.State().IncusInstance
	if info == nil {
		return nil
	}

	found := []map[string]string{}

	for name, dev := range info.Devices {
		if !strings.HasPrefix(name, "imgvol-") || dev["type"] != "disk" || dev["path"] == "" || dev["source"] == "" {
			continue
		}

		found = append(found, dev)
	}

	return found
}
