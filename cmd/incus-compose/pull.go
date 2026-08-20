package main

import (
	"context"
	"io"
	"os"
	"time"

	"github.com/mattn/go-isatty"
	"github.com/urfave/cli/v3"

	"github.com/lxc/incus-compose/client"
	"github.com/lxc/incus-compose/project"
)

// pullArgs holds the pull() options, mirroring the pull command's flags.
type pullArgs struct {
	Services           []string
	WithDeps           bool
	IgnoreBuildable    bool
	IgnorePullFailures bool
	NoHealthd          bool
	HealthdImage       string
	Pull               client.PullMode
	Scale              map[string]int
	Workers            int
	Debug              bool
	Writer             io.Writer
}

// pull fetches the images of the project's services.
func pull(ctx context.Context, p *project.Project, c *client.Client, args pullArgs) error {
	noColor := noColor(ctx)

	if !args.Debug {
		progress := newProgressRenderer(args.Writer, noColor, isatty.IsTerminal(os.Stdout.Fd()))
		progress.Start(c)
		defer progress.Stop(c)
	}

	resources, err := p.Resources(c, project.ResourcesScale(args.Scale))
	if err != nil {
		c.LogError("Getting project resources in reCreate", "error", err)
		return errLogged.Wrap(err)
	}

	filterArgs := filterResourcesArgs{
		OnlyServices:     args.Services,
		WithDependencies: args.WithDeps,
		IncludeKinds:     []client.Kind{client.KindImage},
	}
	myResources := filterResources(p, resources, filterArgs)

	var stack *client.Stack
	if args.IgnorePullFailures {
		stack = client.NewStack(c, client.StackWorkers(args.Workers))
	} else {
		stack = client.NewStack(c, client.StackWorkers(args.Workers), client.StackFailFast())
	}

	for _, res := range myResources {
		for _, r := range res {
			if !args.IgnoreBuildable {
				stack.Add(r)
				continue
			}

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

	if !args.NoHealthd && healthdInUseByProject(c.Global(), p) {
		hparams := healthdParams{
			binary:       "",
			image:        resolveHealthdImage(args.HealthdImage),
			incus:        nil,
			network:      "",
			timeout:      time.Second,
			stackWorkers: args.Workers,
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

	err = stack.ForAction(client.ActionEnsure).Run(
		ctx,
		client.ActionEnsure,
		client.OptionPullMode(args.Pull),
		client.OptionCreate(),
	)
	if err != nil {
		c.LogError("Getting resources", "error", err)
		return errLogged.Wrap(err)
	}

	return nil
}

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

			// RegisterDNSWatcher is not idempotent, so pull() leaves it to its callers.
			if err := c.RegisterDNSWatcher(); err != nil {
				c.LogError("Registering the DNS watcher", "project", p.Name, "error", err)
				return errLogged.Wrap(err)
			}

			// Anything unknown keeps the flag's "always" default.
			pullMode := client.PullAlways
			switch cmd.String("policy") {
			case "missing":
				pullMode = client.PullMissing
			case "never":
				pullMode = client.PullNever
			}

			return pull(ctx, p, c, pullArgs{
				Services:           cmd.Args().Slice(),
				WithDeps:           cmd.Bool("include-deps"),
				IgnoreBuildable:    cmd.Bool("ignore-buildable"),
				IgnorePullFailures: cmd.Bool("ignore-pull-failures"),
				NoHealthd:          cmd.Bool("no-healthd"),
				HealthdImage:       cmd.String("healthd-image"),
				Pull:               pullMode,
				Workers:            cmd.Root().Int("workers"),
				Debug:              cmd.Root().Bool("debug"),
				Writer:             cmd.Root().Writer,
			})
		},
	}
}
