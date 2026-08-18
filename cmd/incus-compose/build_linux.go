//go:build unix && !darwin

package main

import (
	"context"
	"errors"
	"slices"

	"github.com/urfave/cli/v3"

	"github.com/lxc/incus-compose/client"
)

func newBuildCommand() *cli.Command {
	return &cli.Command{
		Name:      "build",
		Usage:     "Build or rebuild service images",
		Category:  "compose",
		ArgsUsage: "[SERVICE...]",
		Flags: []cli.Flag{
			&cli.BoolFlag{
				Name:    "no-cache",
				Usage:   "Do not use a cache when building the image",
				Sources: cli.EnvVars("INCUS_COMPOSE_BUILD_NO_CACHE"),
			},
			&cli.StringFlag{
				Name:    "pull",
				Usage:   `Pull image before running ("always"|"missing"|"never"|"policy")`,
				Value:   "policy",
				Sources: cli.EnvVars("INCUS_COMPOSE_BUILD_PULL"),
			},
			&cli.StringFlag{
				Name:    "builder",
				Usage:   "Preferred builder, binary name or absolute path. Empty for auto-detect.",
				Sources: cli.EnvVars("INCUS_COMPOSE_BUILD_BUILDER"),
			},
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
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

			allResources, err := p.Resources(c)
			if err != nil {
				c.LogError("Getting project resources in reCreate", "error", err)
				return errLogged.Wrap(err)
			}

			noCache := cmd.Bool("no-cache")
			pull := cmd.String("pull")
			services := cmd.Args().Slice()

			stack := client.NewStack(c, client.StackSortDescending(), client.StackWorkers(cmd.Root().Int("workers")))

			for service, resources := range allResources {
				if len(services) > 0 && !slices.Contains(services, service) {
					continue
				}

				for _, r := range resources {
					if r.Kind() != client.KindImage {
						continue
					}

					img, ok := r.(*client.Image)
					if !ok {
						continue
					}

					if img.Config.Build == nil {
						continue
					}

					if noCache {
						img.Config.Build.NoCache = noCache
					}

					stack.Add(img)
				}
			}

			if len(stack.All()) < 1 {
				if cmd.Args().Len() > 0 {
					err = errors.New("no build-configured services matched the filter")
					c.LogError(err.Error())
					return errLogged.Wrap(err)
				}

				c.LogInfo("No services have a build configuration")
				return nil
			}

			buildInfo := client.BuildInfo{
				Mode:             client.BuildForce,
				PreferredBuilder: cmd.String("builder"),
			}

			ensureOpts := []client.Option{
				client.OptionCreate(),
				client.OptionBuild(buildInfo),
			}
			// "missing" and the legacy "policy" are the default, as is anything unknown.
			pullMode := client.PullMissing
			switch pull {
			case "always":
				pullMode = client.PullAlways
			case "never":
				pullMode = client.PullNever
			}

			ensureOpts = append(ensureOpts, client.OptionPullMode(pullMode))

			err = stack.ForAction(client.ActionEnsure).Run(ctx, client.ActionEnsure, ensureOpts...)
			if err != nil {
				c.LogError("Ensuring resources", "error", err)
				return errLogged.Wrap(err)
			}

			return nil
		},
	}
}
