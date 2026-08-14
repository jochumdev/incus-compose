package project

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"math"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/compose-spec/compose-go/v2/types"
	"github.com/lxc/incus/v7/shared/util"

	"github.com/lxc/incus-compose/client"
	"github.com/lxc/incus-compose/shared"
)

// labelIncusComposePrefix is the instance config prefix for incus-compose labels.
const labelIncusComposePrefix = "user.label.incus-compose."

// healthdRestartPolicies are the restart values ic-healthd acts on, so a
// service carrying one wants watching without a healthcheck of its own.
var healthdRestartPolicies = []string{"always", "on-failure", "unless-stopped"}

func buildPlatform(service types.ServiceConfig) (string, error) {
	if service.Build == nil {
		return "", nil
	}
	if len(service.Build.Platforms) > 1 {
		return "", fmt.Errorf("build.platforms with multiple platforms is not supported")
	}
	if len(service.Build.Platforms) == 1 {
		return service.Build.Platforms[0], nil
	}
	return service.Platform, nil
}

// serviceToInstance translates a compose service to an Incus instance.
// Environment vars become instance config, labels become user metadata.
// Volumes default to bind mounts for paths starting with / or ., otherwise named volumes.
func serviceToInstance(c *client.Client, p *types.Project, serviceName string, options *ResourcesOptions, index, scale int) (*client.Instance, []client.Resource, error) {
	service, ok := p.Services[serviceName]
	if !ok {
		return nil, nil, fmt.Errorf("service %q not found", serviceName)
	}

	var errs error
	resources := []client.Resource{}

	config, err := instanceConfig(c, service, p.Name, options.marks)
	if err != nil {
		errs = errors.Join(errs, err)
	}

	image, err := instanceImage(c, service)
	if err != nil {
		errs = errors.Join(errs, err)
	}
	if image == nil {
		return nil, nil, errs
	}
	resources = append(resources, image)

	instanceName := instanceName(service, index, scale)

	devices, networks, err := instanceNetworkDevices(c, p, service, instanceName)
	if err != nil {
		errs = errors.Join(errs, err)
	}
	resources = append(resources, networks...)

	devices, err = instanceProxyDevices(c, devices, service)
	if err != nil {
		errs = errors.Join(errs, err)
	}

	var (
		uid uint64
		gid uint64
	)
	// User override - https://github.com/compose-spec/compose-spec/blob/main/05-services.md#user
	if service.User != "" {
		split := strings.Split(service.User, ":")

		uid, err = strconv.ParseUint(split[0], 10, 64)
		if err != nil {
			return nil, nil, fmt.Errorf("cannot convert service '%v' user '%v' to int: %w", service.Name, split[0], err)
		}
		config["oci.uid"] = split[0]
		if len(split) > 1 {
			gid, err = strconv.ParseUint(split[1], 10, 64)
			if err != nil {
				return nil, nil, fmt.Errorf("cannot convert service '%v' user '%v' to int: %w", service.Name, split[1], err)
			}
		}
	}

	volumes, files, volumeResources, err := instanceVolumeDevices(c, p, service, image, uid, gid)
	if err != nil {
		errs = errors.Join(errs, err)
	}
	devices = append(devices, volumes...)
	resources = append(resources, volumeResources...)

	extraDevices, err := serviceExtraDevices(service)
	if err != nil {
		errs = errors.Join(errs, err)
	}
	devices = append(devices, extraDevices...)

	secrets, err := instanceSecrets(p, service)
	if err != nil {
		errs = errors.Join(errs, err)
	}
	files = append(files, secrets...)

	configs, err := instanceConfigs(p, service)
	if err != nil {
		errs = errors.Join(errs, err)
	}
	files = append(files, configs...)

	err = checkEntrypoint(service)
	if err != nil {
		errs = errors.Join(errs, err)
	}

	if errs != nil {
		return nil, nil, errs
	}

	instCfg := &client.InstanceConfig{
		ServiceName:  service.Name,
		Image:        image.Name(),
		Full:         options.Full,
		Resources:    slices.Clone(resources),
		Extensions:   config,
		Devices:      devices,
		Files:        files,
		Dependencies: instanceDependencyWaits(p, service, options),
		Entrypoint:   service.Entrypoint,
		Command:      service.Command,
		UID:          uid,
		GID:          gid,
	}

	ir, err := c.Resource(client.KindInstance, instanceName, instCfg)
	if err != nil {
		return nil, nil, err
	}

	instance, ok := ir.(*client.Instance)
	if !ok {
		return nil, nil, client.ErrUnknown.WithKindName(client.KindInstance, instanceName)
	}

	return instance, resources, nil
}

