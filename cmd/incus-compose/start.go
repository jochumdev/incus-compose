package main

import (
	"context"
	"errors"
	"io"
	"os"
	"time"

	"github.com/mattn/go-isatty"
	"github.com/urfave/cli/v3"

	"github.com/lxc/incus-compose/client"
	"github.com/lxc/incus-compose/project"
)

// startArgs holds the start() options, mirroring the start command's flags.
type startArgs struct {
	Services []string
	WithDeps bool
	Timeout  time.Duration
	Workers  int
	Debug    bool
	Writer   io.Writer
}

// start starts the project's stopped services.
func start(ctx context.Context, p *project.Project, c *client.Client, args startArgs) error {
	noColor := noColor(ctx)

	// We start all resources, just ignore that warning but let progress know them (so add before - LIFO - progress runs before).
	c.IgnoreError(client.ActionStart, client.ErrRunning)
	c.IgnoreError(client.ActionEnsure, client.ErrNotFound)

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

	resources, err := p.Resources(c)
	if err != nil {
		c.LogError("Getting project resources in reCreate", "error", err)
		return errLogged.Wrap(err)
	}

	order, err := p.ServiceOrder(false)
	if err != nil {
		c.LogError("Getting the service dependency order", "error", err)
		return errLogged.Wrap(err)
	}

	filterArgs := filterResourcesArgs{
		OnlyServices:     args.Services,
		WithDependencies: args.WithDeps,
		ExcludeKinds:     []client.Kind{client.KindImage, client.KindStorageVolume},
	}
	myResources := filterResources(p, resources, filterArgs)

	stack := client.NewStack(c, client.StackWorkers(args.Workers), client.StackFailFast())
	stack.AddOrdered(order, myResources)

	var errs error
	if err := stack.ForAction(client.ActionEnsure).Run(
		ctx,
		client.ActionEnsure,
	); err != nil {
		c.LogError("Getting resources", "error", err)
		errs = errors.Join(errs, err)
	}

	// Without --with-deps the linked services are not in scope, so don't
	// wait on healthd dependency conditions that can never be satisfied.
	startOpts := []client.Option{
		client.OptionTimeout(args.Timeout),
	}

	_, _, err = healthdResolve(p, c)
	if err != nil || (!args.WithDeps && len(args.Services) > 0) {
		startOpts = append(startOpts, client.OptionNoHealthd())
	}

	filter := func(r client.Resource) bool { return r.IsEnsured() }
	errStart := stack.ForActionF(client.ActionStart, filter).Run(ctx, client.ActionStart, startOpts...)
	if errStart != nil {
		c.LogError("Starting resources", "error", errStart)
		errs = errors.Join(errs, errStart)
	}

	if errs != nil {
		return errLogged.Wrap(errs)
	}

	return nil
}

//nolint:dupl // mirrors newStopCommand's shape intentionally; both are thin lifecycle-command wrappers around start()/stop().
func newStartCommand() *cli.Command {
	return &cli.Command{
		Name:      "start",
		Usage:     "Start stopped services",
		Category:  "compose",
		ArgsUsage: "[SERVICE...]",
		Flags: []cli.Flag{
			&cli.DurationFlag{
				Name:    "timeout",
				Usage:   "Timeout for starting",
				Value:   2 * time.Minute,
				Sources: cli.EnvVars("INCUS_COMPOSE_START_TIMEOUT"),
			},
			&cli.BoolFlag{
				Name:    "with-deps",
				Usage:   "Also start linked services",
				Sources: cli.EnvVars("INCUS_COMPOSE_START_WITH_DEPS"),
			},
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			p, c, err := loadProject(ctx, cmd)
			if err != nil {
				return err
			}

			err = c.Open()
			if err != nil {
				c.LogError("Opening the project client", "error", err)
				return errLogged.Wrap(err)
			}
			defer func() { _ = c.Done() }()

			return start(ctx, p, c, startArgs{
				Services: cmd.Args().Slice(),
				WithDeps: cmd.Bool("with-deps"),
				Timeout:  cmd.Duration("timeout"),
				Workers:  cmd.Root().Int("workers"),
				Debug:    cmd.Root().Bool("debug"),
				Writer:   cmd.Root().Writer,
			})
		},
	}
}
