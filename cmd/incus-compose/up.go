package main

import (
	"context"
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
				Name:    "healthd-image",
				Usage:   `Healthd OCI image to use; {version} is replaced with the incus-compose version`,
				Value:   DefaultHealthdImage,
				Sources: cli.EnvVars("INCUS_COMPOSE_HEALTHD_IMAGE"),
			},
			&cli.StringFlag{
				Name:    "init",
				Usage:   "Image the `run` helper comes from",
				Value:   DefaultInitImage,
				Sources: cli.EnvVars("INCUS_COMPOSE_INIT_IMAGE"),
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

			c, err := globalClient.EnsureProject(
				p.Name,
				client.EnsureProjectWithCreate(),
				client.EnsureProjectWithConfig(p.ClientConfig.XIncus),
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

			scale := parseScale(cmd.StringSlice("scale"))
			args := filterResourcesArgs{
				OnlyServices:     cmd.Args().Slice(),
				WithDependencies: !cmd.Bool("no-deps"),
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
				NoHealthd:       true,
				Init:            cmd.String("init"),
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
			recreate := cmd.Bool("recreate")
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
					Image:   cmd.String("healthd-image"),
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
			// torn down and made from the new one.
			if pullMode == client.PullAlways {
				stale := staleInstances(myResources)
				if len(stale) > 0 {
					c.LogDebug("Recreating instances on a replaced image", "instances", stale)

					downStack := client.NewStack(c, client.StackSortDescending(), client.StackWorkers(cmd.Root().Int("workers")))
					downStack.Add(stale...)

					err = downStack.ForAction(client.ActionStop).Run(ctx, client.ActionStop, client.OptionTimeout(cmd.Duration("timeout")))
					if err != nil {
						c.LogError("Stopping instances on a replaced image", "error", err)
						return errLogged.Wrap(err)
					}

					err = downStack.ForAction(client.ActionDelete).Run(ctx, client.ActionDelete, client.OptionTimeout(cmd.Duration("timeout")))
					if err != nil {
						c.LogError("Deleting instances on a replaced image", "error", err)
						return errLogged.Wrap(err)
					}

					// Delete drops them from the client, so Start below runs on
					// the resources this rebuild registers rather than the dead ones.
					resources, err = p.Resources(c, project.ResourcesScale(scale))
					if err != nil {
						c.LogError("Getting project resources after the teardown", "error", err)
						return errLogged.Wrap(err)
					}

					myResources = filterResources(p, resources, args)

					stack = client.NewStack(c, client.StackWorkers(cmd.Root().Int("workers")), client.StackFailFast())
					stack.AddOrdered(order, myResources)

					// Everything else came through the ensure above; the deleted
					// instances are what the rebuild handed back unensured.
					deleted := func(r client.Resource) bool { return !r.IsEnsured() }

					err = stack.ForActionF(client.ActionEnsure, deleted).Run(ctx, client.ActionEnsure, startOptions...)
					if err != nil {
						c.LogError("Ensuring the recreated instances", "error", err)
						return errLogged.Wrap(err)
					}
				}
			}

			// Start
			if !cmd.Bool("no-start") {
				startFilter := func(r client.Resource) bool { return r.IsEnsured() }

				err := stack.ForActionF(client.ActionStart, startFilter).Run(ctx, client.ActionStart, startOptions...)
				if err != nil {
					c.LogError("Starting resources", "error", err)
					return errLogged.Wrap(err)
				}
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
				Instances: projectInstances(c, p),
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

// staleInstances returns the ensured instances running an image other than the
// one their service holds now, sorted by Incus name.
func staleInstances(resources map[string][]client.Resource) []client.Resource {
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

			if pulledImageChanged(info.Config["volatile.base_image"], image.State().IncusAlias) {
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