// instanceConfig builds the Incus instance config map from a compose service.
// Environment vars become environment.* keys, labels become user.* keys, and
// restart/resource/healthcheck settings and raw x-incus options are merged in.
func instanceConfig(c *client.Client, service types.ServiceConfig, projectName string, marks map[string]string) (map[string]string, error) {
	config := make(map[string]string, len(service.Environment)+len(service.Labels))

	// Environment variables
	for key, val := range service.Environment {
		if val != nil {
			config["environment."+key] = *val
		}
	}

	// Labels as user config
	for key, val := range service.Labels {
		config["user.label."+key] = val
	}

	config[labelIncusComposePrefix+"service"] = service.Name
	config[labelIncusComposePrefix+"project"] = projectName

	// Privileged.
	if service.Privileged {
		config["security.privileged"] = "true"
	}

	// Sysctls -- verified live against a real cluster: linux.sysctl.<key>
	// applies immediately and persists across restart, on both privileged
	// and unprivileged OCI application containers.
	for key, val := range service.Sysctls {
		config["linux.sysctl."+key] = val
	}

	// Restart policy
	applyRestartPolicy(config, service.Restart)
	if service.Restart != "" {
		config[client.HealthKeyPrefix+"restart"] = service.Restart
	}

	// Resource limits
	applyResourceLimits(config, resourceLimits(service))

	// The opt-in ic-healthd requires. Set before x-incus, so a service can turn
	// it off again with `user.healthcheck.enabled: "false"`.
	if service.HealthCheck != nil || slices.Contains(healthdRestartPolicies, service.Restart) {
		config[shared.HealthEnabledKey] = "true"
	}

	if service.HealthCheck != nil {
		testB, err := json.Marshal(service.HealthCheck.Test)
		if err != nil {
			return nil, fmt.Errorf("converting service %q healthcheck test: %w", service.Name, err)
		}
		config[client.HealthKeyPrefix+"test"] = string(testB)

		if service.HealthCheck.StartPeriod != nil {
			config[client.HealthKeyPrefix+"start_period"] = service.HealthCheck.StartPeriod.String()
		}

		if service.HealthCheck.StartInterval != nil {
			config[client.HealthKeyPrefix+"start_interval"] = service.HealthCheck.StartInterval.String()
		}

		if service.HealthCheck.Interval != nil {
			config[client.HealthKeyPrefix+"interval"] = service.HealthCheck.Interval.String()
		}

		if service.HealthCheck.Retries != nil {
			config[client.HealthKeyPrefix+"retries"] = strconv.FormatUint(*service.HealthCheck.Retries, 10)
		}

		if service.HealthCheck.Timeout != nil {
			config[client.HealthKeyPrefix+"timeout"] = service.HealthCheck.Timeout.String()
		}
	}

	// DNS - https://github.com/compose-spec/compose-spec/blob/main/05-services.md#dns
	if c.Global().HasExtension(shared.Incus72Extension) {
		if len(service.DNS) > 0 {
			config["oci.dns.nameservers"] = strings.Join(service.DNS, ",")
		}
		if len(service.DNSSearch) > 0 {
			config["oci.dns.search"] = strings.Join(service.DNSSearch, ",")
		}
		if service.DomainName != "" {
			config["oci.dns.domain"] = service.DomainName
		}
	}

	// Apply x-incus extensions (raw Incus options)
	if xIncusOpts := serviceXIncusExtensions(service); len(xIncusOpts) > 0 {
		for k, v := range xIncusOpts {
			config[k] = v
		}
	}

	// After x-incus, so a service cannot drop what the caller marked it with.
	maps.Copy(config, marks)

	// Ensure the network interface is up before the container's init starts.
	// Append lxc.start.delay only if the user hasn't already set it via x-incus.
	_, ok := config["raw.lxc"]
	if !ok {
		config["raw.lxc"] = "lxc.start.delay = 1\n"
	} else {
		if !strings.Contains(config["raw.lxc"], "lxc.start.delay") {
			config["raw.lxc"] += "lxc.start.delay = 1\n"
		}
	}

	return config, nil
}

// instanceImage resolves the image resource for a service, building from a
// Dockerfile when service.Build is set, otherwise pulling service.Image.
func instanceImage(c *client.Client, service types.ServiceConfig) (client.Resource, error) {
	var errs error

	imageName := service.Image
	cfg := &client.ImageConfig{}
	if service.Build != nil {
		if imageName == "" {
			imageName = "localhost/" + service.Name
		}
		platform, err := buildPlatform(service)
		if err != nil {
			errs = errors.Join(errs, err)
		}
		// compose-go resolves build.context to an absolute path but leaves
		// build.dockerfile untouched, and the builder resolves a relative
		// --file against its own working directory instead of the context.
		dockerfile := service.Build.Dockerfile
		if dockerfile != "" && !filepath.IsAbs(dockerfile) {
			dockerfile = filepath.Join(service.Build.Context, dockerfile)
		}

		buildCfg := &client.BuildConfig{
			Context:          service.Build.Context,
			Dockerfile:       dockerfile,
			DockerfileInline: service.Build.DockerfileInline,
			Target:           service.Build.Target,
			Platform:         platform,
			NoCache:          service.Build.NoCache,
			Pull:             service.Build.Pull,
			Args:             service.Build.Args.ToMapping(),
		}
		if len(service.Build.Args) > 0 {
			buildCfg.Args = make(map[string]string, len(service.Build.Args))
			for k, v := range service.Build.Args {
				if v != nil {
					buildCfg.Args[k] = *v
				}
			}
		}
		cfg.Build = buildCfg
	}

	image, err := c.Resource(client.KindImage, imageName, cfg)
	if err != nil {
		return nil, errors.Join(errs, err)
	}

	img, ok := image.(*client.Image)
	if !ok {
		return nil, errors.Join(errs, errors.New("not an image"))
	}
	img.AddService(service.Name)

	return image, errs
}

