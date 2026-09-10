package main

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	incusApi "github.com/lxc/incus/v7/shared/api"
	"github.com/lxc/incus/v7/shared/units"
	"github.com/urfave/cli/v3"

	"github.com/lxc/incus-compose/client"
	"github.com/lxc/incus-compose/shared"
)

const (
	defaultDNSCPU         = "400ms/100ms"
	defaultDNSMemoryLimit = "100MiB"
)

const (
	globalDNSName    = "ic-dns"
	globalDNSNetwork = "icompose0"
	dnsVolume        = "ic-dns"
)

// dnsInstanceName returns the sidecar's Incus instance name.
func dnsInstanceName(incusProject string, global bool) string {
	if global {
		return globalDNSName
	}

	return incusProject + "-ic-dns"
}

// dnsCertName returns the sidecar's name in the Incus trust store.
func dnsCertName(incusProject string, global bool) string {
	if global {
		return "ic-dns-global"
	}

	return "ic-dns-" + incusProject
}

// dnsParams holds the image options for dns setup.
type dnsParams struct {
	image         string
	pull          string
	incus         *url.URL
	network       string
	ipv4Address   string
	ipv6Address   string
	scope         string
	noMetrics     bool
	listen        string
	http          string
	forward       []string
	suffix        string
	ttl           uint
	projectMarker string
	timeout       time.Duration
	stackWorkers  int

	global bool
	xIncus map[string]string
	carry  map[string]string
}

// dnsDropPrefixes are config namespaces Incus or the image owns.
var dnsDropPrefixes = []string{"volatile.", "image.", "oci."}

// dnsDropKeys are decided by the new image, registration or daemon.
var dnsDropKeys = []string{
	"user.image_alias",
	"environment.INCUS_COMPOSE_DNS_TOKEN",
	"environment.DNS_TOKEN",
	shared.HealthStoppedKey,
}

const (
	envDNSIncus         = "environment.INCUS_COMPOSE_DNS_INCUS"
	envDNSListen        = "environment.INCUS_COMPOSE_DNS_LISTEN"
	envDNSHTTP          = "environment.INCUS_COMPOSE_DNS_HTTP"
	envDNSForward       = "environment.INCUS_COMPOSE_DNS_FORWARD"
	envDNSSuffix        = "environment.INCUS_COMPOSE_DNS_SUFFIX"
	envDNSTTL           = "environment.INCUS_COMPOSE_DNS_TTL"
	envDNSDataDir       = "environment.INCUS_COMPOSE_DNS_DATA_DIR"
	envDNSSecretsDir    = "environment.INCUS_COMPOSE_DNS_SECRETS_DIR"
	envDNSMetrics       = "environment.INCUS_COMPOSE_DNS_METRICS"
	envDNSProjectMarker = "environment.INCUS_COMPOSE_DNS_PROJECT_MARKER"
	envDNSProjects      = "environment.INCUS_COMPOSE_DNS_PROJECTS"
	envDNSRestricted    = "environment.INCUS_COMPOSE_DNS_RESTRICTED"
	envDNSLog           = "environment.INCUS_COMPOSE_DNS_LOG"
)

// dnsSettings layers settings over the daemon being replaced.
func dnsSettings(params dnsParams, incusURL string, debug bool) map[string]string {
	settings := map[string]string{}
	maps.Copy(settings, params.carry)

	set := func(k1, k2, v string) {
		settings[k1] = v
		settings[k2] = v
	}

	if params.incus != nil || (settings[envDNSIncus] == "" && settings["environment.DNS_INCUS"] == "") {
		set(envDNSIncus, "environment.DNS_INCUS", incusURL)
	}
	set(envDNSDataDir, "environment.DNS_DATA_DIR", "/var/lib/dns-incus")
	set(envDNSSecretsDir, "environment.DNS_SECRETS_DIR", "/run/secrets")

	httpAddr := ":9153"
	if params.http != "" {
		httpAddr = params.http
	}
	set(envDNSHTTP, "environment.DNS_HTTP", httpAddr)

	if params.listen != "" {
		set(envDNSListen, "environment.DNS_LISTEN", params.listen)
	}
	if len(params.forward) > 0 {
		fw := strings.Join(params.forward, ",")
		set(envDNSForward, "environment.DNS_FORWARD", fw)
	}
	if params.suffix != "" {
		set(envDNSSuffix, "environment.DNS_SUFFIX", params.suffix)
	}
	if params.ttl > 0 {
		ttlStr := strconv.Itoa(int(params.ttl))
		set(envDNSTTL, "environment.DNS_TTL", ttlStr)
	}
	if params.noMetrics {
		set(envDNSMetrics, "environment.DNS_METRICS", "false")
	}
	if debug {
		set(envDNSLog, "environment.DNS_LOG", "DEBUG")
	}

	if params.global {
		marker := params.projectMarker
		if marker == "" {
			marker = shared.DNSScopeKey + "=" + shared.DNSScopeGlobal
		}
		set(envDNSProjectMarker, "environment.DNS_PROJECT_MARKER", marker)
	} else {
		if params.scope == shared.DNSScopeProject {
			set(envDNSRestricted, "environment.DNS_RESTRICTED", "true")
		} else if params.scope != "" {
			marker := params.projectMarker
			if marker == "" {
				marker = shared.DNSScopeKey + "=" + params.scope
			}
			set(envDNSProjectMarker, "environment.DNS_PROJECT_MARKER", marker)
		}
	}

	maps.Copy(settings, params.xIncus)

	return settings
}

