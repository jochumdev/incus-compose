package main

import (
	"context"

	"github.com/urfave/cli/v3"

	"github.com/lxc/incus-compose/client"
	"github.com/lxc/incus-compose/project"
)

// newIncusCommand proxies arbitrary incus CLI commands into the current compose project context
// by injecting INCUS_PROJECT=<sanitized-project-name> into the environment.
func newIncusCommand() *cli.Command {
	return &cli.Command{
		Name:            "incus",
		Usage:           "Run an incus command in the current project context",
		Category:        "extensions",
		ArgsUsage:       "COMMAND [ARGS...]",
		SkipFlagParsing: true,
		Action: func(ctx context.Context, cmd *cli.Command) error {
			globalClient := client.New(ctx)

			p, err := project.New().Load(ctx, buildLoadOptions(cmd)...)
			if err != nil {
				globalClient.LogError("Loading the project", "error", err)
				return errLogged.Wrap(err)
			}

			incusProject := client.SanitizeProjectName(p.Name)

			return runIncus(ctx, cmd.Root().Writer, cmd.Root().ErrWriter, []string{"INCUS_PROJECT=" + incusProject}, cmd.Args().Slice()...)
		},
	}
}