// instanceNetworkDevices builds the NIC devices (eth0, eth1, ...) for a service's
// networks along with the network resources they reference.
func instanceNetworkDevices(c *client.Client, p *types.Project, service types.ServiceConfig, instanceName string) ([]client.InstanceDevice, []client.Resource, error) {
	var errs error
	devices := []client.InstanceDevice{}
	resources := []client.Resource{}

	ethIdx := 0
	for name, sNet := range service.Networks {
		netConfig := &client.NetworkConfig{}

		gateway4, gateway6 := "none", "none"

		networkDef, defOk := p.Networks[name]
		if defOk {
			netConfig.External = bool(networkDef.External)
			netConfig.Extensions = networkExtensions(networkDef)
			// compose-go always fills Name in, with the key for an external network
			// and {project}_{key} otherwise; anything else is a `name:` the user
			// wrote, and it beats the extension.
			defaultName := name
			if !netConfig.External {
				defaultName = p.Name + "_" + name
			}

			netConfig.OverrideName = xICInstanceNetwork(networkDef)
			if networkDef.Name != "" && networkDef.Name != defaultName {
				netConfig.OverrideName = networkDef.Name
			}

			// Incus documents "auto" for ipv4.address/ipv6.address, but it is
			// broken upstream (fix pending), so leave the key unset instead.
			// Keep treating the empty value as the auto case afterwards too,
			// for backward compatibility.
			if !networkDef.Internal {
				v, ok := netConfig.Extensions["ipv4.address"]
				if ok && v != "none" && v != "" {
					ip, _, err := net.ParseCIDR(v)
					if err != nil {
						errs = errors.Join(
							errs,
							fmt.Errorf("failed to parse the gateway IPv4 for network %q: %w", name, err),
						)
						continue
					}
					gateway4 = ip.String()
				}
				v, ok = netConfig.Extensions["ipv6.address"]
				if ok && v != "none" && v != "" {
					ip, _, err := net.ParseCIDR(v)
					if err != nil {
						errs = errors.Join(
							errs,
							fmt.Errorf("failed to parse the gateway IPv6 for network %q: %w", name, err),
						)
						continue
					}
					gateway6 = ip.String()
				}

				_, ok = netConfig.Extensions["ipv4.nat"]
				if !ok {
					netConfig.Extensions["ipv4.nat"] = "true"
				}
				_, ok = netConfig.Extensions["ipv6.nat"]
				if !ok {
					netConfig.Extensions["ipv6.nat"] = "true"
				}
			}
		}

		extensions := map[string]string{}
		userExtensions := map[string]string{}
		noGateway := false
		if sNet != nil && sNet.Extensions != nil {
			userExtensions = xIncusExtensions(sNet.Extensions)

			var ext struct {
				Internal bool  `mapstructure:"internal"`
				Gateway  *bool `mapstructure:"gateway"`
			}
			ok, err := sNet.Extensions.Get("x-incus-compose", &ext)
			if ok || err == nil && ext.Internal {
				gateway4 = "none"
				gateway6 = "none"
			}

			noGateway = ext.Gateway != nil && !*ext.Gateway
		}

		ipv4Address, ipv6Address := "", ""
		if sNet != nil {
			ipv4Address = sNet.Ipv4Address
			ipv6Address = sNet.Ipv6Address
		}

		// A gateway set on the NIC supplies what the missing network address would
		// have; `x-incus-compose.gateway: false` says the instance needs none.
		if !noGateway &&
			((ipv4Address != "" && netConfig.Extensions["ipv4.address"] == "" && userExtensions["ipv4.gateway"] == "") ||
				(ipv6Address != "" && netConfig.Extensions["ipv6.address"] == "" && userExtensions["ipv6.gateway"] == "")) {
			errs = errors.Join(
				errs,
				fmt.Errorf(
					"service %q: cannot assign a static IP on network %q with no address - the gateway isn't known until the network is created; set an explicit CIDR on the network instead",
					service.Name, name,
				),
			)
			continue
		}

		if ((ipv4Address != "" && gateway4 == "none") || (ipv6Address != "" && gateway6 == "none")) &&
			!c.Global().HasExtension(shared.Incus73Extension) {
			c.LogWarn(
				"For `gateway=none` on a network you need at least incus 7.3 or 7.0.2 LTS",
				"service", service.Name,
				"network", name,
			)
			continue
		}

		if ipv4Address != "" {
			extensions["ipv4.address"] = ipv4Address
			extensions["ipv4.gateway"] = gateway4
		}

		if ipv6Address != "" {
			extensions["ipv6.address"] = ipv6Address
			extensions["ipv6.gateway"] = gateway6
		}

		// Whatever we set before, `x-incus` overrides it.
		maps.Copy(extensions, userExtensions)

		rNetwork, err := c.Resource(client.KindNetwork, name, netConfig)
		if err != nil {
			errs = errors.Join(errs, err)
			continue
		}

		if sNet != nil && len(sNet.Aliases) > 0 {
			network, ok := rNetwork.(*client.Network)
			if !ok {
				errs = errors.Join(
					errs,
					fmt.Errorf("failed to get network: %q", name),
				)
				continue
			}

			network.CNames[instanceName] = sNet.Aliases
		}

		nicConfig := client.InstanceDeviceConfig{
			DeviceType: client.InstanceDeviceTypeNic,
			Network:    rNetwork,
			Extensions: extensions,
		}

		devices = append(devices, client.InstanceDevice{
			Name:   fmt.Sprintf("eth%d", ethIdx),
			Config: nicConfig,
		})
		ethIdx++

		resources = append(resources, rNetwork)
	}

	return devices, resources, errs
}

