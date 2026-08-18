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

// stopArgs holds the stop() options, mirroring the stop command's flags.
type stopArgs struct {
	Services []string
	WithDeps bool
	// Force kills outright instead of shutting down within Timeout. This is
	// what separates `kill` from `stop`.
	Force   bool
	Timeout time.Duration
	Workers int
	Debug   bool
	Writer  io.Writer
}

// stop stops the project's running services.
func stop(ctx context.Context, p *project.Project, c *client.Client, args stopArgs) error {
	noColor := noColor(ctx)

	// We start all resources, just ignore that warning but let progress know them (so add before - LIFO - progress runs before).
	c.IgnoreError(client.ActionEnsure, client.ErrNotFound)
	c.IgnoreError(client.ActionStop, client.ErrNotRunning)
	c.IgnoreError(client.ActionStop, client.ErrNotEnsured)

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

	order, err := p.ServiceOrder(true)
	if err != nil {
		c.LogError("Getting the service dependency order", "error", err)
		return errLogged.Wrap(err)
	}

	filterArgs := filterResourcesArgs{
		OnlyServices:     args.Services,
		WithDependencies: args.WithDeps,
		Reverse:          true,
		ExcludeKinds:     []client.Kind{client.KindImage, client.KindStorageVolume},
	}
	myResources := filterResources(p, resources, filterArgs)

	stack := client.NewStack(c, client.StackSortDescending(), client.StackWorkers(args.Workers))
	stack.AddOrdered(order, myResources)

	var errs error
	if err := stack.ForAction(client.ActionEnsure).Run(
		ctx,
		client.ActionEnsure,
	); err != nil {
		c.LogError("Getting resources", "error", err)
		errs = errors.Join(errs, err)
	}

	// Without --with-deps the linked services are not in scope; skip the
	// healthd interaction that targets out-of-scope dependencies.
	// An explicit zero would override the client's own default with a context
	// that is already expired, so leave the option off instead.
	stopOpts := []client.Option{}
	if args.Timeout > 0 {
		stopOpts = append(stopOpts, client.OptionTimeout(args.Timeout))
	}

	if args.Force {
		stopOpts = append(stopOpts, client.OptionForce())
	}

	_, _, err = healthdResolve(p, c)
	if err != nil || (!args.WithDeps && len(args.Services) > 0) {
		stopOpts = append(stopOpts, client.OptionNoHealthd())
	}

	filter := func(r client.Resource) bool { return r.IsEnsured() }
	errStop := stack.ForActionF(client.ActionStop, filter).Run(ctx, client.ActionStop, stopOpts...)
	if errStop != nil {
		c.LogWarn("Stopping resources", "error", errStop)
		errs = errors.Join(errs, errStop)
	}

	if errs != nil {
		return errLogged.Wrap(errs)
	}

	return nil
}

//nolint:dupl // mirrors newStartCommand's shape intentionally; both are thin lifecycle-command wrappers around start()/stop().
func newStopCommand() *cli.Command {
	return &cli.Command{
		Name:      "stop",
		Usage:     "Stop running services",
		Category:  "compose",
		ArgsUsage: "[SERVICE...]",
		Flags: []cli.Flag{
			&cli.DurationFlag{
				Name:    "timeout",
				Usage:   "Timeout for stopping",
				Value:   10 * time.Second,
				Sources: cli.EnvVars("INCUS_COMPOSE_STOP_TIMEOUT"),
			},
			&cli.BoolFlag{
				Name:    "with-deps",
				Usage:   "Also stop linked services",
				Sources: cli.EnvVars("INCUS_COMPOSE_STOP_WITH_DEPS"),
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

			return stop(ctx, p, c, stopArgs{
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
