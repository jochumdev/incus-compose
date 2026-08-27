package main

import (
	"context"
	"fmt"
	"slices"

	"github.com/urfave/cli/v3"

	"github.com/lxc/incus-compose/client"
	"github.com/lxc/incus-compose/project"
)

// eventFormats are what incus monitor renders.
var eventFormats = []string{"json", "pretty", "yaml"}

// newEventsCommand implements `incus-compose events` on top of incus monitor.
func newEventsCommand() *cli.Command {
	return &cli.Command{
		Name:     "events",
		Usage:    "Receive real time events from the project",
		Category: "compose",
		Flags: []cli.Flag{
			&cli.StringSliceFlag{
				Name:    "type",
				Aliases: []string{"t"},
				Usage:   "Event type to listen for. May be specified multiple times.",
				Value:   []string{"lifecycle"},
				Sources: cli.EnvVars("INCUS_COMPOSE_EVENTS_TYPE"),
			},
			&cli.StringFlag{
				Name:    "format",
				Usage:   "Output format: pretty, yaml or json",
				Value:   "pretty",
				Sources: cli.EnvVars("INCUS_COMPOSE_EVENTS_FORMAT"),
			},
			&cli.BoolFlag{
				Name:    "json",
				Usage:   "Output events as a stream of json objects, short for --format=json",
				Sources: cli.EnvVars("INCUS_COMPOSE_EVENTS_JSON"),
			},
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			globalClient := client.New(ctx)

			if cmd.Args().Len() > 0 {
				globalClient.LogError("events reports the whole project; incus monitor cannot filter by service",
					"arguments", cmd.Args().Slice())
				return errLogged.Wrap(fmt.Errorf("events takes no service arguments"))
			}

			format := cmd.String("format")
			if cmd.Bool("json") {
				format = "json"
			}

			if !slices.Contains(eventFormats, format) {
				return fmt.Errorf("unknown format %q, want one of %v", format, eventFormats)
			}

			p, err := project.New().Load(ctx, buildLoadOptions(cmd)...)
			if err != nil {
				globalClient.LogError("Loading the project", "error", err)
				return errLogged.Wrap(err)
			}

			iArgs := []string{"monitor", "--format", format}
			for _, t := range cmd.StringSlice("type") {
				iArgs = append(iArgs, "--type", t)
			}

			return runIncus(ctx, cmd.Root().Writer, cmd.Root().ErrWriter, []string{"INCUS_PROJECT=" + client.SanitizeProjectName(p.Name)}, iArgs...)
		},
	}
}
