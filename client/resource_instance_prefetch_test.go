package client

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// devicePath() decides whether a path an image declares as a VOLUME is already
// covered by a device, and reads Config.Disk.Path / Config.Tmpfs.Path to do it.
// An x-incus-compose device that leaves those empty is invisible to the check.
func TestDevicePathSeesExtraDevices(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name   string
		device InstanceDevice
		at     string
		want   string
	}{
		{
			name: "disk from x-incus-compose.devices covers its path",
			device: InstanceDevice{
				Name: "app-config",
				Config: InstanceDeviceConfig{
					DeviceType: InstanceDeviceTypeDisk,
					Disk:       InstanceDeviceDiskConfig{Path: "/config"},
					Extensions: map[string]string{
						"type": "disk", "pool": "default",
						"source": "web-config", "path": "/config",
					},
				},
			},
			at:   "/config",
			want: "/config",
		},
		{
			name: "a child of a covered path is also covered",
			device: InstanceDevice{
				Name: "app-data",
				Config: InstanceDeviceConfig{
					DeviceType: InstanceDeviceTypeDisk,
					Disk:       InstanceDeviceDiskConfig{Path: "/opt"},
					Extensions: map[string]string{"type": "disk", "path": "/opt"},
				},
			},
			at:   "/opt/data",
			want: "/opt",
		},
		{
			name: "tmpfs from x-incus-compose.devices covers its path",
			device: InstanceDevice{
				Name: "scratch",
				Config: InstanceDeviceConfig{
					DeviceType: InstanceDeviceTypeTmpfs,
					Tmpfs:      InstanceDeviceTmpfsConfig{Path: "/var/lib/influxdb2"},
					Extensions: map[string]string{"type": "tmpfs", "path": "/var/lib/influxdb2"},
				},
			},
			at:   "/var/lib/influxdb2",
			want: "/var/lib/influxdb2",
		},
		{
			name: "an unrelated path is still uncovered",
			device: InstanceDevice{
				Name: "app-config",
				Config: InstanceDeviceConfig{
					DeviceType: InstanceDeviceTypeDisk,
					Disk:       InstanceDeviceDiskConfig{Path: "/config"},
					Extensions: map[string]string{"type": "disk", "path": "/config"},
				},
			},
			at:   "/data",
			want: "",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := &Instance{Config: InstanceConfig{Devices: []InstanceDevice{tc.device}}}
			assert.Equal(t, tc.want, r.devicePath(tc.at))
		})
	}
}
