package main

import (
	"context"
	"errors"
	"time"

	"github.com/urfave/cli/v3"
)

func newRestartCommand() *cli.Command {
	return &cli.Command{
		Name:      "restart",
		Usage:     "Restart running services",
		Category:  "compose",
		ArgsUsage: "[SERVICE...]",
		Flags: []cli.Flag{
			&cli.DurationFlag{
				Name:    "timeout",
				Usage:   "Timeout for stopping and starting",
				Value:   2 * time.Minute,
				Sources: cli.EnvVars("INCUS_COMPOSE_RESTART_TIMEOUT"),
			},
			&cli.BoolFlag{
				Name:    "with-deps",
				Usage:   "Also restart linked services",
				Sources: cli.EnvVars("INCUS_COMPOSE_RESTART_WITH_DEPS"),
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

			services := cmd.Args().Slice()
			withDeps := cmd.Bool("with-deps")
			timeout := cmd.Duration("timeout")
			workers := cmd.Root().Int("workers")
			debug := cmd.Root().Bool("debug")
			writer := cmd.Root().Writer

			// Each phase registers its own DNSWatcher.
			errStop := stop(ctx, p, c.Clone(), stopArgs{
				Services: services,
				WithDeps: withDeps,
				Timeout:  timeout,
				Workers:  workers,
				Debug:    debug,
				Writer:   writer,
			})

			errStart := start(ctx, p, c.Clone(), startArgs{
				Services: services,
				WithDeps: withDeps,
				Timeout:  timeout,
				Workers:  workers,
				Debug:    debug,
				Writer:   writer,
			})

			return errors.Join(errStop, errStart)
		},
	}
}