// instanceProxyDevices builds proxy devices for every published port.
func instanceProxyDevices(c *client.Client, devices []client.InstanceDevice, service types.ServiceConfig) ([]client.InstanceDevice, error) {
	var errs error

	var connectAddr string
	for _, dev := range devices {
		if dev.Config.DeviceType != client.InstanceDeviceTypeNic {
			continue
		}
		if dev.Config.Extensions != nil {
			if addr, ok := dev.Config.Extensions["ipv4.address"]; ok {
				connectAddr, _, _ = strings.Cut(addr, "/")
				break
			}
		}
	}

	for _, port := range service.Ports {
		nat := false
		if port.Extensions != nil {
			var ext struct {
				Nat bool `mapstructure:"nat"`
			}
			ok, err := port.Extensions.Get("x-incus-compose", &ext)
			if ok && err == nil && ext.Nat {
				nat = true
			}
		}

		if nat && connectAddr == "" {
			if !c.Global().HasExtension(shared.Incus72Extension) {
				c.LogWarn("For nat you need at least incus 7.2 or 7.0.1 LTS",
					"service", service.Name,
					"port", port.Published,
				)
				continue
			}
			connectAddr = "0.0.0.0"
		} else if nat {
			if !c.Global().HasExtension(shared.Incus73Extension) {
				c.LogWarn(
					"For nat with a static ip you need at least incus 7.2 or 7.0.1 LTS",
					"service", service.Name,
					"port", port.Published,
				)
				continue
			}
		} else {
			connectAddr = "127.0.0.1"
		}

		lPort, err := strconv.ParseUint(port.Published, 10, 32)
		if err != nil {
			errs = errors.Join(errs, fmt.Errorf("bad publishing port %q must be a number: %w", port.Published, err))
			continue
		}

		proto := port.Protocol
		if proto == "" {
			proto = "tcp"
		}

		listenIP := port.HostIP
		if listenIP == "" {
			listenIP = "0.0.0.0"
		}

		devices = append(devices, client.InstanceDevice{
			Name: fmt.Sprintf("proxy-%d", lPort),
			Config: client.InstanceDeviceConfig{
				DeviceType: client.InstanceDeviceTypeProxy,
				Proxy: client.InstanceDeviceProxyConfig{
					ListenType:  proto,
					ListenAddr:  listenIP,
					ListenPort:  uint32(lPort),
					ConnectType: proto,
					ConnectAddr: connectAddr,
					ConnectPort: port.Target,
					Nat:         nat,
				},
			},
		})
	}

	return devices, errs
}