// dnsCarriedConfig survives replacing a sidecar.
func dnsCarriedConfig(config map[string]string) map[string]string {
	carried := map[string]string{}

	for key, value := range config {
		if slices.Contains(dnsDropKeys, key) {
			continue
		}

		if slices.ContainsFunc(dnsDropPrefixes, func(p string) bool { return strings.HasPrefix(key, p) }) {
			continue
		}

		carried[key] = value
	}

	dnsFloorLimits(carried)

	return carried
}

// dnsFloorLimits raises a carried limit below the sidecar's default.
func dnsFloorLimits(carried map[string]string) {
	floor := func(key string, minBytes int64) {
		val := carried[key]
		if val == "" {
			return
		}

		have, err := units.ParseByteSizeString(val)
		if err == nil && have < minBytes {
			delete(carried, key)
		}
	}

	minMem, err := units.ParseByteSizeString(defaultDNSMemoryLimit)
	if err == nil {
		floor("limits.memory", minMem)
	}
}

// dnsCreateToken creates the sidecar's trust token.
func dnsCreateToken(ctx context.Context, c *client.Client, global bool, restrictedProjects []string) (string, error) {
	req := incusApi.CertificatesPost{
		CertificatePut: incusApi.CertificatePut{
			Name: dnsCertName(c.IncusProject(), global),
			Type: "client",
		},
		Token: true,
	}

	if !global && len(restrictedProjects) > 0 {
		req.Restricted = true
		req.Projects = restrictedProjects
	}

	conn, err := c.GlobalConnection()
	if err != nil {
		return "", err
	}

	tokenCtx, release := context.WithCancel(ctx)
	defer release()

	updates, err := conn.CreateCertificateToken(tokenCtx, req)
	if err != nil {
		return "", err
	}

	opAPI, ok := <-updates
	if !ok {
		return "", errors.New("the certificate token operation reported nothing")
	}

	addToken, err := opAPI.ToCertificateAddToken()
	if err != nil {
		return "", fmt.Errorf("converting operation to certificate add token: %w", err)
	}

	return addToken.String(), nil
}

// dnsRevokeCert removes the sidecar's trust certificate.
func dnsRevokeCert(ctx context.Context, c *client.Client, global bool) error {
	gConn, err := c.GlobalConnection()
	if err != nil {
		return fmt.Errorf("while getting a global connection: %w", err)
	}

	certs, err := gConn.GetCertificates(ctx)
	if err != nil {
		return fmt.Errorf("listing certificates: %w", err)
	}

	want := dnsCertName(c.IncusProject(), global)
	for _, cert := range certs {
		if cert.Name != want {
			continue
		}

		err = gConn.DeleteCertificate(ctx, cert.Fingerprint)
		if err != nil {
			return fmt.Errorf("deleting certificate %s: %w", cert.Fingerprint, err)
		}
	}

	return nil
}

