package main

import (
	"context"
	"io"
	"os"

	"github.com/mattn/go-colorable"
	"github.com/urfave/cli/v3"

	"github.com/lxc/incus-compose/client"
)

func newHealthdLogsCommand() *cli.Command {
	return &cli.Command{
		Name:  "logs",
		Usage: "View output from the healthd sidecar",
		Flags: []cli.Flag{
			&cli.BoolFlag{
				Name:    "follow",
				Aliases: []string{"f"},
				Usage:   "Follow log output",
				Sources: cli.EnvVars("INCUS_COMPOSE_HEALTHD_LOGS_FOLLOW"),
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

			target, done, err := resolveHealthdTarget(ctx, cmd, globalClient)
			if err != nil {
				globalClient.LogError("Finding healthd", "error", err)
				return errLogged.Wrap(err)
			}
			defer done()

			c, h := target.client, target.instance

			var out io.Writer
			if f, ok := cmd.Root().Writer.(*os.File); ok {
				out = colorable.NewColorable(f)
			} else {
				out = cmd.Root().Writer
			}

			if err := h.Ensure(ctx); err != nil {
				c.LogError("Ensuring healthd", "error", err)
				return errLogged.Wrap(err)
			}

			return logs(ctx, c, logsArgs{
				Instances: []*client.Instance{h},
				Follow:    cmd.Bool("follow"),
				Writer:    out,
			})
		},
	}
}