// instanceVolumeDevices builds disk, bind, and tmpfs devices for a service's
// volumes plus the shm_size tmpfs. It returns any storage volume resources
// and the files map for single-file binds.
func instanceVolumeDevices(c *client.Client, p *types.Project, service types.ServiceConfig, image client.Resource, uid, gid uint64) ([]client.InstanceDevice, []client.InstanceFile, []client.Resource, error) {
	var errs error
	devices := []client.InstanceDevice{}
	resources := []client.Resource{}
	files := []client.InstanceFile{}

	for _, cVol := range service.Volumes {
		seed := false
		if cVol.Extensions != nil {
			var ext struct {
				Seed bool `mapstructure:"seed"`
			}
			ok, err := cVol.Extensions.Get("x-incus-compose", &ext)
			if err != nil {
				return nil, nil, nil, err
			}

			if ok {
				seed = ext.Seed
			}
		}

		if cVol.Type == "" {
			// Infer type from source path (short syntax compatibility)
			// Absolute or relative paths are bind mounts, named sources are volumes
			if cVol.Source != "" && (strings.HasPrefix(cVol.Source, "/") || strings.HasPrefix(cVol.Source, ".")) {
				cVol.Type = "bind"
			} else if cVol.Source != "" {
				cVol.Type = "volume"
			}
		}

		extensions := xIncusExtensions(p.Volumes[cVol.Source].Extensions)

		// Inline x-incus on the volume entry takes precedence over the named
		// volume definition (this is the only place binds can set it).
		for k, v := range xIncusExtensions(cVol.Extensions) {
			if extensions == nil {
				extensions = map[string]string{}
			}
			extensions[k] = v
		}

		shifted := true
		es, ok := extensions["security.shifted"]
		if ok && !util.IsTrue(es) {
			shifted = false
		}

		switch cVol.Type {
		case "volume":
			volDef := p.Volumes[cVol.Source]

			pool := volumeXIncusComposePool(volDef)
			if pool == "" {
				pool = c.Config().DefaultStoragePool
			}

			volConfig := &client.StorageVolumeConfig{
				Shifted:       shifted,
				ImageResource: image,
				UID:           uid,
				GID:           gid,
				Pool:          pool,
				Extensions:    extensions,
			}

			v, err := c.Resource(client.KindStorageVolume, cVol.Source, volConfig)
			if err != nil {
				errs = errors.Join(errs, err)
				continue
			}

			resources = append(resources, v)

			devName := "vol-" + client.SanitizeIncusName(cVol.Source, client.MaxIncusNameLen-4)
			devConfig := client.InstanceDeviceConfig{
				DeviceType: client.InstanceDeviceTypeDisk,
				Disk: client.InstanceDeviceDiskConfig{
					StorageVolumeConfig: volConfig,
					Source:              v.IncusName(),
					Path:                cVol.Target,
					Shift:               shifted,
				},
			}

			if cVol.ReadOnly {
				devConfig.Disk.ReadOnly = true
			}

			devices = append(devices, client.InstanceDevice{Name: devName, Config: devConfig})
		case "bind":
			if seed {
				c.LogDebug("Will seed", "service", service.Name, "source", cVol.Source, "target", cVol.Target)

				info, err := os.Stat(cVol.Source)
				if err != nil {
					return nil, nil, nil, client.ErrUnknown.WithKindName(client.KindInstance, service.Name).Wrap(err)
				}

				if !info.IsDir() {
					files = append(files, client.InstanceFile{
						Target:    cVol.Target,
						File:      cVol.Source,
						UID:       -1,
						GID:       -1,
						Mode:      0o644,
						DirMode:   0o755,
						Overwrite: true,
					})
				} else {
					devName := "vol-seed-" + client.SanitizeIncusName(cVol.Source, client.MaxIncusNameLen-10)

					volConfig := &client.StorageVolumeConfig{
						Shifted:       shifted,
						ImageResource: image,
						UID:           uid,
						GID:           gid,
						HostPath:      cVol.Source,
						Pool:          c.Config().DefaultStoragePool,
					}

					v, err := c.Resource(client.KindStorageVolume, "bind-"+cVol.Source, volConfig)
					if err != nil {
						errs = errors.Join(errs, err)
						continue
					}

					resources = append(resources, v)

					devConfig := client.InstanceDeviceConfig{
						DeviceType: client.InstanceDeviceTypeDisk,
						Disk: client.InstanceDeviceDiskConfig{
							StorageVolumeConfig: volConfig,
							Source:              v.IncusName(),
							Path:                cVol.Target,
							Shift:               shifted,
						},
					}

					if cVol.ReadOnly {
						devConfig.Disk.ReadOnly = true
					}

					devices = append(devices, client.InstanceDevice{Name: devName, Config: devConfig})
				}
			} else {
				// Refuse bind without seed on remote hosts.
				err := c.Global().SameHost()
				if err != nil {
					return nil, nil, nil, fmt.Errorf("failed to add a bind-mount for service %v: %w", service.Name, err)
				}

				_, err = os.Stat(cVol.Source)
				if err != nil {
					return nil, nil, nil, client.ErrUnknown.WithKindName(client.KindInstance, service.Name).Wrap(err)
				}

				devName := "bind-" + client.SanitizeIncusName(cVol.Source, client.MaxIncusNameLen-5)

				devConfig := client.InstanceDeviceConfig{
					DeviceType: client.InstanceDeviceTypeDisk,
					Disk: client.InstanceDeviceDiskConfig{
						Source: cVol.Source,
						Path:   cVol.Target,
						Shift:  shifted,
					},
				}

				if cVol.ReadOnly {
					devConfig.Disk.ReadOnly = true
				}

				devices = append(devices, client.InstanceDevice{Name: devName, Config: devConfig})
			}
		case "tmpfs":
			devName := fmt.Sprintf("tmpfs-%s", strings.ReplaceAll(cVol.Target, "/", "-"))
			devConfig := client.InstanceDeviceConfig{
				DeviceType: client.InstanceDeviceTypeTmpfs,
				Tmpfs: client.InstanceDeviceTmpfsConfig{
					Path: cVol.Target,
					Size: formatTmpfsSize(cVol.Tmpfs),
				},
			}
			devices = append(devices, client.InstanceDevice{Name: devName, Config: devConfig})
		default:
			err := fmt.Errorf("unknown volume type %q for service %q", cVol.Type, service.Name)
			errs = errors.Join(errs, err)
			continue
		}
	}

	// Another declaration for tmpfs devices.
	if len(service.Tmpfs) > 0 {
		for idx, tmpfsPath := range service.Tmpfs {
			devices = append(devices, client.InstanceDevice{
				Name: fmt.Sprintf("tmpfs-%d", idx),
				Config: client.InstanceDeviceConfig{
					DeviceType: client.InstanceDeviceTypeTmpfs,
					Tmpfs: client.InstanceDeviceTmpfsConfig{
						Path: tmpfsPath,
						Size: strconv.FormatInt(32*1024, 10),
					},
				},
			})
		}
	}

	// shm_size mounts a tmpfs at /dev/shm with the specified size.
	if service.ShmSize > 0 {
		devices = append(devices, client.InstanceDevice{
			Name: "shm",
			Config: client.InstanceDeviceConfig{
				DeviceType: client.InstanceDeviceTypeTmpfs,
				Tmpfs: client.InstanceDeviceTmpfsConfig{
					Path: "/dev/shm",
					Size: strconv.FormatInt(int64(service.ShmSize), 10),
				},
			},
		})
	}

	return devices, files, resources, errs
}

