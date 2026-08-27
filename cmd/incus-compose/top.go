package main

import (
	"context"
	"fmt"
	"strconv"

	"github.com/urfave/cli/v3"

	"github.com/lxc/incus-compose/client"
	"github.com/lxc/incus-compose/project"
)

// newTopCommand implements `incus-compose top`, which reports per instance
// where `docker compose top` reports per process.
func newTopCommand() *cli.Command {
	return &cli.Command{
		Name:     "top",
		Usage:    "Display resource usage per instance",
		Category: "compose",
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:    "columns",
				Aliases: []string{"c"},
				Usage:   "Columns to display, as incus top spells them",
				Sources: cli.EnvVars("INCUS_COMPOSE_TOP_COLUMNS"),
			},
			&cli.StringFlag{
				Name:    "format",
				Usage:   "Output format: table or compact",
				Sources: cli.EnvVars("INCUS_COMPOSE_TOP_FORMAT"),
			},
			&cli.IntFlag{
				Name:    "refresh",
				Usage:   "Refresh delay in seconds, 10 at the lowest",
				Sources: cli.EnvVars("INCUS_COMPOSE_TOP_REFRESH"),
			},
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			globalClient := client.New(ctx)

			if cmd.Args().Len() > 0 {
				globalClient.LogError("top reports the whole project; incus top cannot filter by service",
					"arguments", cmd.Args().Slice())
				return errLogged.Wrap(fmt.Errorf("top takes no service arguments"))
			}

			p, err := project.New().Load(ctx, buildLoadOptions(cmd)...)
			if err != nil {
				globalClient.LogError("Loading the project", "error", err)
				return errLogged.Wrap(err)
			}

			iArgs := []string{"top"}
			if cmd.String("columns") != "" {
				iArgs = append(iArgs, "--columns", cmd.String("columns"))
			}

			if cmd.String("format") != "" {
				iArgs = append(iArgs, "--format", cmd.String("format"))
			}

			if cmd.Int("refresh") > 0 {
				iArgs = append(iArgs, "--refresh", strconv.Itoa(cmd.Int("refresh")))
			}

			return runIncus(ctx, cmd.Root().Writer, cmd.Root().ErrWriter, []string{"INCUS_PROJECT=" + client.SanitizeProjectName(p.Name)}, iArgs...)
		},
	}
}
