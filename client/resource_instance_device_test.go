package client

import (
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/bradleyjkemp/cupaloy/v2"
	"github.com/stretchr/testify/require"
)

var deviceSnapshotter = cupaloy.New(cupaloy.SnapshotSubdirectory(filepath.Join("..", "test", "snapshots", "client")))

// DeviceTestCase represents a single device test case.
type DeviceTestCase struct {
	Name   string
	Device InstanceDevice
}

func runDeviceTest(t *testing.T, tc DeviceTestCase) {
	t.Helper()
	t.Run(tc.Name, func(t *testing.T) {
		t.Parallel()
		name, config, err := tc.Device.ToIncusDevice()
		require.Nil(t, err)

		result := map[string]any{
			"name":   name,
			"config": config,
		}

		output, jsonErr := json.MarshalIndent(result, "", "  ")
		require.NoError(t, jsonErr)

		deviceSnapshotter.SnapshotT(t, string(output))
	})
}

func TestProxyDevices(t *testing.T) {
	t.Parallel()

	testCases := []DeviceTestCase{
		{
			Name: "proxy_basic_tcp",
			Device: InstanceDevice{
				Name: "proxy-8080",
				Config: InstanceDeviceConfig{
					DeviceType: InstanceDeviceTypeProxy,
					Proxy: InstanceDeviceProxyConfig{
						ListenType:  "tcp",
						ListenAddr:  "0.0.0.0",
						ListenPort:  8080,
						ConnectType: "tcp",
						ConnectAddr: "127.0.0.1",
						ConnectPort: 80,
					},
				},
			},
		},
		{
			Name: "proxy_udp",
			Device: InstanceDevice{
				Name: "proxy-5353",
				Config: InstanceDeviceConfig{
					DeviceType: InstanceDeviceTypeProxy,
					Proxy: InstanceDeviceProxyConfig{
						ListenType:  "udp",
						ListenAddr:  "0.0.0.0",
						ListenPort:  5353,
						ConnectType: "udp",
						ConnectAddr: "127.0.0.1",
						ConnectPort: 53,
					},
				},
			},
		},
		{
			Name: "proxy_custom_listen_addr",
			Device: InstanceDevice{
				Name: "proxy-3000",
				Config: InstanceDeviceConfig{
					DeviceType: InstanceDeviceTypeProxy,
					Proxy: InstanceDeviceProxyConfig{
						ListenType:  "tcp",
						ListenAddr:  "192.168.1.100",
						ListenPort:  3000,
						ConnectType: "tcp",
						ConnectAddr: "127.0.0.1",
						ConnectPort: 3000,
					},
				},
			},
		},
		{
			Name: "proxy_with_nat",
			Device: InstanceDevice{
				Name: "proxy-443",
				Config: InstanceDeviceConfig{
					DeviceType: InstanceDeviceTypeProxy,
					Proxy: InstanceDeviceProxyConfig{
						ListenType:  "tcp",
						ListenAddr:  "0.0.0.0",
						ListenPort:  443,
						ConnectType: "tcp",
						ConnectAddr: "127.0.0.1",
						ConnectPort: 8443,
						Nat:         true,
					},
				},
			},
		},
		{
			Name: "proxy_ipv6",
			Device: InstanceDevice{
				Name: "proxy-8080",
				Config: InstanceDeviceConfig{
					DeviceType: InstanceDeviceTypeProxy,
					Proxy: InstanceDeviceProxyConfig{
						ListenType:  "tcp",
						ListenAddr:  "2a0a:51c4:7:7d5b::",
						ListenPort:  8080,
						ConnectType: "tcp",
						ConnectAddr: "fd42:ab37:29f:47d7::2",
						ConnectPort: 80,
						Nat:         true,
					},
				},
			},
		},
		{
			Name: "proxy_high_port",
			Device: InstanceDevice{
				Name: "proxy-65535",
				Config: InstanceDeviceConfig{
					DeviceType: InstanceDeviceTypeProxy,
					Proxy: InstanceDeviceProxyConfig{
						ListenType:  "tcp",
						ListenAddr:  "0.0.0.0",
						ListenPort:  65535,
						ConnectType: "tcp",
						ConnectAddr: "127.0.0.1",
						ConnectPort: 65535,
					},
				},
			},
		},
		{
			Name: "proxy_port_zero",
			Device: InstanceDevice{
				Name: "proxy-0",
				Config: InstanceDeviceConfig{
					DeviceType: InstanceDeviceTypeProxy,
					Proxy: InstanceDeviceProxyConfig{
						ListenType:  "tcp",
						ListenAddr:  "0.0.0.0",
						ListenPort:  0,
						ConnectType: "tcp",
						ConnectAddr: "127.0.0.1",
						ConnectPort: 0,
					},
				},
			},
		},
	}

	for _, tc := range testCases {
		runDeviceTest(t, tc)
	}
}

