package main

import (
	"context"
	"fmt"
	"maps"
	"os"
	"os/signal"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	incusApi "github.com/lxc/incus/v7/shared/api"
	"github.com/mattn/go-isatty"
	"github.com/urfave/cli/v3"

	"github.com/lxc/incus-compose/client"
	"github.com/lxc/incus-compose/project"
	"github.com/lxc/incus-compose/shared"
)

func newUpCommand() *cli.Command {
	return &cli.Command{
		Name:      "up",
		Usage:     "Create and start containers",
		Category:  "compose",
		ArgsUsage: "[SERVICE...]",
		Flags: []cli.Flag{
			&cli.BoolFlag{
				Name:  "recreate",
				Usage: "Recreate containers by deleting them first",
			},
			&cli.BoolFlag{
				Name:    "no-start",
				Usage:   "Don't start containers after creating (implies --detach)",
				Sources: cli.EnvVars("INCUS_COMPOSE_UP_NO_START"),
			},
			&cli.DurationFlag{
				Name:    "timeout",
				Usage:   "Timeout for stopping/starting a service",
				Value:   2 * time.Minute,
				Sources: cli.EnvVars("INCUS_COMPOSE_UP_TIMEOUT"),
			},
			&cli.DurationFlag{
				Name:    "dependency-timeout",
				Usage:   "Max time to wait for service_healthy depends_on (0 = no limit)",
				Value:   5 * time.Minute,
				Sources: cli.EnvVars("INCUS_COMPOSE_UP_DEPENDENCY_TIMEOUT"),
			},
			&cli.StringSliceFlag{
				Name:    "scale",
				Usage:   "Scale SERVICE to NUM instances (service=num)",
				Sources: cli.EnvVars("INCUS_COMPOSE_SCALE"),
			},
			&cli.StringFlag{
				Name:    "pull",
				Usage:   `Pull image before running ("always"|"missing"|"never"|"policy")`,
				Value:   "policy",
				Sources: cli.EnvVars("INCUS_COMPOSE_UP_PULL"),
			},
			&cli.BoolFlag{
				Name:    "build",
				Usage:   "Build images before starting containers",
				Sources: cli.EnvVars("INCUS_COMPOSE_UP_BUILD"),
			},
			&cli.StringFlag{
				Name:    "builder",
				Usage:   "Preferred builder, binary name or absolute path. Empty for auto-detect.",
				Sources: cli.EnvVars("INCUS_COMPOSE_UP_BUILDER"),
			},
			&cli.BoolFlag{
				Name:    "no-build",
				Usage:   "Do not build images even if missing",
				Sources: cli.EnvVars("INCUS_COMPOSE_UP_NO_BUILD"),
			},
			&cli.BoolFlag{
				Name:    "no-deps",
				Usage:   "Don't start linked services",
				Sources: cli.EnvVars("INCUS_COMPOSE_UP_NO_DEPS"),
			},
			&cli.BoolFlag{
				Name:    "detach",
				Aliases: []string{"d"},
				Usage:   "Detached mode: run containers in the background",
				Sources: cli.EnvVars("INCUS_COMPOSE_UP_DETACH"),
			},
			&cli.BoolFlag{
				Name:    "no-healthd",
				Usage:   "Don't create healthd sidecar for healthchecks",
				Sources: cli.EnvVars("INCUS_COMPOSE_NO_HEALTHD"),
			},
			&cli.BoolFlag{
				Name:    "external-healthd",
				Usage:   "Use healthd but do not try to create or lookup it",
				Sources: cli.EnvVars("INCUS_COMPOSE_EXTERNAL_HEALTHD"),
			},
			&cli.StringFlag{
				Name:    "healthd-binary",
				Usage:   "Path to local ic-healthd binary (uses images:alpine/edge instead of OCI image)",
				Sources: cli.EnvVars("INCUS_COMPOSE_HEALTHD_BINARY"),
			},
			&cli.StringFlag{
				Name:    "healthd-incus",
				Usage:   `Connection URL of the incus to connect to from inside the sidecar. Empty = detect the ip from the bridge we are connected too`,
				Sources: cli.EnvVars("INCUS_COMPOSE_HEALTHD_INCUS"),
			},
			&cli.StringFlag{
				Name:    "healthd-network",
				Usage:   "Incus bridge for healthd to use (default: the network of the project it runs in)",
				Sources: cli.EnvVars("INCUS_COMPOSE_HEALTHD_NETWORK"),
			},
			&cli.StringFlag{
				Name:    "healthd-scope",
				Usage:   "Which healthd watches this project: `global` (shared, in its own project) or `project` (a sidecar of its own); loses to a scope the project already carries",
				Sources: cli.EnvVars("INCUS_COMPOSE_HEALTHD_SCOPE"),
			},
			&cli.BoolFlag{
				Name:    "no-dns",
				Usage:   "Don't start or configure DNS for the project",
				Sources: cli.EnvVars("INCUS_COMPOSE_NO_DNS", "INCUS_COMPOSE_DISABLE_DNS"),
			},
			&cli.StringFlag{
				Name:    "network-driver",
				Usage:   `Network driver to use: "auto" (default), "ovn", or "bridge"`,
				Sources: cli.EnvVars("INCUS_COMPOSE_NETWORK_DRIVER"),
				Validator: func(v string) error {
					if v == "" {
						return nil
					}
					switch v {
					case "auto", "ovn", "bridge":
						return nil
					default:
						return fmt.Errorf("invalid network-driver %q: must be auto, ovn, or bridge", v)
					}
				},
			},
			&cli.StringFlag{
				Name:    "network-uplink",
				Usage:   `Uplink network for OVN networks (e.g. "incusbr0")`,
				Sources: cli.EnvVars("INCUS_COMPOSE_NETWORK_UPLINK"),
			},
			&cli.BoolFlag{
				Name:    "allow-local-storage",
				Usage:   `Allow multi-replica deployments on a cluster even when the storage pool is not shared`,
				Sources: cli.EnvVars("INCUS_COMPOSE_ALLOW_LOCAL_STORAGE"),
			},
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			noColor := noColor(ctx)

			globalClient, err := clientFromContext(ctx)
			if err != nil {
				return err
			}
			if err := globalClient.Connect(); err != nil {
				return err
			}

			p, err := project.New().Load(ctx, buildLoadOptions(cmd)...)
			if err != nil {
				globalClient.LogError("Loading the project", "error", err)
				return errLogged.Wrap(err)
			}

			if cmd.Args().Len() > 0 {
				for _, s := range cmd.Args().Slice() {
					_, ok := p.Services[s]
					if !ok {
						err := client.ErrNotFound.WithKindName(client.KindInstance, s)
						globalClient.LogError("Service not found", "service", s)
						return errLogged.Wrap(err)
					}
				}
			}

			if cmd.Bool("external-healthd") {
				p.ClientConfig.Healthd.External = true
			}
			if cmd.Bool("allow-local-storage") {
				p.ClientConfig.AllowLocalStorage = true
			}

			c, err := globalClient.EnsureProject(
				p.Name,
				client.EnsureProjectWithCreate(),
				client.EnsureProjectWithConfig(p.ClientConfig.XIncus),
				client.EnsureProjectWithNetworkDriver(p.ClientConfig.Network.Driver),
			)
			if err != nil {
				globalClient.LogError("Getting the incus project", "error", err)
				return errLogged.Wrap(err)
			}
			defer c.WarnError(c.Done, "Failure during Client.Done()")

			err = c.Open()
			if err != nil {
				globalClient.LogError("Opening the project client", "error", err)
				return errLogged.Wrap(err)
			}

			c.LogDebug("Running on network type", "type", c.NetworkType())

			// The recreate client has own errors it ignores and it registers
			// its own hooks (DNSWatcher).
			rc := c.Clone()

			// We start all resources, just ignore that warning but let progress know them (so add before - LIFO - progress runs before).
			c.IgnoreError(client.ActionStart, client.ErrRunning)
			c.IgnoreError(client.ActionStop, client.ErrNotRunning)

			// Register the DNS Watcher after the progress renderer so progress waits for the dns changes.
			err = c.RegisterDNSWatcher()
			if err != nil {
				globalClient.LogError("Registering the DNS watcher", "project", p.Name, "error", err)
				return errLogged.Wrap(err)
			}

			usesHealthd := !cmd.Bool("no-healthd")
			if usesHealthd && !healthdInUseByProject(globalClient, p) {
				usesHealthd = false
			}

			buildMode := client.BuildAuto
			if cmd.Bool("build") {
				buildMode = client.BuildForce
			} else if cmd.Bool("no-build") {
				buildMode = client.BuildNever
			}
			buildInfo := client.BuildInfo{
				Mode:             buildMode,
				PreferredBuilder: cmd.String("builder"),
			}

			runOptions := []client.Option{client.OptionTimeout(cmd.Duration("timeout"))}
			if !p.ClientConfig.Healthd.External && !usesHealthd {
				runOptions = append(runOptions, client.OptionNoHealthd())
			}

			if p.ClientConfig.Healthd.External {
				runOptions = append(runOptions, client.OptionExternalHealthd())
			}

			recreate := cmd.Bool("recreate")

			scale := parseScale(cmd.StringSlice("scale"))
			args := filterResourcesArgs{
				OnlyServices:     cmd.Args().Slice(),
				WithDependencies: !cmd.Bool("no-deps"),
				Reverse:          recreate,
			}

			// "missing" and the legacy "policy" are the default, as is anything unknown.
			pullMode := client.PullMissing
			switch cmd.String("pull") {
			case "always":
				pullMode = client.PullAlways
			case "never":
				pullMode = client.PullNever
			}

			// The ensure below carries no pull mode, so this is where --pull
			// lands. Built images stay with builtServices, healthd with healthdUp.
			err = pull(ctx, p, c, pullArgs{
				Services:        cmd.Args().Slice(),
				WithDeps:        !cmd.Bool("no-deps"),
				IgnoreBuildable: true,
				Pull:            pullMode,
				Scale:           scale,
				Workers:         cmd.Root().Int("workers"),
				Debug:           cmd.Root().Bool("debug"),
				Writer:          cmd.Root().Writer,
			})
			if err != nil {
				return err
			}

			// A rebuilt image only reaches an instance created from it again.
			downServices, downNoDeps := cmd.Args().Slice(), cmd.Bool("no-deps")
			if !recreate && buildMode == client.BuildForce {
				downServices = builtServices(p, args)
				downNoDeps = true
				recreate = len(downServices) > 0
				c.LogDebug("Recreating built services", "services", downServices)
			}

			if recreate {
				err = down(ctx, p, rc, downArgs{
					Project:    cmd.Bool("project"),
					Volumes:    false,
					Images:     false,
					Timeout:    cmd.Duration("timeout"),
					NoDeps:     downNoDeps,
					NoNetworks: len(downServices) != 0,
					Services:   downServices,
					Workers:    cmd.Root().Int("workers"),
					Debug:      cmd.Root().Bool("debug"),
					Scale:      scale,
					Writer:     cmd.Root().Writer,
					Reverse:    true,
					NoHealthd:  !usesHealthd,

					// A delete that failed here must not look like success: the
					// ensure below would accept the surviving instance and exit 0.
					ReportErrors: true,
				})
				if err != nil {
					return err
				}
			}

			if usesHealthd && !cmd.Bool("external-healthd") {
				err = healthdUp(ctx, p, c, healthdUpArgs{
					Binary:  cmd.String("healthd-binary"),
					Image:   healthdImage(),
					Incus:   cmd.String("healthd-incus"),
					Network: cmd.String("healthd-network"),
					Scope:   cmd.String("healthd-scope"),
					Pull:    cmd.String("pull"),
					Timeout: cmd.Duration("timeout"),
					Workers: cmd.Root().Int("workers"),
					Debug:   cmd.Root().Bool("debug"),
					Trace:   cmd.Root().Bool("trace"),
					Writer:  cmd.Root().Writer,
				})
				if err != nil {
					return err
				}
			}

			var dnsConfigs map[string]string
			if cmd.Bool("no-dns") {
				p.ClientConfig.DNS.Disabled = true
			}

			usesDNS := !p.ClientConfig.DNS.Disabled
			if usesDNS {
				err = dnsUp(ctx, p, c, dnsUpArgs{
					Image:   dnsImage(),
					Pull:    cmd.String("pull"),
					Timeout: cmd.Duration("timeout"),
					Workers: cmd.Root().Int("workers"),
					Debug:   cmd.Root().Bool("debug"),
					Trace:   cmd.Root().Bool("trace"),
					Writer:  cmd.Root().Writer,
				})
				if err != nil {
					return err
				}

				dnsIP, err := resolveDNSIP(ctx, c, cmd.Duration("timeout"))
				if err != nil {
					c.LogError("Getting dns daemon IP", "error", err)
					return errLogged.Wrap(err)
				}

				zone := p.ClientConfig.DNS.Zone
				if zone == "" {
					pConfig, pErr := c.Global().ProjectConfig(p.Name)
					if pErr == nil && pConfig[shared.DNSZoneKey] != "" {
						zone = pConfig[shared.DNSZoneKey]
					} else {
						zone = p.Name + "." + project.DefaultDNSZoneSuffix
					}
				}
				dnsProjectConfig := map[string]string{shared.DNSZoneKey: zone}
				if p.ClientConfig.DNS.Transfer || len(p.ClientConfig.DNS.AllowTransfer) > 0 {
					dnsProjectConfig[shared.DNSTransferKey] = "true"
				}
				err = c.Global().UpdateProjectConfig(p.Name, dnsProjectConfig)
				if err != nil {
					c.LogError("Updating project dns zone", "error", err)
					return errLogged.Wrap(err)
				}

				dnsConfigs = map[string]string{
					"oci.dns.nameservers": dnsIP,
					"oci.dns.search":      strings.TrimSuffix(zone, "."),
				}

				if p.InstanceMarks == nil {
					p.InstanceMarks = map[string]string{}
				}
				maps.Copy(p.InstanceMarks, dnsConfigs)
			}

			var progress *progressRenderer
			if !cmd.Root().Bool("debug") {
				progress = newProgressRenderer(cmd.Root().Writer, noColor, isatty.IsTerminal(os.Stdout.Fd()))
				progress.Start(c)
				defer progress.Stop(c)
			}

			resources, err := p.Resources(c, project.ResourcesScale(scale))
			if err != nil {
				c.LogError("Getting project resources in reCreate", "error", err)
				return errLogged.Wrap(err)
			}

			order, err := p.ServiceOrder(false)
			if err != nil {
				c.LogError("Getting the service dependency order", "error", err)
				return errLogged.Wrap(err)
			}

			myResources := filterResources(p, resources, args)

			stack := client.NewStack(c, client.StackWorkers(cmd.Root().Int("workers")), client.StackFailFast())
			stack.AddOrdered(order, myResources)

			c.LogDebug("Ensure", "resources", stack.All())

			startOptions := append(append([]client.Option{}, runOptions...), client.OptionCreate())
			if buildInfo.Mode != client.BuildAuto || buildInfo.PreferredBuilder != "" {
				startOptions = append(startOptions, client.OptionBuild(buildInfo))
			}
			if cmd.Duration("dependency-timeout") > 0 {
				startOptions = append(startOptions, client.OptionDependencyTimeout(cmd.Duration("dependency-timeout")))
			}

			err = stack.ForAction(client.ActionEnsure).Run(ctx, client.ActionEnsure, startOptions...)
			if err != nil {
				c.LogError("Ensuring resources", "error", err)
				return errLogged.Wrap(err)
			}

			// A pulled image only reaches an instance created from it again, so
			// the ones the ensure above found on the previous fingerprint are
			// torn down and made from the new one, as are the ones created from
			// an older service config.
			stale := staleInstances(myResources, pullMode)
			if len(stale) > 0 {
				c.LogDebug("Recreating outdated instances", "instances", stale)

				myResources, stack, err = recreateStaleInstances(ctx, p, c, recreateStaleArgs{
					Stale:        stale,
					Scale:        scale,
					FilterArgs:   args,
					Order:        order,
					Workers:      cmd.Root().Int("workers"),
					Timeout:      cmd.Duration("timeout"),
					StartUpdated: !cmd.Bool("no-start"),
					StartOptions: startOptions,
					DNSConfigs:   dnsConfigs,
				})
				if err != nil {
					return err
				}
			}

			if len(dnsConfigs) > 0 {
				for _, res := range myResources {
					for _, r := range res {
						inst, ok := r.(*client.Instance)
						if ok && inst.IsEnsured() {
							err = inst.AddConfigs(ctx, dnsConfigs)
							if err != nil {
								c.LogError("Adding DNS configs to instance", "instance", inst.Name(), "error", err)
								return errLogged.Wrap(err)
							}
						}
					}
				}
			}

			// Start
			startFilter := func(r client.Resource) bool {
				if cmd.Bool("no-start") && r.Kind() == client.KindInstance {
					return false
				}

				return r.IsEnsured()
			}

			err = stack.ForActionF(client.ActionStart, startFilter).Run(ctx, client.ActionStart, startOptions...)
			if err != nil {
				c.LogError("Starting resources", "error", err)
				return errLogged.Wrap(err)
			}

			// Nothing was started, so there is nothing to stream logs from.
			if cmd.Bool("detach") || cmd.Bool("no-start") {
				_ = c.Done()
				return nil
			}

			// Stop rendering progress before streaming logs; it is idempotent so
			// the deferred call above becomes a no-op.
			if progress != nil {
				progress.Stop(c)
			}

			logsCtx, stopNotify := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
			defer stopNotify()

			_ = logs(logsCtx, c, logsArgs{
				Instances: projectInstances(c, p, slices.Sorted(maps.Keys(myResources))...),
				Follow:    true,
				Writer:    cmd.Root().Writer,
			})

			// The interrupt that ended the log stream should also tear down
			// what up started, mirroring the `down` command.
			return down(ctx, p, rc, downArgs{
				Timeout:    cmd.Duration("timeout"),
				NoDeps:     cmd.Bool("no-deps"),
				NoNetworks: true,
				Services:   cmd.Args().Slice(),
				Workers:    cmd.Root().Int("workers"),
				Debug:      cmd.Root().Bool("debug"),
				Scale:      scale,
				Writer:     cmd.Root().Writer,
				Reverse:    true,
				NoHealthd:  !usesHealthd,
			})
		},
	}
}

