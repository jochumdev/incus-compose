package main

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/url"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/compose-spec/compose-go/v2/types"
	incusApi "github.com/lxc/incus/v7/shared/api"
	"github.com/lxc/incus/v7/shared/units"
	"github.com/urfave/cli/v3"

	"github.com/lxc/incus-compose/client"
	"github.com/lxc/incus-compose/project"
	"github.com/lxc/incus-compose/shared"
)

// DefaultHealthdImage is the default healthd image to use, a var cause overwriteable by ldflags.
var DefaultHealthdImage = "ghcr.io/lxc/incus-compose/ic-healthd:{version}"

const (
	defaultHealthdCPU         = "200ms/100ms"
	defaultHealthdMemoryLimit = "256MiB"
)

const (
	// globalHealthdProject is where the shared daemon lives, not configurable.
	globalHealthdName = "ic-healthd"

	// globalHealthdNetwork is the shared daemon's own bridge, so it depends on
	// nothing about how the default project is set up.
	globalHealthdNetwork = "icompose0"

	// healthdVolume holds the daemon's generated cert and key.
	healthdVolume = "ic-healthd"
)

// healthdInstanceName returns the sidecar's Incus instance name.
func healthdInstanceName(incusProject string, global bool) string {
	if global {
		return globalHealthdName
	}

	return incusProject + "-ic-healthd"
}

// healthdCertName returns the sidecar's name in the Incus trust store.
func healthdCertName(incusProject string, global bool) string {
	if global {
		return "ic-healthd-global"
	}

	return "ic-healthd-" + incusProject
}

// healthdParams holds the image/binary options for healthd setup.
type healthdParams struct {
	binary       string
	image        string // already resolved via resolveImageVersion
	pull         string
	incus        *url.URL
	network      string // Incus bridge name; empty = the scope's own default network
	timeout      time.Duration
	stackWorkers int // concurrency of our own resource stack, not the daemon's

	// global shares one daemon in globalHealthdProject instead of one per project.
	global bool
	scope  string

	// trace turns on the daemon's per-event logging.
	trace bool

	// workers and restartWorkers size the daemon's pools; 0 keeps its defaults.
	workers        int
	restartWorkers int

	// xIncus is Incus instance config for the sidecar, e.g. limits.*.
	xIncus map[string]string

	// carry is the replaced daemon's config on an upgrade. It is filled between
	// the two ensures, so the hook applying it must read it at hook time.
	carry map[string]string
}

// healthdDropPrefixes are config namespaces Incus or the image owns.
var healthdDropPrefixes = []string{"volatile.", "image.", "oci."}

// healthdDropKeys are decided by the new image, the new registration or the
// daemon itself, so carrying them would pin a sidecar to the one it replaced.
var healthdDropKeys = []string{
	"user.image_alias",
	"environment.INCUS_COMPOSE_HEALTHD_TOKEN",
	shared.HealthStatusKey,
	client.HealthKeyPrefix + "stopped",
}

// The daemon's environment keys, as Incus instance config.
const (
	envIncus          = "environment.INCUS_COMPOSE_HEALTHD_INCUS"
	envWorkers        = "environment.INCUS_COMPOSE_HEALTHD_WORKERS"
	envRestartWorkers = "environment.INCUS_COMPOSE_HEALTHD_RESTART_WORKERS"
	envDebug          = "environment.INCUS_COMPOSE_HEALTHD_DEBUG"
	envTrace          = "environment.INCUS_COMPOSE_HEALTHD_TRACE"
)