// dnsGetResources creates image and volume resources and returns configured instance.
func dnsGetResources(c *client.Client, params dnsParams) (*client.Instance, []client.Resource, error) {
	imgRes, err := c.Resource(client.KindImage, params.image, &client.ImageConfig{})
	if err != nil {
		return nil, nil, fmt.Errorf("getting the dns image '%v': %w", params.image, err)
	}
	img, ok := imgRes.(*client.Image)
	if !ok {
		return nil, nil, client.ErrUnknown.WithResource(imgRes)
	}

	volRes, err := c.Resource(
		client.KindStorageVolume,
		dnsVolume,
		&client.StorageVolumeConfig{Shifted: true, ImageResource: imgRes},
	)
	if err != nil {
		return nil, nil, client.ErrUnknown.WithKindName(client.KindStorageVolume, dnsVolume).Wrap(err)
	}
	volume, ok := volRes.(*client.StorageVolume)
	if !ok {
		return nil, nil, client.ErrUnknown.WithResource(volRes)
	}

	instanceConfig := &client.InstanceConfig{
		Image: imgRes.Name(),
		Type:  incusApi.InstanceTypeContainer,
		Extensions: map[string]string{
			"limits.cpu.allowance":             defaultDNSCPU,
			"limits.memory":                    defaultDNSMemoryLimit,
			shared.HealthKeyPrefix + "restart": "unless-stopped",
			shared.HealthIgnoreKey:             "true",
			shared.DNSDaemonScopeKey:           params.scope,
			managedKey:                         "true",
		},
		Resources: []client.Resource{img},
		Priority:  client.PriorityInstance - 1,
	}

	maps.Copy(instanceConfig.Extensions, params.xIncus)

	instanceConfig.Devices = append(instanceConfig.Devices, client.InstanceDevice{
		Name: "data",
		Config: client.InstanceDeviceConfig{
			DeviceType: client.InstanceDeviceTypeDisk,
			Disk: client.InstanceDeviceDiskConfig{
				StorageVolumeConfig: &volume.Config,
				Source:              volume.IncusName(),
				Path:                "/var/lib/dns-incus",
				Shift:               true,
			},
		},
	})

	instRes, err := c.Resource(client.KindInstance, dnsInstanceName(c.IncusProject(), params.global), instanceConfig)
	if err != nil {
		return nil, nil, fmt.Errorf("getting the dns instance resource: %w", err)
	}

	inst, ok := instRes.(*client.Instance)
	if !ok {
		return nil, nil, client.ErrUnknown.WithResource(instRes)
	}

	c.AddHookBefore(func(ctx context.Context, action client.Action, r client.Resource, options client.Options, err error) error {
		if err != nil || action != client.ActionEnsure || r.Kind() != client.KindInstance || r.IncusName() != inst.IncusName() {
			return err
		}

		if !options.Create {
			return err
		}

		if info := inst.State().IncusInstance; info != nil {
			_, ok := info.Config[envDNSIncus]
			_, okLegacy := info.Config["environment.DNS_INCUS"]
			if ok || okLegacy {
				return nil
			}
		}

		network, err := dnsEnsureNetwork(ctx, c, params)
		if err != nil {
			return err
		}

		inst.Config.Resources = append(inst.Config.Resources, network)

		var incusURL string
		if params.incus != nil || params.carry[envDNSIncus] == "" {
			u, err := dnsIncusURL(c, params, network)
			if err != nil {
				return err
			}

			incusURL = u.String()
		}

		var restrictedProjects []string
		if !params.global && params.scope == shared.DNSScopeProject {
			restrictedProjects = []string{c.IncusProject()}
		}

		token, err := dnsCreateToken(ctx, c, params.global, restrictedProjects)
		if err != nil {
			c.LogWarn("Failed to get a dns token", "error", err)
			token = ""
		}

		maps.Copy(inst.Config.Extensions, dnsSettings(params, incusURL, c.IsDebugging()))
		if token != "" {
			inst.Config.Extensions["environment.INCUS_COMPOSE_DNS_TOKEN"] = token
			inst.Config.Extensions["environment.DNS_TOKEN"] = token
		}

		inst.Config.Files = append(inst.Config.Files, client.InstanceFile{
			Target:  "/run/secrets/token",
			Content: client.NewReaderFromBytes([]byte(token)),
			Mode:    0o600,
			DirMode: 0o700,
		})

		eth0 := client.InstanceDevice{
			Name: "eth0",
			Config: client.InstanceDeviceConfig{
				DeviceType:  client.InstanceDeviceTypeNic,
				NetworkName: network.IncusName(),
				Extensions:  map[string]string{},
			},
		}
		if params.ipv4Address != "" {
			eth0.Config.Extensions["ipv4.address"] = params.ipv4Address
		}
		if params.ipv6Address != "" {
			eth0.Config.Extensions["ipv6.address"] = params.ipv6Address
		}
		inst.Config.Devices = append(inst.Config.Devices, eth0)
		inst.Config.Extensions["oci.entrypoint"] = "/usr/local/sbin/ic-dns run"

		return nil
	})

	return inst, []client.Resource{img, volume}, nil
}