// builtServices returns the in-scope services whose image comes from a build
// config, sorted. A service that only consumes such an image is in there too.
func builtServices(p *project.Project, args filterResourcesArgs) []string {
	// filterResources selects by service name; the resources are just payload.
	scope := map[string][]client.Resource{}
	for name := range p.Services {
		scope[name] = nil
	}

	images := map[string]bool{}
	for _, s := range p.Services {
		if s.Build != nil && s.Image != "" {
			images[s.Image] = true
		}
	}

	services := []string{}
	for name := range filterResources(p, scope, args) {
		s := p.Services[name]
		if s.Build != nil || images[s.Image] {
			services = append(services, name)
		}
	}
	slices.Sort(services)

	return services
}

// staleInstances returns the ensured instances created from an older service
// config or, under PullAlways, running an image other than the one their
// service holds now, sorted by Incus name.
func staleInstances(resources map[string][]client.Resource, pullMode client.PullMode) []client.Resource {
	stale := []client.Resource{}

	for _, res := range resources {
		var image *client.Image
		for _, r := range res {
			i, ok := r.(*client.Image)
			if ok {
				image = i
				break
			}
		}

		if image == nil {
			continue
		}

		for _, r := range res {
			instance, ok := r.(*client.Instance)
			if !ok {
				continue
			}

			// Nil for one the ensure just created, which already runs the new image.
			info := instance.State().IncusInstance
			if info == nil {
				continue
			}

			if instance.ConfigChanged() || pullMode == client.PullAlways && pulledImageChanged(info.Config["volatile.base_image"], image.State().IncusAlias) {
				stale = append(stale, instance)
			}
		}
	}

	slices.SortFunc(stale, func(a, b client.Resource) int {
		return strings.Compare(a.IncusName(), b.IncusName())
	})

	return stale
}

