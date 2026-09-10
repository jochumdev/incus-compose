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

	"github.com/lxc/incus-compose/shared"
)

// dnsDownConfirm asks before stopping a daemon other projects rely on.
func dnsDownConfirm(w io.Writer, others []string) (bool, error) {
	_, err := fmt.Fprintf(w, "The shared ic-dns also serves %d other project(s): %s\n",
		len(others), strings.Join(others, ", "))
	if err != nil {
		return false, err
	}

	if !isatty.IsTerminal(os.Stdin.Fd()) {
		return false, errors.New("refusing to stop the shared ic-dns without a terminal to confirm on, pass --force")
	}

	_, err = fmt.Fprint(w, "Stop it anyway, leaving them without DNS? [y/N] ")
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

func newDNSDownCommand() *cli.Command {
	return &cli.Command{
		Name:  "down",
		Usage: "Stop and remove the ic-dns sidecar",
		Flags: []cli.Flag{
			&cli.BoolFlag{
				Name:    "force",
				Usage:   "Stop the shared ic-dns without asking, even when other projects rely on it",
				Sources: cli.EnvVars("INCUS_COMPOSE_DNS_DOWN_FORCE"),
			},
			&cli.DurationFlag{
				Name:    "timeout",
				Usage:   "Timeout for stopping",
				Value:   10 * time.Second,
				Sources: cli.EnvVars("INCUS_COMPOSE_DNS_TIMEOUT"),
			},
			&cli.BoolFlag{
				Name:    "volumes",
				Aliases: []string{"v"},
				Usage:   "Remove the persistent data volume",
				Sources: cli.EnvVars("INCUS_COMPOSE_DNS_DOWN_VOLUMES"),
			},
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			noColor := noColor(ctx)

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

			c := target.client
			global := target.client.IncusProject() == systemProject

			if global && !cmd.Bool("force") {
				others, err := globalClient.ProjectsWithConfig(shared.DNSScopeKey, shared.DNSScopeGlobal)
				if err != nil {
					c.LogError("Listing the projects the shared dns serves", "error", err)
					return errLogged.Wrap(err)
				}

				others = slices.DeleteFunc(others, func(name string) bool {
					return name == c.IncusProject()
				})

				if len(others) > 0 {
					ok, err := dnsDownConfirm(cmd.Root().Writer, others)
					if err != nil {
						c.LogError("Confirming", "error", err)
						return errLogged.Wrap(err)
					}

					if !ok {
						c.LogInfo("Leaving the shared ic-dns alone")
						return nil
					}
				}
			}

			if !cmd.Root().Bool("debug") {
				progress := newProgressRenderer(cmd.Root().Writer, noColor, isatty.IsTerminal(os.Stdout.Fd()))
				progress.Start(c)
				defer progress.Stop(c)
			}

			keepVolume := !cmd.Bool("volumes")
			err = dnsTeardown(ctx, c, global, cmd.Duration("timeout"), keepVolume)
			if err != nil {
				c.LogError("Removing dns", "error", err)
				return errLogged.Wrap(err)
			}

			return nil
		},
	}
}
