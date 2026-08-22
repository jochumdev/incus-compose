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

	// Init is the tools image `run` needs. Prefetched here so an air-gapped
	// site that can pull can also run a one-off later.
	Init    string
	Pull    client.PullMode
	Scale   map[string]int
	Workers int
	Debug   bool
	Writer  io.Writer
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
			image:        resolveImageVersion(args.HealthdImage),
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

	downloadTools(ctx, c, args.Init)

	// Only "always" acts on the answer, and "never" may not touch the source at all.
	if args.Pull == client.PullAlways {
		err = refreshImages(ctx, c, stack.All(), args.Workers)
		if err != nil {
			return err
		}
	}

	// The dropped images miss the store now, so this is the plain create path.
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

// refreshImages deletes the images their source has moved off, so that the
// ensure after it creates them from what the source holds now. Anything that is
// not an image is ignored, so a caller can hand it a whole stack.
func refreshImages(ctx context.Context, c *client.Client, resources []client.Resource, workers int) error {
	stack := client.NewStack(c, client.StackWorkers(workers), client.StackFailFast())

	for _, r := range resources {
		if r.Kind() == client.KindImage {
			stack.Add(r)
		}
	}

	if len(stack.All()) == 0 {
		return nil
	}

	err := stack.ForAction(client.ActionEnsure).Run(
		ctx,
		client.ActionEnsure,
		client.OptionCreate(),
		client.OptionResolveSource(),
	)
	if err != nil {
		c.LogError("Reading the images from their source", "error", err)
		return errLogged.Wrap(err)
	}

	stale := []client.Resource{}

	for _, r := range stack.All() {
		img, ok := r.(*client.Image)
		if !ok {
			continue
		}

		state := img.State()

		// An empty side is a source nothing resolved, or an image this run just
		// created; neither says what is stored is out of date.
		if state.SourceFingerprint == "" || state.IncusAlias == nil {
			continue
		}

		if state.SourceFingerprint != state.IncusAlias.Target {
			stale = append(stale, img)
		}
	}

	if len(stale) == 0 {
		return nil
	}

	c.LogDebug("Refreshing images their source has moved off", "images", stale)

	// Both copies go: the cached one is the store hit the next Ensure would
	// find, and the project one is what an instance is created from.
	dropStack := client.NewStack(c, client.StackWorkers(workers), client.StackFailFast())
	dropStack.Add(stale...)

	err = dropStack.ForAction(client.ActionDelete).Run(ctx, client.ActionDelete, client.OptionCache())
	if err != nil {
		c.LogError("Deleting the images their source has moved off", "error", err)
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
			&cli.StringFlag{
				Name:    "init",
				Usage:   "Image the `run` helper comes from",
				Value:   DefaultInitImage,
				Sources: cli.EnvVars("INCUS_COMPOSE_INIT_IMAGE"),
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
				Init:               cmd.String("init"),
				Pull:               pullMode,
				Workers:            cmd.Root().Int("workers"),
				Debug:              cmd.Root().Bool("debug"),
				Writer:             cmd.Root().Writer,
			})
		},
	}
}