// healthdSettings layers this run's settings over the daemon being replaced:
// what a flag or the compose file names wins, what it leaves out is kept, and
// incusURL only fills the gap when neither supplies an endpoint.
func healthdSettings(params healthdParams, incusURL string, debug bool) map[string]string {
	settings := map[string]string{}
	maps.Copy(settings, params.carry)

	if params.incus != nil || settings[envIncus] == "" {
		settings[envIncus] = incusURL
	}
	if params.workers > 0 {
		settings[envWorkers] = strconv.Itoa(params.workers)
	}
	if params.restartWorkers > 0 {
		settings[envRestartWorkers] = strconv.Itoa(params.restartWorkers)
	}
	if debug {
		settings[envDebug] = "true"
	}
	if params.trace {
		settings[envTrace] = "true"
	}

	// Last, so x-incus overrides a limit the replaced daemon carried.
	maps.Copy(settings, params.xIncus)

	return settings
}

// healthdCarriedConfig is what survives replacing a sidecar: everything the
// running daemon carries except what has to be derived again. Excluding rather
// than listing keeps settings a newer incus-compose does not know about.
func healthdCarriedConfig(config map[string]string) map[string]string {
	carried := map[string]string{}

	for key, value := range config {
		if slices.Contains(healthdDropKeys, key) {
			continue
		}

		if slices.ContainsFunc(healthdDropPrefixes, func(p string) bool { return strings.HasPrefix(key, p) }) {
			continue
		}

		carried[key] = value
	}

	healthdFloorLimits(carried)

	return carried
}

// healthdFloorLimits raises a carried limit below the sidecar's default, so a
// daemon created by an older version is not kept at a size its worker pools
// have outgrown.
func healthdFloorLimits(carried map[string]string) {
	_, hasOldCPULimit := carried["limits.cpu"]
	if hasOldCPULimit {
		i, err := strconv.Atoi(carried["limits.cpu"])
		if err != nil {
			return
		}
		// Upgrade v1.1.0 `limits.cpu: 1` to v1.2 `limits.cpu: 2`
		if i < 2 {
			i = 2
		}

		// Upgrade to current "limits.cpu.allowance"
		carried["limits.cpu.allowance"] = fmt.Sprintf("%dms/100ms", i*100)
		delete(carried, "limits.cpu")
	}

	// An empty value parses as zero bytes, which would floor a limit into being.
	if carried["limits.memory"] == "" {
		return
	}

	memory, err := units.ParseByteSizeString(carried["limits.memory"])
	if err != nil {
		return
	}

	def, err := units.ParseByteSizeString(defaultHealthdMemoryLimit)
	if err == nil && memory < def {
		carried["limits.memory"] = defaultHealthdMemoryLimit
	}
}