// instanceSecrets resolves a service's secrets from their compose definitions,
// reading content from a file or an environment variable.
func instanceSecrets(p *types.Project, service types.ServiceConfig) ([]client.InstanceFile, error) {
	var errs error
	result := []client.InstanceFile{}

	for _, svcSecret := range service.Secrets {
		secretDef, ok := p.Secrets[svcSecret.Source]
		if !ok {
			errs = errors.Join(errs, fmt.Errorf("secret %q not defined", svcSecret.Source))
			continue
		}

		switch {
		case secretDef.File != "":
			fp, err := os.Open(secretDef.File)
			if err != nil {
				errs = errors.Join(errs, fmt.Errorf("secret '%v' source not found or not readable", secretDef.File))
				continue
			}
			result = append(result, client.InstanceFile{
				Target:    svcSecret.Target,
				Content:   fp,
				UID:       parseSecretID(svcSecret.UID),
				GID:       parseSecretID(svcSecret.GID),
				Mode:      parseSecretMode(svcSecret.Mode),
				Overwrite: true,
			})
		case secretDef.Environment != "":
			value, ok := p.Environment[secretDef.Environment]
			if !ok {
				errs = errors.Join(errs, fmt.Errorf("secret '%v' not found in the environment", secretDef.Environment))
				continue
			}

			result = append(result, client.InstanceFile{
				Target:    svcSecret.Target,
				Content:   client.NewReaderFromBytes([]byte(value)),
				UID:       parseSecretID(svcSecret.UID),
				GID:       parseSecretID(svcSecret.GID),
				Mode:      parseSecretMode(svcSecret.Mode),
				Overwrite: true,
			})
		default:
			errs = errors.Join(errs, fmt.Errorf("secret '%v' has no source (file or environment)", svcSecret.Source))

			continue
		}
	}

	return result, errs
}

// instanceDependencyWaits builds the health-wait map for depends_on entries with
// condition: service_healthy, keyed by the dependency's sanitized instance names.
func instanceDependencyWaits(p *types.Project, service types.ServiceConfig, options *ResourcesOptions) map[string]string {
	deps := map[string]string{}
	for depName, dep := range service.DependsOn {
		if dep.Condition != types.ServiceConditionHealthy {
			continue
		}
		depSvc := p.Services[depName]
		depScale := 1
		if s, ok := options.Scale[depName]; ok {
			depScale = s
		} else if depSvc.Deploy != nil && depSvc.Deploy.Replicas != nil {
			depScale = *depSvc.Deploy.Replicas
		}
		if depSvc.ContainerName != "" {
			deps[client.SanitizeIncusName(depSvc.ContainerName, client.MaxIncusNameLen)] = client.HealthStatusHealthy
		} else {
			for i := 1; i <= depScale; i++ {
				deps[client.SanitizeIncusName(fmt.Sprintf("%s-%d", depName, i), client.MaxIncusNameLen)] = client.HealthStatusHealthy
			}
		}
	}

	return deps
}

// instanceName derives the instance name: container_name takes precedence,
// otherwise {service}-{index}. A container_name with scale > 1 is suffixed with
// the index to keep names unique.
func instanceName(service types.ServiceConfig, index, scale int) string {
	name := fmt.Sprintf("%s-%d", service.Name, index)
	if service.ContainerName != "" {
		if scale > 1 {
			name = fmt.Sprintf("%s-%d", service.ContainerName, index)
		} else {
			name = service.ContainerName
		}
	}
	return name
}

// resourceLimits resolves the two ways compose expresses limits: the v2-era
// service-level `cpus`/`mem_limit` and `deploy.resources.limits`, which wins
// where both carry a value, as in docker compose.
//
// The loader rejects the two disagreeing, but only when the deploy block sets
// the same key, so a service may legitimately arrive with one of each.
func resourceLimits(service types.ServiceConfig) types.Resource {
	limits := types.Resource{
		NanoCPUs:    types.NanoCPUs(service.CPUS),
		MemoryBytes: service.MemLimit,
	}

	if service.Deploy == nil || service.Deploy.Resources.Limits == nil {
		return limits
	}

	deploy := service.Deploy.Resources.Limits
	if deploy.NanoCPUs != 0 {
		limits.NanoCPUs = deploy.NanoCPUs
	}

	if deploy.MemoryBytes != 0 {
		limits.MemoryBytes = deploy.MemoryBytes
	}

	return limits
}