// pulledImageChanged reports whether the pulled alias differs from the image
// the instance runs today.
func pulledImageChanged(baseImage string, alias *incusApi.ImageAliasesEntry) bool {
	return alias != nil && alias.Target != "" && alias.Target != baseImage
}

// parseScale parses --scale flags of the form "service=num".
func parseScale(values []string) map[string]int {
	scaleOverrides := make(map[string]int)
	for _, s := range values {
		parts := strings.SplitN(s, "=", 2)
		if len(parts) == 2 {
			if n, err := strconv.Atoi(parts[1]); err == nil {
				scaleOverrides[parts[0]] = n
			}
		}
	}
	return scaleOverrides
}

type recreateStaleArgs struct {
	Stale        []client.Resource
	Scale        map[string]int
	FilterArgs   filterResourcesArgs
	Order        []string
	Workers      int
	Timeout      time.Duration
	StartUpdated bool
	StartOptions []client.Option
	DNSConfigs   map[string]string
}

type staleBatch struct {
	service       string
	instances     []client.Resource
	delay         time.Duration
	failureAction string
	startInBatch  bool
}

// buildStaleBatches groups stale instances into recreate batches: services with
// deploy.update_config are split into batches of size parallelism, while all
// other stale instances are recreated together in a single batch.
func buildStaleBatches(p *project.Project, c *client.Client, stale []client.Resource, order []string) []staleBatch {
	byService := map[string][]client.Resource{}
	var unbatched []client.Resource

	for _, r := range stale {
		inst, ok := r.(*client.Instance)
		if !ok {
			continue
		}
		svc, hasSvc := p.Services[inst.Config.ServiceName]
		if hasSvc && svc.Deploy != nil && svc.Deploy.UpdateConfig != nil {
			byService[inst.Config.ServiceName] = append(byService[inst.Config.ServiceName], inst)
		} else {
			unbatched = append(unbatched, inst)
		}
	}

	var batches []staleBatch
	if len(unbatched) > 0 {
		batches = append(batches, staleBatch{
			instances:     unbatched,
			failureAction: "pause",
		})
	}

	for _, svcName := range order {
		svcStale := byService[svcName]
		if len(svcStale) == 0 {
			continue
		}

		uc := p.Services[svcName].Deploy.UpdateConfig
		if uc.Order == "start-first" && c != nil {
			c.LogWarn(
				"deploy.update_config.order: start-first is not supported for in-place instance recreation; using stop-first",
				"service", svcName,
			)
		}

		failureAction := "pause"
		if uc.FailureAction != "" {
			failureAction = uc.FailureAction
		}

		chunks := batchStaleInstances(svcStale, uc.Parallelism)
		for idx, chunk := range chunks {
			var delay time.Duration
			if idx < len(chunks)-1 {
				delay = time.Duration(uc.Delay)
			}
			batches = append(batches, staleBatch{
				service:       svcName,
				instances:     chunk,
				delay:         delay,
				failureAction: failureAction,
				startInBatch:  true,
			})
		}
	}

	return batches
}