func TestDiskDevices(t *testing.T) {
	t.Parallel()

	testCases := []DeviceTestCase{
		{
			Name: "disk_volume",
			Device: InstanceDevice{
				Name: "vol-data",
				Config: InstanceDeviceConfig{
					DeviceType: InstanceDeviceTypeDisk,
					Disk: InstanceDeviceDiskConfig{
						StorageVolumeConfig: &StorageVolumeConfig{Pool: "default"},
						Source:              "my-volume",
						Path:                "/data",
					},
				},
			},
		},
		{
			Name: "disk_bind_mount",
			Device: InstanceDevice{
				Name: "bind-config",
				Config: InstanceDeviceConfig{
					DeviceType: InstanceDeviceTypeDisk,
					Disk: InstanceDeviceDiskConfig{
						Source: "/host/config",
						Path:   "/app/config",
						Shift:  true,
					},
				},
			},
		},
		{
			Name: "disk_readonly",
			Device: InstanceDevice{
				Name: "bind-static",
				Config: InstanceDeviceConfig{
					DeviceType: InstanceDeviceTypeDisk,
					Disk: InstanceDeviceDiskConfig{
						Source:   "/host/static",
						Path:     "/app/static",
						ReadOnly: true,
					},
				},
			},
		},
	}

	for _, tc := range testCases {
		runDeviceTest(t, tc)
	}
}

func TestNicDevices(t *testing.T) {
	t.Parallel()

	testCases := []DeviceTestCase{
		{
			Name: "nic_managed_network",
			Device: InstanceDevice{
				Name: "eth0",
				Config: InstanceDeviceConfig{
					DeviceType:  InstanceDeviceTypeNic,
					NetworkName: "incusbr0",
				},
			},
		},
		{
			// Regression test: an unmanaged host bridge (nictype + parent, no
			// managed network) must be passed through, not rejected. Incus
			// itself accepts this device shape directly.
			Name: "nic_unmanaged_bridge",
			Device: InstanceDevice{
				Name: "eth1",
				Config: InstanceDeviceConfig{
					DeviceType: InstanceDeviceTypeNic,
					Extensions: map[string]string{
						"nictype": "bridged",
						"parent":  "br0",
					},
				},
			},
		},
	}

	for _, tc := range testCases {
		runDeviceTest(t, tc)
	}
}

func TestTmpfsDevices(t *testing.T) {
	t.Parallel()

	testCases := []DeviceTestCase{
		{
			Name: "tmpfs_basic",
			Device: InstanceDevice{
				Name: "tmpfs--tmp",
				Config: InstanceDeviceConfig{
					DeviceType: InstanceDeviceTypeTmpfs,
					Tmpfs: InstanceDeviceTmpfsConfig{
						Path: "/tmp",
					},
				},
			},
		},
		{
			Name: "tmpfs_with_size",
			Device: InstanceDevice{
				Name: "tmpfs--run",
				Config: InstanceDeviceConfig{
					DeviceType: InstanceDeviceTypeTmpfs,
					Tmpfs: InstanceDeviceTmpfsConfig{
						Path: "/run",
						Size: "104857600",
					},
				},
			},
		},
	}

	for _, tc := range testCases {
		runDeviceTest(t, tc)
	}
}

func TestDeviceErrors(t *testing.T) {
	t.Parallel()

	t.Run("nic_no_network", func(t *testing.T) {
		t.Parallel()
		device := InstanceDevice{
			Name: "eth0",
			Config: InstanceDeviceConfig{
				DeviceType: InstanceDeviceTypeNic,
			},
		}
		_, _, err := device.ToIncusDevice()
		require.Error(t, err)
	})

	// The nictype exemption is narrow: an unrelated extension must not also
	// satisfy it.
	t.Run("nic_no_network_unrelated_extension", func(t *testing.T) {
		t.Parallel()
		device := InstanceDevice{
			Name: "eth0",
			Config: InstanceDeviceConfig{
				DeviceType: InstanceDeviceTypeNic,
				Extensions: map[string]string{
					"mtu": "1500",
				},
			},
		}
		_, _, err := device.ToIncusDevice()
		require.Error(t, err)
	})

	t.Run("unknown_no_extra", func(t *testing.T) {
		t.Parallel()
		device := InstanceDevice{
			Name: "custom",
			Config: InstanceDeviceConfig{
				DeviceType: "custom-type",
			},
		}
		_, _, err := device.ToIncusDevice()
		require.Error(t, err)
	})
}

// TestCustomDevice covers the raw-device path: an unknown DeviceType with its
// full config in Extensions is passed through verbatim, with type forced to
// DeviceType.
func TestCustomDevice(t *testing.T) {
	t.Parallel()

	device := InstanceDevice{
		Name: "gpu0",
		Config: InstanceDeviceConfig{
			DeviceType: "gpu",
			Extensions: map[string]string{
				"type":    "gpu",
				"gputype": "physical",
				"pci":     "0000:01:00.0",
			},
		},
	}

	name, config, err := device.ToIncusDevice()
	require.Nil(t, err)
	require.Equal(t, "gpu0", name)
	require.Equal(t, map[string]string{
		"type":    "gpu",
		"gputype": "physical",
		"pci":     "0000:01:00.0",
	}, config)
}
