package main

import (
	"context"
	"io"
	"os"
	"time"

	"github.com/mattn/go-isatty"
	"github.com/urfave/cli/v3"

	"github.com/lxc/incus-compose/client"
	"github.com/lxc/incus-compose/project"
)

type downArgs struct {
	Project    bool
	Volumes    bool
	Images     bool
	Timeout    time.Duration
	NoDeps     bool
	NoNetworks bool
	Services   []string
	Workers    int
	Debug      bool
	Scale      map[string]int
	Writer     io.Writer
	Reverse    bool
	NoHealthd  bool

	// ReportErrors makes down return a teardown failure instead of only logging
	// it. `up --recreate` sets it, because it calls down() before the ensure
	// phase and a delete that silently failed leaves that ensure accepting the
	// surviving instance — a recreate that exits 0 and changes nothing. A plain
	// `down` stays best-effort.
	ReportErrors bool
}

// down stops and removes the project's resources.
func down(ctx context.Context, p *project.Project, c *client.Client, args downArgs) error {
	noColor := noColor(ctx)

	// We start all resources, just ignore that warning but let progress know them (so add before - LIFO - progress runs before).
	c.IgnoreError(client.ActionStop, client.ErrNotEnsured)
	c.IgnoreError(client.ActionStop, client.ErrNotRunning)
	c.IgnoreError(client.ActionEnsure, client.ErrNotFound)
	c.IgnoreError(client.ActionDelete, client.ErrNotEnsured)
	c.IgnoreError(client.ActionDelete, client.ErrNotFound)

	// Replicas of a service share one volume, so all but the last delete says this.
	c.IgnoreError(client.ActionDelete, client.ErrVolumeInUse)

	if !args.Debug {
		progress := newProgressRenderer(args.Writer, noColor, isatty.IsTerminal(os.Stdout.Fd()))
		progress.Start(c)
		defer progress.Stop(c)
	}

	// Register the DNS Watcher after the progress renderer so progress waits for the dns changes.
	if err := c.RegisterDNSWatcher(); err != nil {
		c.LogError("Registering the DNS watcher", "project", p.Name, "error", err)
		return errLogged.Wrap(err)
	}

	resources, err := p.Resources(c, project.ResourcesScale(args.Scale))
	if err != nil {
		c.LogError("Getting project resources in reCreate", "error", err)
		return errLogged.Wrap(err)
	}

	filterArgs := filterResourcesArgs{
		OnlyServices:     args.Services,
		WithDependencies: !args.NoDeps,
		Reverse:          args.Reverse,
	}

	if !args.Volumes && !args.Project {
		filterArgs.ExcludeKinds = append(filterArgs.ExcludeKinds, client.KindStorageVolume)
	}

	// Do not delete networks when we are not deleting all other resources.
	if len(args.Services) > 0 || args.NoNetworks {
		filterArgs.ExcludeKinds = append(filterArgs.ExcludeKinds, client.KindNetwork)
	}

	if !args.Images {
		filterArgs.ExcludeKinds = append(filterArgs.ExcludeKinds, client.KindImage)
	}

	order, err := p.ServiceOrder(args.Reverse)
	if err != nil {
		c.LogError("Getting the service dependency order", "error", err)
		return errLogged.Wrap(err)
	}

	myResources := filterResources(p, resources, filterArgs)

	var stack *client.Stack
	if args.Reverse {
		stack = client.NewStack(c, client.StackSortDescending(), client.StackWorkers(args.Workers))
	} else {
		stack = client.NewStack(c, client.StackWorkers(args.Workers))
	}
	stack.AddOrdered(order, myResources)

	// Only a sidecar this project owns comes down with it.
	if len(args.Services) == 0 && !args.NoHealthd {
		hc, h, err := healthdResolve(p, c)
		if err == nil && hc.IncusProject() == c.IncusProject() {
			stack.Add(h)
		}
	}

	if err := stack.ForAction(client.ActionEnsure).Run(ctx, client.ActionEnsure); err != nil {
		c.LogWarn("Getting resources", "error", err)
	}

	runOpts := []client.Option{
		client.OptionForce(),
		client.OptionTimeout(args.Timeout),
	}

	// An instance brings its own volumes up, so it takes them down as well.
	if args.Volumes || args.Project {
		runOpts = append(runOpts, client.OptionVolumes())
	}

	if !p.ClientConfig.Healthd.External || args.NoHealthd {
		runOpts = append(runOpts, client.OptionNoHealthd())
	}

	errStop := stack.ForAction(client.ActionStop).Run(ctx, client.ActionStop, runOpts...)
	if errStop != nil {
		c.LogWarn("Stopping resources", "error", errStop)
	}

	errDel := stack.ForAction(client.ActionDelete).Run(ctx, client.ActionDelete, runOpts...)
	if errDel != nil {
		c.LogWarn("Deleting resources", "error", errDel)
	}

	// A one-off is nobody's declared service, so the stack above never saw it.
	if len(args.Services) == 0 {
		removeOneOffs(ctx, c, args.Timeout)
	}

	if args.Project {
		c.LogDebug("Deleting the project")
		err := c.Global().DeleteProject(c.Project(), true)
		if err != nil {
			c.LogError("Deleting the project", "error", err)
			return errLogged.Wrap(err)
		}
	}

	// Reported only now, so the one-offs and the project above are still cleaned
	// up. The kinds meaning "already gone" were filtered per resource by the
	// IgnoreError hooks at the top, so anything left here is a real failure.
	if errDel != nil && args.ReportErrors {
		return errLogged.Wrap(errDel)
	}

	return nil
}

