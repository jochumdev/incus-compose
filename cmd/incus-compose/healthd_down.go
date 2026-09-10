package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/mattn/go-isatty"
	"github.com/urfave/cli/v3"

	"github.com/lxc/incus-compose/client"
	"github.com/lxc/incus-compose/shared"
)

// healthdDownConfirm asks before stopping a daemon other projects rely on.
func healthdDownConfirm(w io.Writer, others []string) (bool, error) {
	_, err := fmt.Fprintf(w, "The shared ic-healthd also watches %d other project(s): %s\n",
		len(others), strings.Join(others, ", "))
	if err != nil {
		return false, err
	}

	if !isatty.IsTerminal(os.Stdin.Fd()) {
		return false, errors.New("refusing to stop the shared ic-healthd without a terminal to confirm on, pass --force")
	}

	_, err = fmt.Fprint(w, "Stop it anyway, leaving them unwatched? [y/N] ")
	if err != nil {
		return false, err
	}

	answer, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil {
		return false, err
	}

	answer = strings.ToLower(strings.TrimSpace(answer))

	return answer == "y" || answer == "yes", nil
}

func newHealthdDownCommand() *cli.Command {
	return &cli.Command{
		Name:  "down",
		Usage: "Stop and remove the ic-healthd sidecar",
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:    "image",
				Usage:   `Healthd OCI image to use; {version} is replaced with the incus-compose version`,
				Value:   DefaultHealthdImage,
				Sources: cli.EnvVars("INCUS_COMPOSE_HEALTHD_IMAGE"),
			},
			&cli.BoolFlag{
				Name:    "force",
				Usage:   "Stop the shared ic-healthd without asking, even when other projects rely on it",
				Sources: cli.EnvVars("INCUS_COMPOSE_HEALTHD_DOWN_FORCE"),
			},
			&cli.DurationFlag{
				Name:    "timeout",
				Usage:   "Timeout for stopping",
				Value:   10 * time.Second,
				Sources: cli.EnvVars("INCUS_COMPOSE_HEALTHD_TIMEOUT"),
			},
		},
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

			c := target.client
			global := target.scope == shared.HealthScopeGlobal

			if global && !cmd.Bool("force") {
				others, err := globalClient.ProjectsWithConfig(shared.HealthScopeKey, shared.HealthScopeGlobal)
				if err != nil {
					c.LogError("Listing the projects the shared healthd watches", "error", err)
					return errLogged.Wrap(err)
				}

				others = slices.DeleteFunc(others, func(name string) bool {
					return name == c.IncusProject()
				})

				if len(others) > 0 {
					ok, err := healthdDownConfirm(cmd.Root().Writer, others)
					if err != nil {
						c.LogError("Confirming", "error", err)
						return errLogged.Wrap(err)
					}

					if !ok {
						c.LogInfo("Leaving the shared ic-healthd alone")
						return nil
					}
				}
			}

			// We stop all resources, just ignore that warning but let progress know them (so add before - LIFO - progress runs before).
			c.IgnoreError(client.ActionStop, client.ErrNotEnsured)
			c.IgnoreError(client.ActionStop, client.ErrNotRunning)
			c.IgnoreError(client.ActionEnsure, client.ErrNotFound)
			c.IgnoreError(client.ActionDelete, client.ErrNotEnsured)
			c.IgnoreError(client.ActionDelete, client.ErrNotFound)

			// Replicas of a service share one volume, so all but the last delete says this.
			c.IgnoreError(client.ActionDelete, client.ErrVolumeInUse)

			if !cmd.Root().Bool("debug") {
				progress := newProgressRenderer(cmd.Root().Writer, noColor, isatty.IsTerminal(os.Stdout.Fd()))
				progress.Start(c)
				defer progress.Stop(c)
			}

			if err := healthdTeardown(ctx, c, global, cmd.Duration("timeout")); err != nil {
				c.LogError("Removing healthd", "error", err)
				return errLogged.Wrap(err)
			}

			return nil
		},
	}
}