func parseDNSNetwork(c *client.Client, network string, global bool) (sidecarNetworkRef, error) {
	if network == "" {
		if global {
			return sidecarNetworkRef{name: globalDNSNetwork, deflt: true, incusName: globalDNSNetwork}, nil
		}

		return sidecarNetworkRef{name: "default", deflt: true}, nil
	}

	if strings.Contains(network, ":") {
		p, n, _ := strings.Cut(network, ":")

		if p == "" {
			p = c.Project()
		}
		if n == "" || strings.Contains(n, ":") {
			return sidecarNetworkRef{}, errors.New("`--network` is wrong, need something like `<project>:<network>` or `<bridge>`")
		}

		return sidecarNetworkRef{project: p, name: n}, nil
	}

	return sidecarNetworkRef{name: network}, nil
}

func dnsEnsureNetwork(ctx context.Context, c *client.Client, params dnsParams) (*client.Network, error) {
	ref, err := parseDNSNetwork(c, params.network, params.global)
	if err != nil {
		return nil, err
	}

	return sidecarEnsureNetwork(ctx, c, ref, "dns")
}

func dnsIncusURL(c *client.Client, params dnsParams, network *client.Network) (*url.URL, error) {
	return sidecarIncusURL(c, params.incus, network, "ic-dns", "INCUS_COMPOSE_DNS_INCUS")
}

// dnsNeedsUpgrade reports whether a sidecar on have should be replaced by want.
func dnsNeedsUpgrade(have, want string) bool {
	if have == want {
		return false
	}

	haveVersion, wantVersion := imageSemver(have), imageSemver(want)
	if haveVersion != nil && wantVersion != nil {
		return wantVersion.GreaterThan(haveVersion)
	}

	return true
}

// dnsTeardown removes a dns sidecar, its volume and its certificate
// from the project c belongs to.
func dnsTeardown(ctx context.Context, c *client.Client, global bool, timeout time.Duration, keepVolume bool) error {
	stack := client.NewStack(c, client.StackSortDescending())

	if !keepVolume {
		volRes, err := c.Resource(client.KindStorageVolume, dnsVolume, &client.StorageVolumeConfig{})
		if err != nil {
			return fmt.Errorf("getting the dns volume resource: %w", err)
		}
		stack.Add(volRes)
	}

	instRes, err := c.Resource(
		client.KindInstance,
		dnsInstanceName(c.IncusProject(), global),
		&client.InstanceConfig{},
	)
	if err != nil {
		return fmt.Errorf("getting the dns instance resource: %w", err)
	}
	stack.Add(instRes)

	c.LogDebug("Ensure", "resources", stack.All())

	err = stack.ForAction(client.ActionEnsure).Run(ctx, client.ActionEnsure)
	if err != nil {
		return fmt.Errorf("ensuring dns: %w", err)
	}

	runOpts := []client.Option{client.OptionForce(), client.OptionTimeout(timeout)}

	var errs error
	err = stack.ForAction(client.ActionStop).Run(ctx, client.ActionStop, runOpts...)
	if err != nil {
		errs = errors.Join(errs, fmt.Errorf("stopping dns resources: %w", err))
	}

	err = stack.ForAction(client.ActionDelete).Run(ctx, client.ActionDelete, runOpts...)
	if err != nil {
		errs = errors.Join(errs, fmt.Errorf("deleting dns resources: %w", err))
	}

	if !keepVolume {
		err = dnsRevokeCert(ctx, c, global)
		if err != nil {
			errs = errors.Join(errs, fmt.Errorf("revoking the dns cert: %w", err))
		}
	}

	return errs
}

func newDNSCommand() *cli.Command {
	return &cli.Command{
		Name:     "dns",
		Usage:    "Manage the ic-dns sidecar",
		Category: "extensions",
		Commands: []*cli.Command{
			newDNSLogsCommand(),
			newDNSStatusCommand(),
			newDNSUpCommand(),
			newDNSDownCommand(),
		},
	}
}
