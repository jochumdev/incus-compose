package main

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/urfave/cli/v3"

	"github.com/lxc/incus-compose/client"
)

func newHealthdStatusCommand() *cli.Command {
	return &cli.Command{
		Name:  "status",
		Usage: "Prints the status of healthd",
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:    "format",
				Usage:   "Format the output. Values: [text | json]",
				Value:   "text",
				Sources: cli.EnvVars("INCUS_COMPOSE_HEALTHD_STATUS_FORMAT"),
				Action: func(ctx context.Context, cmd *cli.Command, v string) error {
					if !slices.Contains([]string{"text", "json"}, v) {
						return fmt.Errorf("invalid format: %s (must be text or json)", v)
					}
					return nil
				},
			},
			&cli.IntFlag{
				Name:    "port",
				Usage:   "HTTP port to query",
				Value:   9153,
				Sources: cli.EnvVars("INCUS_COMPOSE_HEALTHD_HTTP_PORT"),
			},
			&cli.BoolFlag{
				Name:    "metrics",
				Usage:   "Include Prometheus metrics in output",
				Sources: cli.EnvVars("INCUS_COMPOSE_HEALTHD_STATUS_METRICS"),
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

			target, done, err := resolveHealthdTarget(ctx, cmd, globalClient)
			if err != nil {
				globalClient.LogError("Finding healthd", "error", err)
				return errLogged.Wrap(err)
			}
			defer done()

			err = client.RunAction(ctx, target.instance, client.ActionEnsure)
			if err != nil {
				return fmt.Errorf("while fetching healthd: %w", err)
			}

			state := target.instance.State()
			if state == nil {
				return errors.New("no healthd state after fetch")
			}

			ipv4, ipv6 := sidecarIPAddresses(state)
			status := "stopped"
			var metricsBody string

			if state.IncusInstance.Status == "Running" {
				status, metricsBody = fetchSidecarStatus(ctx, target.instance.IncusName(), target.client.IncusProject(), cmd.Int("port"), cmd.Bool("metrics"))
			}

			return renderSidecarStatus(cmd.Root().Writer, cmd.String("format"), status, ipv4, ipv6, metricsBody)
		},
	}
}