// applyResourceLimits maps Docker Compose resource limits to Incus config keys.
//
// CPU mapping: limits.cpu.allowance = "<cpus*100>ms/100ms".
//
// Memory mapping: limits.memory = human-readable size (GiB, MiB, KiB, or B).
func applyResourceLimits(config map[string]string, limits types.Resource) {
	if limits.NanoCPUs != 0 {
		config["limits.cpu.allowance"] = formatCPUAllowance(limits.NanoCPUs.Value())
	}
	if limits.MemoryBytes != 0 {
		config["limits.memory"] = formatMemoryLimit(int64(limits.MemoryBytes))
	}
}

// formatCPUAllowance converts a compose cpus count to an Incus CPU allowance.
//
// The time form is the one that caps: Incus reads it as a CFS quota over the
// period, which is what compose `cpus` means. Its percentage form only sets
// scheduler shares, so it bites under contention and never otherwise.
func formatCPUAllowance(cpus float32) string {
	const periodMS = 100

	// Rounding a sub-millisecond quota down to 0 would stop the instance dead.
	quota := max(int64(math.Round(float64(cpus)*periodMS)), 1)

	return fmt.Sprintf("%dms/%dms", quota, periodMS)
}

// formatMemoryLimit converts bytes to a human-readable Incus memory limit string.
func formatMemoryLimit(bytes int64) string {
	const (
		gib = 1 << 30
		mib = 1 << 20
		kib = 1 << 10
	)
	switch {
	case bytes%gib == 0:
		return fmt.Sprintf("%dGiB", bytes/gib)
	case bytes%mib == 0:
		return fmt.Sprintf("%dMiB", bytes/mib)
	case bytes%kib == 0:
		return fmt.Sprintf("%dKiB", bytes/kib)
	default:
		return fmt.Sprintf("%dB", bytes)
	}
}

// applyRestartPolicy maps Docker Compose restart policies to Incus boot config.
//
// Mapping:
//   - "no" (default): boot.autostart=false
//   - "always": boot.autostart=true
//   - "on-failure": boot.autostart=true, boot.autorestart=true
//   - "unless-stopped": boot.autostart unset (uses last-state behavior)
func applyRestartPolicy(config map[string]string, policy string) {
	switch policy {
	case "always":
		config["boot.autostart"] = "true"
	case "on-failure":
		config["boot.autostart"] = "true"
		config["boot.autorestart"] = "true"
	case "unless-stopped":
		// Leave unset - Incus defaults to "last-state" behavior
	case "no", "":
		config["boot.autostart"] = "false"
	}
}

// formatTmpfsSize converts compose tmpfs size to a string.
func formatTmpfsSize(opts *types.ServiceVolumeTmpfs) string {
	if opts == nil || opts.Size == 0 {
		return ""
	}
	return strconv.FormatInt(int64(opts.Size), 10)
}

// instanceConfigs resolves a service's configs from their compose definitions,
// reading content from a file, inline content, or an environment variable.
func instanceConfigs(p *types.Project, service types.ServiceConfig) ([]client.InstanceFile, error) {
	var errs error
	result := []client.InstanceFile{}

	for _, svcConfig := range service.Configs {
		configDef, ok := p.Configs[svcConfig.Source]
		if !ok {
			errs = errors.Join(errs, fmt.Errorf("config %q not defined", svcConfig.Source))
			continue
		}

		target := svcConfig.Target
		if target == "" {
			target = "/" + svcConfig.Source
		}

		switch {
		case configDef.File != "":
			fp, err := os.Open(configDef.File)
			if err != nil {
				errs = errors.Join(errs, fmt.Errorf("config '%v' source not found or not readable", configDef.File))
				continue
			}
			result = append(result, client.InstanceFile{
				Target:    target,
				Content:   fp,
				UID:       parseSecretID(svcConfig.UID),
				GID:       parseSecretID(svcConfig.GID),
				Mode:      parseConfigMode(svcConfig.Mode),
				Overwrite: true,
			})
		// Content will be populated by compose-go from its environment.
		case configDef.Content != "":
			result = append(result, client.InstanceFile{
				Target:    target,
				Content:   client.NewReaderFromBytes([]byte(configDef.Content)),
				UID:       parseSecretID(svcConfig.UID),
				GID:       parseSecretID(svcConfig.GID),
				Mode:      parseConfigMode(svcConfig.Mode),
				Overwrite: true,
			})
		default:
			errs = errors.Join(errs, fmt.Errorf("config '%v' has no source (file or content - content would be populated from environment)", target))
			continue
		}
	}

	return result, errs
}

