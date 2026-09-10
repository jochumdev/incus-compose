package main

import (
	"context"
	"io"
	"os"

	"github.com/mattn/go-colorable"
	"github.com/urfave/cli/v3"

	"github.com/lxc/incus-compose/client"
)

func newDNSLogsCommand() *cli.Command {
	return &cli.Command{
		Name:  "logs",
		Usage: "View output from the ic-dns sidecar",
		Flags: []cli.Flag{
			&cli.BoolFlag{
				Name:    "follow",
				Aliases: []string{"f"},
				Usage:   "Follow log output",
				Sources: cli.EnvVars("INCUS_COMPOSE_DNS_LOGS_FOLLOW"),
			},
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			globalClient, err := clientFromContext(ctx)
			if err != nil {
				return err
			}

			err = globalClient.Connect()
			if err != nil {
				return err
			}

			target, done, err := resolveDNSTarget(ctx, cmd, globalClient)
			if err != nil {
				globalClient.LogError("Finding dns", "error", err)
				return errLogged.Wrap(err)
			}
			defer done()

			c, d := target.client, target.instance

			var out io.Writer
			if f, ok := cmd.Root().Writer.(*os.File); ok {
				out = colorable.NewColorable(f)
			} else {
				out = cmd.Root().Writer
			}

			err = d.Ensure(ctx)
			if err != nil {
				c.LogError("Ensuring dns", "error", err)
				return errLogged.Wrap(err)
			}

			return logs(ctx, c, logsArgs{
				Instances: []*client.Instance{d},
				Follow:    cmd.Bool("follow"),
				Writer:    out,
			})
		},
	}
}
