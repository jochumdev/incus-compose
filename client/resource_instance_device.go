package client

import (
	"errors"
	"fmt"
	"maps"
	"net"
)

// Device type constants.
const (
	InstanceDeviceTypeProxy = "proxy"
	InstanceDeviceTypeDisk  = "disk"
	InstanceDeviceTypeNic   = "nic"
	InstanceDeviceTypeTmpfs = "tmpfs"
)

// InstanceDeviceProxyConfig configures a proxy device for port forwarding.
type InstanceDeviceProxyConfig struct {
	// ListenType is the protocol type for the listen side (e.g., "tcp").
	ListenType string

	// ListenAddr is the address to listen on.
	ListenAddr string

	// ListenPort is the port to listen on.
	ListenPort uint32

	// ConnectType is the protocol type for the connect side (e.g., "tcp").
	ConnectType string

	// ConnectAddr is the address to connect to.
	ConnectAddr string

	// ConnectPort is the port to connect to.
	ConnectPort uint32

	// Nat enables NAT mode for the proxy.
	Nat bool
}

// InstanceDeviceDiskConfig configures a disk device (volume or bind mount).
type InstanceDeviceDiskConfig struct {
	StorageVolumeConfig *StorageVolumeConfig

	// Source is the volume name or host path.
	Source string

	// Path is the mount point inside the instance.
	Path string

	// Shift enables UID/GID shifting for the mount.
	Shift bool

	// ReadOnly makes the mount read-only.
	ReadOnly bool
}

// InstanceDeviceTmpfsConfig configures a tmpfs device.
type InstanceDeviceTmpfsConfig struct {
	// Path is the mount point inside the instance.
	Path string

	// Size is the optional size limit in bytes.
	Size string
}

// InstanceDeviceConfig configures an instance device.
type InstanceDeviceConfig struct {
	// DeviceType identifies the device type (nic, disk, proxy, tmpfs).
	DeviceType string

	// Network is the network resource for nic devices (compose-managed).
	Network Resource

	// NetworkName is a raw Incus network name for nic devices that reference an
	// existing bridge directly, without a compose-managed Resource.
	// Only one of Network or NetworkName should be set.
	NetworkName string

	// Proxy contains proxy device configuration.
	Proxy InstanceDeviceProxyConfig

	// Disk contains disk device configuration.
	Disk InstanceDeviceDiskConfig

	// Tmpfs contains tmpfs device configuration.
	Tmpfs InstanceDeviceTmpfsConfig

	// Extensions contains direct device configuration options. For a raw device
	// (unknown DeviceType) it holds the entire device config; for a typed device
	// it holds extra keys merged over the generated config.
	Extensions map[string]string
}

// InstanceDevice represents an instance device configuration.
type InstanceDevice struct {
	// Name is the device name.
	Name string

	// Config holds the device configuration.
	Config InstanceDeviceConfig
}

// ToIncusDevice converts the device to Incus API format.
// Returns the device name and configuration map.
func (d *InstanceDevice) ToIncusDevice() (string, map[string]string, error) {
	var (
		devConfig map[string]string
		err       error
	)

	switch d.Config.DeviceType {
	case InstanceDeviceTypeNic:
		devConfig, err = d.toNicDevice()
	case InstanceDeviceTypeProxy:
		devConfig, err = d.toProxyDevice()
	case InstanceDeviceTypeDisk:
		devConfig, err = d.toDiskDevice()
	case InstanceDeviceTypeTmpfs:
		devConfig, err = d.toTmpfsDevice()
	default:
		if d.Config.Extensions == nil {
			err = errors.New("device config not given")
		} else {
			devConfig = maps.Clone(d.Config.Extensions)
			devConfig["type"] = d.Config.DeviceType
		}
	}

	if err != nil {
		return "", nil, ErrBadDeviceConfig.WithText(d.Name).Wrap(err)
	}

	return d.Name, devConfig, nil
}

func (d *InstanceDevice) toNicDevice() (map[string]string, error) {
	networkName := ""
	if d.Config.Network != nil {
		networkName = d.Config.Network.IncusName()
	} else if d.Config.NetworkName != "" {
		networkName = d.Config.NetworkName
	} else if _, hasNicType := d.Config.Extensions["nictype"]; !hasNicType {
		// A managed `network:` isn't the only valid way to declare a NIC -- Incus itself accepts
		// a device carrying `nictype` (e.g. "bridged" + "parent") with no `network` key at all,
		// which is the only way to attach to an unmanaged host bridge (MANAGED=NO in `incus
		// network list`). Only reject when neither a managed network nor nictype is present.
		return map[string]string{}, ErrBadDeviceConfig.WithText("network not given")
	}

	device := map[string]string{
		"type": "nic",
		"name": d.Name,
	}
	if networkName != "" {
		device["network"] = networkName
	}

	maps.Copy(device, d.Config.Extensions)

	return device, nil
}

// proxyEndpoint formats a host address and port for Incus proxy device notation.
// IPv6 addresses are bracketed: [addr]:port; IPv4/hostname use addr:port.
func proxyEndpoint(addr string, port uint32) string {
	if net.ParseIP(addr).To4() == nil && net.ParseIP(addr) != nil {
		return fmt.Sprintf("[%s]:%d", addr, port)
	}
	return fmt.Sprintf("%s:%d", addr, port)
}

func (d *InstanceDevice) toProxyDevice() (map[string]string, error) { //nolint:unparam // signature matches the other toXDevice methods dispatched from ToIncusDevice
	cfg := d.Config.Proxy

	device := map[string]string{
		"type":    "proxy",
		"listen":  fmt.Sprintf("%s:%s", cfg.ListenType, proxyEndpoint(cfg.ListenAddr, cfg.ListenPort)),
		"connect": fmt.Sprintf("%s:%s", cfg.ConnectType, proxyEndpoint(cfg.ConnectAddr, cfg.ConnectPort)),
	}

	if cfg.Nat {
		device["nat"] = "true"
	}

	maps.Copy(device, d.Config.Extensions)

	return device, nil
}

func (d *InstanceDevice) toDiskDevice() (map[string]string, error) { //nolint:unparam // signature matches the other toXDevice methods dispatched from ToIncusDevice
	cfg := d.Config.Disk

	device := map[string]string{
		"type":   "disk",
		"source": cfg.Source,
		"path":   cfg.Path,
	}

	if cfg.StorageVolumeConfig != nil {
		// Storage volume - pool is required, shift is set on volume not device
		device["pool"] = cfg.StorageVolumeConfig.Pool
	} else {
		// Bind mount - shift is set on device
		if cfg.Shift {
			device["shift"] = "true"
		}
	}

	if cfg.ReadOnly {
		device["readonly"] = "true"
	}

	maps.Copy(device, d.Config.Extensions)

	return device, nil
}

func (d *InstanceDevice) toTmpfsDevice() (map[string]string, error) { //nolint:unparam // signature matches the other toXDevice methods dispatched from ToIncusDevice
	cfg := d.Config.Tmpfs

	device := map[string]string{
		"type":   "disk",
		"path":   cfg.Path,
		"source": "tmpfs:",
	}

	if cfg.Size != "" {
		device["size"] = cfg.Size
	}

	maps.Copy(device, d.Config.Extensions)

	return device, nil
}