// parseConfigMode parses a file mode to int, defaulting to 0444. Per the
// compose-spec, the writable bit must be ignored for configs.
func parseConfigMode(mode *types.FileMode) int {
	if mode == nil {
		return 0o444
	}
	return int(*mode) &^ 0o222
}

// parseSecretID parses a UID string to int64.
func parseSecretID(id string) int64 {
	if id == "" {
		return -1
	}
	v, _ := strconv.ParseInt(id, 10, 64)
	return v
}

// parseSecretMode parses a file mode to int. Per the
// compose-spec, the writable bit must be ignored for secrets.
func parseSecretMode(mode *types.FileMode) int {
	if mode == nil {
		return 0o400
	}
	return int(*mode) &^ 0o222
}

// xICInstanceNetwork extracts the x-incus-compose.network string override
// from a compose network definition. Returns "" if not set.
func xICInstanceNetwork(networkDef types.NetworkConfig) string {
	var raw map[string]any
	ok, err := networkDef.Extensions.Get("x-incus-compose", &raw)
	if !ok || err != nil {
		return ""
	}
	n, ok := raw["network"].(string)
	if !ok {
		return ""
	}
	return n
}

// networkExtensions extracts the x-incus extension map from a compose network
// definition and returns it as a flat map[string]string for use as Incus network
// config. Keys and values are taken verbatim from the x-incus YAML block.
func networkExtensions(networkDef types.NetworkConfig) map[string]string {
	var raw map[string]any
	ok, err := networkDef.Extensions.Get("x-incus", &raw)
	if !ok || err != nil || len(raw) == 0 {
		return map[string]string{}
	}

	result := make(map[string]string, len(raw))
	for k, v := range raw {
		result[k] = fmt.Sprint(v)
	}

	return result
}

// xIncusExtensions extracts the x-incus extension map from a compose
// volume definition or inline volume entry and returns it as a flat
// map[string]string for use as Incus volume config. Keys and values are taken
// verbatim from the x-incus YAML block.
func xIncusExtensions(ext types.Extensions) map[string]string {
	var raw map[string]any
	ok, err := ext.Get("x-incus", &raw)
	if !ok || err != nil || len(raw) == 0 {
		return map[string]string{}
	}

	result := make(map[string]string, len(raw))
	for k, v := range raw {
		result[k] = fmt.Sprint(v)
	}

	return result
}

// volumeXIncusComposePool extracts the pool name from x-incus-compose.pool on a
// compose volume definition.
func volumeXIncusComposePool(volDef types.VolumeConfig) string {
	var raw map[string]any
	ok, err := volDef.Extensions.Get("x-incus-compose", &raw)
	if !ok || err != nil {
		return ""
	}
	pool, ok := raw["pool"].(string)
	if !ok {
		return ""
	}
	return pool
}

// serviceXIncusExtensions extracts the x-incus extension map from a compose service
// definition and returns it as a flat map[string]string for use as Incus instance
// config. Keys and values are taken verbatim from the x-incus YAML block.
func serviceXIncusExtensions(service types.ServiceConfig) map[string]string {
	var raw map[string]any
	ok, err := service.Extensions.Get("x-incus", &raw)
	if !ok || err != nil || len(raw) == 0 {
		return nil
	}

	result := make(map[string]string, len(raw))
	for k, v := range raw {
		result[k] = fmt.Sprint(v)
	}

	return result
}

// serviceExtraDevices extracts raw Incus devices from the x-incus-compose.devices
// block on a compose service. Each named entry becomes an instance device whose
// keys are passed verbatim to Incus; the `type` key selects the device type.
func serviceExtraDevices(service types.ServiceConfig) ([]client.InstanceDevice, error) {
	var raw map[string]any
	ok, err := service.Extensions.Get("x-incus-compose", &raw)
	if !ok || err != nil {
		return nil, nil //nolint:nilerr // missing/malformed extension means no extra devices
	}

	devicesRaw, ok := raw["devices"].(map[string]any)
	if !ok || len(devicesRaw) == 0 {
		return nil, nil
	}

	devices := make([]client.InstanceDevice, 0, len(devicesRaw))
	for name, cfg := range devicesRaw {
		cfgMap, ok := cfg.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("x-incus-compose.devices: device %q must be a map", name)
		}

		ext := make(map[string]string, len(cfgMap))
		for k, v := range cfgMap {
			ext[k] = fmt.Sprint(v)
		}

		if ext["type"] == "" {
			return nil, fmt.Errorf("x-incus-compose.devices: device %q is missing 'type'", name)
		}

		devices = append(devices, client.InstanceDevice{
			Name: name,
			Config: client.InstanceDeviceConfig{
				DeviceType: ext["type"],
				Extensions: ext,
			},
		})
	}

	return devices, nil
}

// checkEntrypoint rejects a service whose entrypoint and command are both
// explicitly empty, which would leave nothing to exec.
func checkEntrypoint(service types.ServiceConfig) error {
	if service.Entrypoint == nil || len(service.Entrypoint)+len(service.Command) > 0 {
		return nil
	}

	return fmt.Errorf("service %q sets an empty entrypoint and no command, leaving nothing to run", service.Name)
}
