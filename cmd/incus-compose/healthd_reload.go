package main

import (
	"context"
	"os"

	"github.com/mattn/go-isatty"
	"github.com/urfave/cli/v3"
)

func newHealthdReloadCommand() *cli.Command {
	return &cli.Command{
		Name:  "reload",
		Usage: "Send SIGHUP to the ic-healthd process",
		Action: func(ctx context.Context, cmd *cli.Command) error {
			noColor := noColor(ctx)

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

			hc, h := target.client, target.instance

			if !cmd.Root().Bool("debug") {
				progress := newProgressRenderer(cmd.Root().Writer, noColor, isatty.IsTerminal(os.Stdout.Fd()))
				progress.Start(hc)
				defer progress.Stop(hc)
			}

			if err := h.Ensure(ctx); err != nil {
				hc.LogError("Ensuring healthd", "error", err)
				return errLogged.Wrap(err)
			}

			err = h.Exec(ctx, "sh", "-c",
				`pids="$(pidof ic-healthd)" && for pid in $pids; do kill -HUP "$pid"; done`)
			if err != nil {
				hc.LogError("Reloading healthd", "error", err)
				return errLogged.Wrap(err)
			}

			return nil
		},
	}
}