// healthdCreateToken creates the sidecar's trust token. Daemons watching multiple
// projects must reach projects that do not exist yet, so only a per-project one is restricted.
func healthdCreateToken(ctx context.Context, c *client.Client, restricted bool) (string, error) {
	req := incusApi.CertificatesPost{
		CertificatePut: incusApi.CertificatePut{
			Name: healthdCertName(c.IncusProject(), !restricted),
			Type: "client",
		},
		Token: true,
	}

	if restricted {
		req.Restricted = true
		req.Projects = []string{c.IncusProject()}
	}

	conn, err := c.GlobalConnection()
	if err != nil {
		return "", err
	}

	// A token operation never finishes, only its first update carries the token.
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

// healthdRevokeCert removes the healthd's trust-store certificate, if any.
func healthdRevokeCert(ctx context.Context, c *client.Client, global bool) error {
	gConn, err := c.GlobalConnection()
	if err != nil {
		return fmt.Errorf("while getting a global connection: %w", err)
	}

	certs, err := gConn.GetCertificates(ctx)
	if err != nil {
		return fmt.Errorf("listing certificates: %w", err)
	}

	want := healthdCertName(c.IncusProject(), global)
	for _, cert := range certs {
		if cert.Name != want {
			continue
		}
		if err := gConn.DeleteCertificate(ctx, cert.Fingerprint); err != nil {
			return fmt.Errorf("deleting certificate %s: %w", cert.Fingerprint, err)
		}
	}
	return nil
}

// healthdInUseByProject reports whether any service in the project requires ic-healthd:
// a declared healthcheck, a non-default restart policy, or a service_healthy depends_on.
func healthdInUseByProject(gc *client.GlobalClient, p *project.Project) bool {
	inUse := false

SERVICES_LOOP:
	for _, svc := range p.Services {
		// https://github.com/compose-spec/compose-spec/blob/main/05-services.md#restart
		if slices.Contains(shared.RestartPolicies, svc.Restart) {
			inUse = true
			break SERVICES_LOOP
		}

		if svc.HealthCheck != nil {
			inUse = true
			break SERVICES_LOOP
		}

		for _, dep := range svc.DependsOn {
			if dep.Condition == types.ServiceConditionHealthy {
				inUse = true
				break SERVICES_LOOP
			}
		}
	}

	if inUse {
		_, err := gc.HTTPSAddress()
		if err != nil {
			gc.LogWarn("Your incus isn't listening on the network, skipping healthd support, see: https://github.com/lxc/incus-compose/blob/main/docs/getting-started.md")
			inUse = false
		}
	}

	return inUse
}

// healthdGetResources creates the image and volume resources for healthd and returns a
// configured (but not yet ensured) instance resource. The returned []client.Resource
// slice contains the image and volume; callers build a stack from it as needed.
func healthdGetResources(c *client.Client, params healthdParams) (*client.Instance, []client.Resource, error) {
	imageName := params.image
	if params.binary != "" {
		imageName = "images:alpine/edge"
	}

	imgRes, err := c.Resource(client.KindImage, imageName, &client.ImageConfig{})
	if err != nil {
		return nil, nil, fmt.Errorf("getting the healthd image '%v': %w", imageName, err)
	}
	img, ok := imgRes.(*client.Image)
	if !ok {
		return nil, nil, client.ErrUnknown.WithResource(imgRes)
	}

	volRes, err := c.Resource(
		client.KindStorageVolume,
		healthdVolume,
		&client.StorageVolumeConfig{Shifted: true, ImageResource: imgRes},
	)
	if err != nil {
		return nil, nil, client.ErrUnknown.WithKindName(client.KindStorageVolume, healthdVolume).Wrap(err)
	}
	volume, ok := volRes.(*client.StorageVolume)
	if !ok {
		return nil, nil, client.ErrUnknown.WithResource(volRes)
	}

	instanceConfig := &client.InstanceConfig{
		Image: imgRes.Name(),
		Type:  incusApi.InstanceTypeContainer,
		Extensions: map[string]string{
			"limits.cpu.allowance":             defaultHealthdCPU,
			"limits.memory":                    defaultHealthdMemoryLimit,
			client.HealthKeyPrefix + "restart": "unless-stopped", // Needed for instance.Start to wait for it.
			client.HealthKeyPrefix + "daemon":  "true",
			client.HealthKeyPrefix + "ignore":  "true",
			managedKey:                         "true",
		},
		Resources: []client.Resource{img},
		Priority:  client.PriorityInstance - 1,
	}

	// After the defaults, so a user can raise the sidecar's limits.
	maps.Copy(instanceConfig.Extensions, params.xIncus)

	instanceConfig.Devices = append(instanceConfig.Devices, client.InstanceDevice{
		Name: "data",
		Config: client.InstanceDeviceConfig{
			DeviceType: client.InstanceDeviceTypeDisk,
			Disk: client.InstanceDeviceDiskConfig{
				StorageVolumeConfig: &volume.Config,
				Source:              volume.IncusName(),
				Path:                "/var/lib/ic-healthd",
				Shift:               true,
			},
		},
	})

	instRes, err := c.Resource(client.KindInstance, healthdInstanceName(c.IncusProject(), params.global), instanceConfig)
	if err != nil {
		return nil, nil, fmt.Errorf("getting the healthd instance resource: %w", err)
	}

	inst, ok := instRes.(*client.Instance)
	if !ok {
		return nil, nil, client.ErrUnknown.WithResource(instRes)
	}

	// The kind matters: the daemon's volume is called ic-healthd too, and
	// ensuring it from in here would re-enter this hook forever.
	c.AddHookBefore(func(ctx context.Context, action client.Action, r client.Resource, options client.Options, err error) error {
		if err != nil || action != client.ActionEnsure || r.Kind() != client.KindInstance || r.IncusName() != inst.IncusName() {
			return err
		}

		// Everything below builds what a create needs, and healthdEnsure's
		// lookup pass carries no create - that pass is what fills the state.
		if !options.Create {
			return err
		}

		if info := inst.State().IncusInstance; info != nil {
			// No need to setup the instance when we already did that.
			_, ok := info.Config["environment.INCUS_COMPOSE_HEALTHD_INCUS"]
			if ok {
				if drift := healthdConfigDrift(params, info.Config); len(drift) > 0 {
					c.LogWarn("The running ic-healthd was configured by another project, ignoring",
						"keys", strings.Join(drift, ", "), "instance", r.IncusName())
				}

				return nil
			}
		}

		network, err := healthdEnsureNetwork(ctx, c, params)
		if err != nil {
			return err
		}

		inst.Config.Resources = append(inst.Config.Resources, network)

		// A carried endpoint stands, so replacing a daemon does not require
		// being able to derive the one it already dials.
		var incusURL string
		if params.incus != nil || params.carry[envIncus] == "" {
			u, err := healthdIncusURL(c, params, network)
			if err != nil {
				return err
			}

			incusURL = u.String()
		}

		restricted := !params.global && params.scope == shared.HealthScopeProject
		token, err := healthdCreateToken(ctx, c, restricted)
		if err != nil {
			c.LogWarn("Failed to get a token", "error", err)
			token = ""
		}

		maps.Copy(inst.Config.Extensions, healthdSettings(params, incusURL, c.IsDebugging()))
		// inst.Config.Extensions["environment.INCUS_COMPOSE_HEALTHD_TOKEN"] = token

		if params.global {
			// No list, so projects that do not exist yet are picked up too.
			inst.Config.Extensions["environment.INCUS_COMPOSE_HEALTHD_PROJECT_MARKER"] =
				shared.HealthScopeKey + "=" + shared.HealthScopeGlobal
		} else if params.scope == shared.HealthScopeProject {
			inst.Config.Extensions["environment.INCUS_COMPOSE_HEALTHD_PROJECTS"] = c.IncusProject()
		} else if params.scope != "" {
			inst.Config.Extensions["environment.INCUS_COMPOSE_HEALTHD_PROJECT_MARKER"] =
				shared.HealthScopeKey + "=" + params.scope
		}

		inst.Config.Files = append(inst.Config.Files, client.InstanceFile{
			Target:  "/run/secrets/token",
			Content: client.NewReaderFromBytes([]byte(token)),
			Mode:    0o600,
			DirMode: 0o700,
		})

		inst.Config.Devices = append(inst.Config.Devices, client.InstanceDevice{
			Name: "eth0",
			Config: client.InstanceDeviceConfig{
				DeviceType:  client.InstanceDeviceTypeNic,
				NetworkName: network.IncusName(),
			},
		})

		if params.binary != "" {
			f, err := filepath.Abs(params.binary)
			if err != nil {
				return err
			}

			inst.Config.Extensions["environment.INCUS_COMPOSE_HEALTHD_TOKEN"] = token

			inst.Config.Files = append(inst.Config.Files, client.InstanceFile{
				Target:  "/usr/local/bin/ic-healthd",
				File:    f,
				Mode:    0o700,
				DirMode: 0o700,
			})
		} else {
			// So ic-healthd can update its own status.
			inst.Config.Extensions["environment.INCUS_COMPOSE_HEALTHD_OWN_PROJECT"] = c.IncusProject()
			inst.Config.Extensions["environment.INCUS_COMPOSE_HEALTHD_OWN_NAME"] = inst.IncusName()

			// c.LogDebug("Setting entrypoint")
			inst.Config.Extensions["oci.entrypoint"] = "/usr/local/bin/ic-healthd run"
		}

		return err
	})

	c.AddHookAfter(func(ctx context.Context, action client.Action, r client.Resource, args client.Options, err error) error {
		if err != nil || action != client.ActionStart || r.Kind() != client.KindInstance || r.IncusName() != inst.IncusName() {
			return err
		}

		if params.binary != "" {
			err := inst.Exec(ctx, "sh", "-c",
				`nohup /usr/local/bin/ic-healthd run > /var/log/ic-healthd.log 2>&1 &`)
			if err != nil {
				return err
			}
		}

		return nil
	})

	return inst, []client.Resource{img, volume}, nil
}

// parseHealthdNetwork decodes the healthd network selector. An empty value means
// the project's default network, or the shared daemon's own bridge. A
// "<project>:<network>" value references a managed network that must already
// exist; anything else is a host bridge name.
func parseHealthdNetwork(c *client.Client, network string, global bool) (sidecarNetworkRef, error) {
	if network == "" {
		if global {
			return sidecarNetworkRef{name: globalHealthdNetwork, deflt: true, incusName: globalHealthdNetwork}, nil
		}

		return sidecarNetworkRef{name: "default", deflt: true}, nil
	}

	if strings.Contains(network, ":") {
		p, n, _ := strings.Cut(network, ":")

		if p == "" {
			p = c.Project()
		}
		if n == "" || strings.Contains(n, ":") {
			return sidecarNetworkRef{}, errors.New("`--healthd-network` is wrong, need something like `<project>:<network>` or `<bridge>`")
		}

		return sidecarNetworkRef{project: p, name: n}, nil
	}

	return sidecarNetworkRef{name: network}, nil
}

// healthdConfigDrift names the settings params asks for that the running daemon
// does not have. First creator wins, so these are what a later project loses.
func healthdConfigDrift(params healthdParams, config map[string]string) []string {
	var drift []string

	want := map[string]string{}
	if params.incus != nil {
		want["incus"] = params.incus.String()
	}
	if params.workers > 0 {
		want["workers"] = strconv.Itoa(params.workers)
	}
	if params.restartWorkers > 0 {
		want["restart-workers"] = strconv.Itoa(params.restartWorkers)
	}

	env := map[string]string{
		"incus":           "environment.INCUS_COMPOSE_HEALTHD_INCUS",
		"workers":         "environment.INCUS_COMPOSE_HEALTHD_WORKERS",
		"restart-workers": "environment.INCUS_COMPOSE_HEALTHD_RESTART_WORKERS",
	}

	for name, value := range want {
		if config[env[name]] != value {
			drift = append(drift, name)
		}
	}

	for key, value := range params.xIncus {
		if config[key] != value {
			drift = append(drift, "x-incus."+key)
		}
	}

	slices.Sort(drift)

	return drift
}

// healthdEnsureNetwork brings up the bridge the sidecar attaches to.
func healthdEnsureNetwork(ctx context.Context, c *client.Client, params healthdParams) (*client.Network, error) {
	ref, err := parseHealthdNetwork(c, params.network, params.global)
	if err != nil {
		return nil, err
	}

	return sidecarEnsureNetwork(ctx, c, ref, "healthd")
}

// healthdIncusURL is the endpoint the sidecar dials: --healthd-incus, then
// core.https_address once it names a host, then the bridge gateway.
func healthdIncusURL(c *client.Client, params healthdParams, network *client.Network) (*url.URL, error) {
	return sidecarIncusURL(c, params.incus, network, "ic-healthd", "INCUS_COMPOSE_HEALTHD_INCUS")
}

// healthdTeardown removes a healthd sidecar, its volume and its certificate
// from the project c belongs to.
func healthdTeardown(ctx context.Context, c *client.Client, global bool, timeout time.Duration) error {
	stack := client.NewStack(c, client.StackSortDescending())

	volRes, err := c.Resource(client.KindStorageVolume, healthdVolume, &client.StorageVolumeConfig{})
	if err != nil {
		return fmt.Errorf("getting the healthd volume resource: %w", err)
	}
	stack.Add(volRes)

	instRes, err := c.Resource(
		client.KindInstance,
		healthdInstanceName(c.IncusProject(), global),
		&client.InstanceConfig{},
	)
	if err != nil {
		return fmt.Errorf("getting the healthd instance resource: %w", err)
	}
	stack.Add(instRes)

	c.LogDebug("Ensure", "resources", stack.All())

	var errs error
	if err := stack.ForAction(client.ActionEnsure).Run(ctx, client.ActionEnsure); err != nil {
		errs = errors.Join(errs, fmt.Errorf("ensuring healthd: %w", err))
	}

	runOpts := []client.Option{client.OptionForce(), client.OptionTimeout(timeout)}

	if err := stack.ForAction(client.ActionStop).Run(ctx, client.ActionStop, runOpts...); err != nil {
		errs = errors.Join(errs, fmt.Errorf("stopping healthd resources: %w", err))
	}

	if err := stack.ForAction(client.ActionDelete).Run(ctx, client.ActionDelete, runOpts...); err != nil {
		errs = errors.Join(errs, fmt.Errorf("deleting healthd resources: %w", err))
	}

	if err := healthdRevokeCert(ctx, c, global); err != nil {
		errs = errors.Join(errs, fmt.Errorf("revoking the healthd cert: %w", err))
	}

	return errs
}

// healthdResolve returns the daemon watching p and the client of the project it
// lives in, erroring when there is none.
func healthdResolve(p *project.Project, c *client.Client) (*client.Client, *client.Instance, error) {
	hc, _, err := healthdClient(p, c)
	if err != nil {
		return nil, nil, err
	}

	name, err := hc.FindHealthd()
	if err != nil {
		return nil, nil, fmt.Errorf("finding healthd: %w", err)
	}

	res, err := hc.Resource(client.KindInstance, name, &client.InstanceConfig{})
	if err != nil {
		return nil, nil, err
	}
	inst, ok := res.(*client.Instance)
	if !ok {
		return nil, nil, errors.New("unexpected resource type for healthd")
	}

	return hc, inst, nil
}

func newHealthdStatusCommand() *cli.Command {
	return &cli.Command{
		Name:  "status",
		Usage: "Prints the status of healthd",
		Action: func(ctx context.Context, cmd *cli.Command) error {
			globalClient, err := clientFromContext(ctx)
			if err != nil {
				return err
			}
			if err := globalClient.Connect(); err != nil {
				return err
			}

			target, done, err := resolveHealthdTarget(ctx, cmd, globalClient)
			if err != nil {
				globalClient.LogError("Finding healthd", "error", err)
				return errLogged.Wrap(err)
			}
			defer done()

			err = client.RunAction(ctx, target.instance, client.ActionEnsure)
			if err != nil {
				return fmt.Errorf("while fetching healthd: %w", err)
			}

			state := target.instance.State()
			if state == nil {
				return errors.New("no healthd state after fetch")
			}

			_, _ = fmt.Fprint(cmd.Root().Writer, state.IncusInstance.Config[shared.HealthStatusKey])

			return nil
		},
	}
}

func newHealthdCommand() *cli.Command {
	return &cli.Command{
		Name:     "healthd",
		Usage:    "Manage the ic-healthd sidecar",
		Category: "extensions",
		Commands: []*cli.Command{
			newHealthdLogsCommand(),
			newHealthdReloadCommand(),
			newHealthdRestartCommand(),
			newHealthdStatusCommand(),
			newHealthdUpCommand(),
			newHealthdDownCommand(),
		},
	}
}
