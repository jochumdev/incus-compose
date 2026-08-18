package main

import (
	"context"
	"errors"
	"io"
	"os"
	"strings"

	"github.com/mattn/go-isatty"
	"github.com/urfave/cli/v3"

	"github.com/lxc/incus-compose/client"
	"github.com/lxc/incus-compose/project"
)

// pauseArgs holds the pause() options, mirroring the pause command's flags.
type pauseArgs struct {
	Services []string
	WithDeps bool
	Workers  int
	Debug    bool
	Writer   io.Writer
}

// pause freezes the project's running services, or thaws them again when
// action is client.ActionUnpause.
func pause(ctx context.Context, p *project.Project, c *client.Client, action client.Action, args pauseArgs) error {
	noColor := noColor(ctx)

	// The whole project is in the stack, so the services that are in the wrong
	// state for this action say so and are otherwise left alone.
	c.IgnoreError(client.ActionEnsure, client.ErrNotFound)
	c.IgnoreError(action, client.ErrPaused)
	c.IgnoreError(action, client.ErrNotPaused)
	c.IgnoreError(action, client.ErrNotRunning)
	c.IgnoreError(action, client.ErrNotEnsured)

	if !args.Debug {
		progress := newProgressRenderer(args.Writer, noColor, isatty.IsTerminal(os.Stdout.Fd()))
		progress.Start(c)
		defer progress.Stop(c)
	}

	resources, err := p.Resources(c)
	if err != nil {
		c.LogError("Getting project resources", "error", err)
		return errLogged.Wrap(err)
	}

	// A pause takes the dependents down first, as a stop does; resuming goes
	// back the other way.
	reverse := action == client.ActionPause

	order, err := p.ServiceOrder(reverse)
	if err != nil {
		c.LogError("Getting the service dependency order", "error", err)
		return errLogged.Wrap(err)
	}

	filterArgs := filterResourcesArgs{
		OnlyServices:     args.Services,
		WithDependencies: args.WithDeps,
		Reverse:          reverse,
		IncludeKinds:     []client.Kind{client.KindInstance},
	}
	myResources := filterResources(p, resources, filterArgs)

	stackOpts := []client.StackOption{client.StackWorkers(args.Workers)}
	if reverse {
		stackOpts = append(stackOpts, client.StackSortDescending())
	}

	stack := client.NewStack(c, stackOpts...)
	stack.AddOrdered(order, myResources)

	var errs error

	err = stack.ForAction(client.ActionEnsure).Run(ctx, client.ActionEnsure)
	if err != nil {
		c.LogError("Getting resources", "error", err)
		errs = errors.Join(errs, err)
	}

	filter := func(r client.Resource) bool { return r.IsEnsured() }

	err = stack.ForActionF(action, filter).Run(ctx, action)
	if err != nil {
		c.LogError("Running the action", "action", action, "error", err)
		errs = errors.Join(errs, err)
	}

	if errs != nil {
		return errLogged.Wrap(errs)
	}

	return nil
}

// newPauseCommand implements `incus-compose pause` and `unpause`, which freeze
// and thaw a service's instances.
func newPauseCommand(action client.Action) *cli.Command {
	name, usage := "pause", "Pause running services"
	if action == client.ActionUnpause {
		name, usage = "unpause", "Resume paused services"
	}

	return &cli.Command{
		Name:      name,
		Usage:     usage,
		Category:  "compose",
		ArgsUsage: "[SERVICE...]",
		Flags: []cli.Flag{
			&cli.BoolFlag{
				Name:    "with-deps",
				Usage:   "Also " + name + " linked services",
				Sources: cli.EnvVars("INCUS_COMPOSE_" + strings.ToUpper(name) + "_WITH_DEPS"),
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

			return pause(ctx, p, c, action, pauseArgs{
				Services: cmd.Args().Slice(),
				WithDeps: cmd.Bool("with-deps"),
				Workers:  cmd.Root().Int("workers"),
				Debug:    cmd.Root().Bool("debug"),
				Writer:   cmd.Root().Writer,
			})
		},
	}
}
