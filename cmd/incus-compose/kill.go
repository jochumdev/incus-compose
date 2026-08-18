package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/urfave/cli/v3"
)

// killSignal is the only signal Incus can deliver through the state API.
const killSignal = "SIGKILL"

// newKillCommand implements `incus-compose kill`, which is stop() without the
// graceful shutdown.
func newKillCommand() *cli.Command {
	return &cli.Command{
		Name:      "kill",
		Usage:     "Force stop running services",
		Category:  "compose",
		ArgsUsage: "[SERVICE...]",
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:    "signal",
				Aliases: []string{"s"},
				Usage:   "Signal to send to the container. Only SIGKILL is supported.",
				Value:   killSignal,
				Sources: cli.EnvVars("INCUS_COMPOSE_KILL_SIGNAL"),
			},
			&cli.BoolFlag{
				Name:    "with-deps",
				Usage:   "Also kill linked services",
				Sources: cli.EnvVars("INCUS_COMPOSE_KILL_WITH_DEPS"),
			},
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			signal := strings.ToUpper(cmd.String("signal"))
			if signal != killSignal && signal != strings.TrimPrefix(killSignal, "SIG") {
				return fmt.Errorf("unsupported signal %q: the Incus state API delivers no signal but %s", cmd.String("signal"), killSignal)
			}

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
				Force:    true,
				Workers:  cmd.Root().Int("workers"),
				Debug:    cmd.Root().Bool("debug"),
				Writer:   cmd.Root().Writer,
			})
		},
	}
}
