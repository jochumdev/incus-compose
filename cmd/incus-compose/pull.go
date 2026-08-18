package main

import (
	"context"
	"os"
	"time"

	"github.com/mattn/go-isatty"
	"github.com/urfave/cli/v3"

	"github.com/lxc/incus-compose/client"
)

func newPullCommand() *cli.Command {
	return &cli.Command{
		Name:      "pull",
		Usage:     "Pull service images",
		Category:  "compose",
		ArgsUsage: "[SERVICE...]",
		Flags: []cli.Flag{
			&cli.BoolFlag{
				Name:    "ignore-buildable",
				Usage:   `Ignore images that can be built`,
				Sources: cli.EnvVars("INCUS_COMPOSE_PULL_IGNORE_BUILDABLE"),
			},
			&cli.BoolFlag{
				Name:    "ignore-pull-failures",
				Usage:   `Pull what it can and ignores images with pull failures`,
				Sources: cli.EnvVars("INCUS_COMPOSE_PULL_IGNORE_PULL_FAILURES"),
			},
			&cli.BoolFlag{
				Name:    "include-deps",
				Aliases: []string{"with-deps"},
				Usage:   "Also pull linked services",
				Sources: cli.EnvVars("INCUS_COMPOSE_PULL_INCLUDE_DEPS"),
			},
			&cli.StringFlag{
				Name:    "policy",
				Usage:   `Apply pull policy ("missing"|"always"|"never")`,
				Value:   "always",
				Sources: cli.EnvVars("INCUS_COMPOSE_PULL_POLICY"),
			},
			&cli.BoolFlag{
				Name:    "no-healthd",
				Usage:   "Don't pull the healthd sidecar",
				Sources: cli.EnvVars("INCUS_COMPOSE_NO_HEALTHD"),
			},
			&cli.StringFlag{
				Name:    "healthd-image",
				Usage:   `Healthd OCI image to use; {version} is replaced with the incus-compose version`,
				Value:   DefaultHealthdImage,
				Sources: cli.EnvVars("INCUS_COMPOSE_HEALTHD_IMAGE"),
			},
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			noColor := noColor(ctx)

			withDeps := cmd.Bool("include-deps")

			p, c, err := loadProject(ctx, cmd, client.EnsureProjectWithCreate())
			if err != nil {
				return err
			}

			err = c.Open()
			if err != nil {
				c.LogError("Opening the project client", "error", err)
				return errLogged.Wrap(err)
			}
			defer c.WarnError(c.Done, "Failure during Client.Done()")

			if !cmd.Root().Bool("debug") {
				progress := newProgressRenderer(cmd.Root().Writer, noColor, isatty.IsTerminal(os.Stdout.Fd()))
				progress.Start(c)
				defer progress.Stop(c)
			}

			// Register the DNS Watcher after the progress renderer so progress waits for the dns changes.
			if err := c.RegisterDNSWatcher(); err != nil {
				c.LogError("Registering the DNS watcher", "project", p.Name, "error", err)
				return errLogged.Wrap(err)
			}

			resources, err := p.Resources(c)
			if err != nil {
				c.LogError("Getting project resources in reCreate", "error", err)
				return errLogged.Wrap(err)
			}

			args := filterResourcesArgs{
				OnlyServices:     cmd.Args().Slice(),
				WithDependencies: withDeps,
			}
			myResources := filterResources(p, resources, args)

			var stack *client.Stack
			if cmd.Bool("ignore-pull-failures") {
				stack = client.NewStack(c, client.StackWorkers(cmd.Root().Int("workers")))
			} else {
				stack = client.NewStack(c, client.StackWorkers(cmd.Root().Int("workers")), client.StackFailFast())
			}

			for _, res := range myResources {
				for _, r := range res {
					if r.Kind() != client.KindImage {
						continue
					}

					if !cmd.Bool("ignore-buildable") {
						stack.Add(r)
					} else {
						i, ok := r.(*client.Image)
						if !ok {
							continue
						}

						// Ignore images with a build config.
						if i.Config.Build == nil {
							stack.Add(i)
						}
					}
				}
			}

			if !cmd.Bool("no-healthd") && healthdInUseByProject(c.Global(), p) {
				hparams := healthdParams{
					binary:       "",
					image:        resolveHealthdImage(cmd.String("healthd-image")),
					pull:         cmd.String("policy"),
					incus:        nil,
					network:      "",
					timeout:      time.Second,
					stackWorkers: cmd.Root().Int("workers"),
				}

				_, hResources, err := healthdGetResources(c, hparams)
				if err != nil {
					c.LogError("Creating healthd resources", "error", err)
					return errLogged.Wrap(err)
				}

				for _, r := range hResources {
					if r.Kind() == client.KindImage {
						stack.Add(r)
					}
				}
			}

			// Anything unknown keeps the flag's "always" default.
			pullMode := client.PullAlways
			switch cmd.String("policy") {
			case "missing":
				pullMode = client.PullMissing
			case "never":
				pullMode = client.PullNever
			}

			err = stack.ForAction(client.ActionEnsure).Run(
				ctx,
				client.ActionEnsure,
				client.OptionPullMode(pullMode),
				client.OptionCreate(),
			)
			if err != nil {
				c.LogError("Getting resources", "error", err)
				return errLogged.Wrap(err)
			}

			return nil
		},
	}
}