// batchStaleInstances splits a service's stale instances into batches of size
// parallelism (1 when parallelism is nil, or a single batch when *parallelism == 0).
func batchStaleInstances(instances []client.Resource, parallelism *uint64) [][]client.Resource {
	if len(instances) == 0 {
		return nil
	}

	batchSize := 1
	if parallelism != nil {
		if *parallelism == 0 || int(*parallelism) >= len(instances) {
			return [][]client.Resource{instances}
		}
		batchSize = int(*parallelism)
	}

	var batches [][]client.Resource
	for i := 0; i < len(instances); i += batchSize {
		end := min(i+batchSize, len(instances))
		batches = append(batches, instances[i:end])
	}

	return batches
}

// recreateStaleInstances tears down and recreates outdated instances, honoring
// deploy.update_config batching when configured.
func recreateStaleInstances(ctx context.Context, p *project.Project, c *client.Client, args recreateStaleArgs) (map[string][]client.Resource, *client.Stack, error) {
	var myResources map[string][]client.Resource
	var stack *client.Stack

	for _, batch := range buildStaleBatches(p, c, args.Stale, args.Order) {
		batchNames := make(map[string]bool, len(batch.instances))
		for _, r := range batch.instances {
			batchNames[r.IncusName()] = true
		}

		downStack := client.NewStack(c, client.StackSortDescending(), client.StackWorkers(args.Workers))
		downStack.Add(batch.instances...)

		err := downStack.ForAction(client.ActionStop).Run(ctx, client.ActionStop, client.OptionTimeout(args.Timeout))
		if err != nil {
			if batch.failureAction == "continue" {
				c.LogWarn("Rolling update failed stopping batch, continuing", "service", batch.service, "error", err)
				continue
			}
			c.LogError("Stopping outdated instances", "error", err)
			return nil, nil, errLogged.Wrap(err)
		}

		err = downStack.ForAction(client.ActionDelete).Run(ctx, client.ActionDelete, client.OptionTimeout(args.Timeout))
		if err != nil {
			if batch.failureAction == "continue" {
				c.LogWarn("Rolling update failed deleting batch, continuing", "service", batch.service, "error", err)
				continue
			}
			c.LogError("Deleting outdated instances", "error", err)
			return nil, nil, errLogged.Wrap(err)
		}

		// Delete drops them from the client, so Start below runs on
		// the resources this rebuild registers rather than the dead ones.
		resources, err := p.Resources(c, project.ResourcesScale(args.Scale))
		if err != nil {
			c.LogError("Getting project resources after the teardown", "error", err)
			return nil, nil, errLogged.Wrap(err)
		}

		myResources = filterResources(p, resources, args.FilterArgs)
		stack = client.NewStack(c, client.StackWorkers(args.Workers), client.StackFailFast())
		stack.AddOrdered(args.Order, myResources)

		inBatch := func(r client.Resource) bool {
			return batchNames[r.IncusName()]
		}

		err = stack.ForActionF(client.ActionEnsure, inBatch).Run(ctx, client.ActionEnsure, args.StartOptions...)
		if err != nil {
			if batch.failureAction == "continue" {
				c.LogWarn("Rolling update failed ensuring batch, continuing", "service", batch.service, "error", err)
				continue
			}
			c.LogError("Ensuring the recreated instances", "error", err)
			return nil, nil, errLogged.Wrap(err)
		}

		if batch.startInBatch && args.StartUpdated {
			if len(args.DNSConfigs) > 0 {
				for _, r := range myResources[batch.service] {
					inst, ok := r.(*client.Instance)
					if ok && batchNames[inst.IncusName()] && inst.IsEnsured() {
						err = inst.AddConfigs(ctx, args.DNSConfigs)
						if err != nil {
							c.LogError("Adding DNS configs to instance", "instance", inst.Name(), "error", err)
							return nil, nil, errLogged.Wrap(err)
						}
					}
				}
			}

			err = stack.ForActionF(client.ActionStart, inBatch).Run(ctx, client.ActionStart, args.StartOptions...)
			if err != nil {
				if batch.failureAction == "continue" {
					c.LogWarn("Rolling update failed starting batch, continuing", "service", batch.service, "error", err)
					continue
				}
				c.LogError("Starting recreated instances", "service", batch.service, "error", err)
				return nil, nil, errLogged.Wrap(err)
			}
		}

		if batch.delay > 0 {
			t := time.NewTimer(batch.delay)
			select {
			case <-ctx.Done():
				t.Stop()
				return nil, nil, ctx.Err()
			case <-t.C:
			}
		}
	}

	return myResources, stack, nil
}