func newDownCommand() *cli.Command {
	return &cli.Command{
		Name:      "down",
		Usage:     "Stop and remove containers",
		Category:  "compose",
		ArgsUsage: "[SERVICE...]",
		Flags: []cli.Flag{
			&cli.BoolFlag{
				Name:  "project",
				Usage: "Remove the project",
			},
			&cli.BoolFlag{
				Name:  "volumes",
				Usage: `Also delete volumes`,
			},
			&cli.StringFlag{
				Name:    "rmi",
				Usage:   `Remove images used by services. "local" for known images - all is currently the same as "local".`,
				Sources: cli.EnvVars("INCUS_COMPOSE_DOWN_RMI"),
			},
			&cli.BoolFlag{
				Name:    "images",
				Usage:   `Remove known images from the project.`,
				Sources: cli.EnvVars("INCUS_COMPOSE_DOWN_IMAGES"),
			},
			&cli.DurationFlag{
				Name:    "timeout",
				Usage:   "Timeout for stopping",
				Value:   2 * time.Minute,
				Sources: cli.EnvVars("INCUS_COMPOSE_DOWN_TIMEOUT"),
			},
			&cli.BoolFlag{
				Name:    "no-deps",
				Usage:   "Don't stop linked services",
				Sources: cli.EnvVars("INCUS_COMPOSE_DOWN_NO_DEPS"),
			},
			&cli.BoolFlag{
				Name:    "no-healthd",
				Usage:   "Don't create healthd sidecar for healthchecks",
				Sources: cli.EnvVars("INCUS_COMPOSE_NO_HEALTHD"),
			},
			&cli.BoolFlag{
				Name:    "external-healthd",
				Usage:   "Use healthd but do not try to lookup it",
				Sources: cli.EnvVars("INCUS_COMPOSE_EXTERNAL_HEALTHD"),
			},
			&cli.BoolFlag{
				Name:    "no-networks",
				Usage:   "Don't touch networks",
				Sources: cli.EnvVars("INCUS_COMPOSE_DOWN_NO_NETWORKS"),
			},
			&cli.StringSliceFlag{
				Name:    "scale",
				Usage:   "Scale SERVICE to NUM instances (service=num)",
				Sources: cli.EnvVars("INCUS_COMPOSE_SCALE"),
			},
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			globalClient, err := clientFromContext(ctx)
			if err != nil {
				return err
			}
			if err := globalClient.Connect(); err != nil {
				return err
			}

			p, err := project.New().Load(ctx, buildLoadOptions(cmd)...)
			if err != nil {
				globalClient.LogError("Configuring the project", "error", err)
				return errLogged.Wrap(err)
			}

			usesHealthd := !cmd.Bool("no-healthd")
			if usesHealthd && !healthdInUseByProject(globalClient, p) {
				usesHealthd = false
			}

			// Get the per Project client early, gives early errors if the project does not exists
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

			if err := c.Open(); err != nil {
				globalClient.LogError("Opening the project client", "project", p.Name, "error", err)
				return errLogged.Wrap(err)
			}

			return down(ctx, p, c, downArgs{
				Project:    cmd.Bool("project"),
				Volumes:    cmd.Bool("volumes"),
				Images:     cmd.Bool("images") || cmd.String("rmi") == "local" || cmd.String("rmi") == "all",
				Timeout:    cmd.Duration("timeout"),
				NoDeps:     cmd.Bool("no-deps"),
				NoNetworks: cmd.Bool("no-networks"),
				Services:   cmd.Args().Slice(),
				Workers:    cmd.Root().Int("workers"),
				Debug:      cmd.Root().Bool("debug"),
				Scale:      parseScale(cmd.StringSlice("scale")),
				Writer:     cmd.Root().Writer,
				Reverse:    true,
				NoHealthd:  !usesHealthd,
			})
		},
	}
}
