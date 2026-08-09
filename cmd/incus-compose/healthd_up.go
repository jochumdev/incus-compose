package main

import (
	"context"
	"errors"
	"io"
	"maps"
	"net/url"
	"os"
	"time"

	"github.com/mattn/go-isatty"
	"github.com/urfave/cli/v3"

	"github.com/lxc/incus-compose/client"
	"github.com/lxc/incus-compose/project"
	"github.com/lxc/incus-compose/shared"
)

// healthdUpArgs holds the healthdUp() options, mirroring the `healthd up` command's flags.
type healthdUpArgs struct {
	Binary  string
	Image   string // raw --image flag value; resolved via resolveHealthdImage inside healthdUp.
	Incus   string // raw --incus/--healthd-incus override; empty keeps the project default.
	Network string // raw --network/--healthd-network override; empty keeps the project default.
	Scope   string // raw --scope/--healthd-scope override; loses to a scope the project carries.
	Pull    string
	Timeout time.Duration
	Workers int
	Debug   bool
	Trace   bool
	Writer  io.Writer
}

// healthdUp points the project at a healthd, shared or its own.
func healthdUp(ctx context.Context, p *project.Project, c *client.Client, args healthdUpArgs) error {
	if !healthdInUseByProject(c.Global(), p) {
		c.LogError("No service in this project declares a healthcheck")
		return errLogged.Wrap(errors.New("no service"))
	}

	noColor := noColor(ctx)

	projectConfig, err := c.Global().ProjectConfig(p.Name)
	if err != nil {
		c.LogError("Reading the project config", "error", err)
		return errLogged.Wrap(err)
	}

	scope, err := resolveHealthdScope(projectConfig, args.Scope, p.ClientConfig.Healthd.Scope)
	if err != nil {
		c.LogError("Resolving the healthd scope", "error", err)
		return errLogged.Wrap(err)
	}

	healthdIncus := p.ClientConfig.Healthd.Incus
	healthdNetwork := p.ClientConfig.Healthd.Network
	if args.Incus != "" {
		healthdIncus = args.Incus
	}
	if args.Network != "" {
		healthdNetwork = args.Network
	}

	var incus *url.URL
	if healthdIncus != "" {
		incus, err = url.Parse(healthdIncus)
		if err != nil {
			c.LogError("Parsing the healthd incus URL failed", "error", err)
			return errLogged.Wrap(errors.New("parsing error"))
		}
	}

	params := healthdParams{
		global:         scope == shared.HealthScopeGlobal,
		trace:          args.Trace,
		binary:         args.Binary,
		image:          resolveHealthdImage(args.Image),
		pull:           args.Pull,
		incus:          incus,
		network:        healthdNetwork,
		timeout:        args.Timeout,
		stackWorkers:   args.Workers,
		workers:        p.ClientConfig.Healthd.Workers,
		restartWorkers: p.ClientConfig.Healthd.RestartWorkers,
		xIncus:         p.ClientConfig.Healthd.XIncus,
	}

	c.LogDebug("Healthd",
		"scope", scope, "image", params.image, "binary", params.binary,
		"incus", healthdIncus, "network", params.network,
		"workers", params.workers, "restart_workers", params.restartWorkers)

	if !args.Debug {
		progress := newProgressRenderer(args.Writer, noColor, isatty.IsTerminal(os.Stdout.Fd()))
		progress.Start(c)
		defer progress.Stop(c)
	}

	// hc owns the sidecar, which for global scope is not this project.
	hc := c

	if params.global {
		// Before the marking below, or both daemons watch this project at once.
		exists, err := c.InstanceExists(healthdInstanceName(c.IncusProject(), false))
		if err != nil {
			c.LogError("Looking for a project healthd", "error", err)
			return errLogged.Wrap(err)
		}

		if exists {
			c.LogInfo("Replacing the project healthd with the shared one")

			if err := healthdTeardown(ctx, c, false, params.timeout); err != nil {
				c.LogError("Removing the project healthd", "error", err)
				return errLogged.Wrap(err)
			}
		}

		hc, err = c.Global().EnsureProject(
			systemProject,
			client.EnsureProjectWithCreate(),
			client.EnsureProjectWithConfig(map[string]string{managedKey: "true"}),
		)
		if err != nil {
			c.LogError("Getting the healthd project", "error", err)
			return errLogged.Wrap(err)
		}

		// Open before any stack action, as Client.Open documents.
		if err := hc.Open(); err != nil {
			c.LogError("Opening the healthd project client", "error", err)
			return errLogged.Wrap(err)
		}
		defer hc.WarnError(hc.Done, "Failure during Client.Done()")
	}

	// After the teardown, so nothing watches the project in between.
	err = c.Global().AddMissingProjectConfig(p.Name, map[string]string{shared.HealthScopeKey: scope})
	if err != nil {
		c.LogError("Marking the project's healthd scope", "error", err)
		return errLogged.Wrap(err)
	}

	// A daemon another project already started is the normal case, not an error.
	hc.IgnoreError(client.ActionStart, client.ErrRunning)

	stack := client.NewStack(hc, client.StackWorkers(params.stackWorkers))

	// Either sidecar may attach to a network of ours, so those come up first.
	pResources, err := p.Resources(c)
	if err != nil {
		c.LogError("Getting the service resources", "error", err)
		return errLogged.Wrap(err)
	}

	filterArgs := filterResourcesArgs{
		IncludeKinds: []client.Kind{client.KindNetwork},
	}
	myPResources := filterResources(p, pResources, filterArgs)

	order, err := p.ServiceOrder(true)
	if err != nil {
		c.LogError("Getting the service dependency order", "error", err)
		return errLogged.Wrap(err)
	}
	stack.AddOrdered(order, myPResources)

	return healthdEnsure(ctx, hc, stack, params)
}

// healthdUpGlobal brings the shared daemon up with no compose project to read.
// It marks nothing, so projects still opt in on their own `up`.
func healthdUpGlobal(ctx context.Context, gc *client.GlobalClient, args healthdUpArgs) error {
	var incus *url.URL
	if args.Incus != "" {
		var err error

		incus, err = url.Parse(args.Incus)
		if err != nil {
			gc.LogError("Parsing the healthd incus URL failed", "error", err)
			return errLogged.Wrap(errors.New("parsing error"))
		}
	}

	params := healthdParams{
		global:       true,
		trace:        args.Trace,
		binary:       args.Binary,
		image:        resolveHealthdImage(args.Image),
		pull:         args.Pull,
		incus:        incus,
		network:      args.Network,
		timeout:      args.Timeout,
		stackWorkers: args.Workers,
	}

	hc, err := gc.EnsureProject(
		systemProject,
		client.EnsureProjectWithCreate(),
		client.EnsureProjectWithConfig(map[string]string{managedKey: "true"}),
	)
	if err != nil {
		gc.LogError("Getting the healthd project", "error", err)
		return errLogged.Wrap(err)
	}

	if err := hc.Open(); err != nil {
		gc.LogError("Opening the healthd project client", "error", err)
		return errLogged.Wrap(err)
	}
	defer hc.WarnError(hc.Done, "Failure during Client.Done()")

	hc.LogDebug("Healthd", "scope", shared.HealthScopeGlobal, "image", params.image)

	if !args.Debug {
		progress := newProgressRenderer(args.Writer, noColor(ctx), isatty.IsTerminal(os.Stdout.Fd()))
		progress.Start(hc)
		defer progress.Stop(hc)
	}

	// After the renderer: hooks run last-registered first, so this has to nil
	// the error out before the renderer would draw it.
	hc.IgnoreError(client.ActionStart, client.ErrRunning)

	return healthdEnsure(ctx, hc, client.NewStack(hc, client.StackWorkers(params.stackWorkers)), params)
}

// healthdEnsure adds the sidecar to stack, brings it up, and replaces it when
// the image asked for is newer than the one it runs.
func healthdEnsure(ctx context.Context, hc *client.Client, stack *client.Stack, params healthdParams) error {
	// Shared with the hook that applies it, which reads it after the teardown.
	params.carry = map[string]string{}

	hInst, hResources, err := healthdGetResources(hc, params)
	if err != nil {
		hc.LogError("Creating healthd resources", "error", err)
		return errLogged.Wrap(err)
	}

	stack.Add(hResources...)
	stack.Add(hInst)

	hc.LogDebug("Ensure", "resources", stack.All())

	ensureOpts := []client.Option{client.OptionCreate(), client.OptionTimeout(params.timeout)}
	if params.pull == "always" {
		ensureOpts = append(ensureOpts, client.OptionPull())
	}

	var wantAlias string
	for _, r := range hResources {
		if r.Kind() == client.KindImage {
			wantAlias = r.IncusName()
			break
		}
	}

	// Only creating or replacing the sidecar needs the image. Read it rather
	// than ensure it, so an unreachable tag cannot fail a daemon that is
	// already running the one asked for.
	conn, err := hc.Connection()
	if err != nil {
		hc.LogError("Connecting to the healthd project", "error", err)
		return errLogged.Wrap(err)
	}

	current, _, err := conn.GetInstance(ctx, hInst.IncusName(), nil)
	fetchImage := err != nil ||
		params.pull == "always" ||
		healthdNeedsUpgrade(current.Config["user.image_alias"], wantAlias)

	// The image and the volume only exist to create the sidecar, and the
	// volume's Start validates itself against the image we did not fetch. One
	// already on the image asked for needs neither, only starting.
	needed := func(r client.Resource) bool {
		if fetchImage {
			return true
		}

		return r.Kind() != client.KindImage && r.Kind() != client.KindStorageVolume
	}

	if err := stack.ForActionF(client.ActionEnsure, needed).Run(ctx, client.ActionEnsure, ensureOpts...); err != nil {
		hc.LogError("Creating healthd resources", "error", err)
		return errLogged.Wrap(err)
	}

	// A newer image means the sidecar is replaced by one built from it.
	if info := hInst.State().IncusInstance; info != nil && healthdNeedsUpgrade(info.Config["user.image_alias"], wantAlias) {
		maps.Copy(params.carry, healthdCarriedConfig(info.Config))

		downStack := client.NewStack(hc, client.StackSortDescending(), client.StackWorkers(params.stackWorkers))

		for _, r := range hResources {
			if r.Kind() != client.KindNetwork && r.Kind() != client.KindImage {
				downStack.Add(r)
			}
		}
		downStack.Add(hInst)

		err := downStack.ForAction(client.ActionStop).Run(ctx, client.ActionStop, client.OptionTimeout(params.timeout))
		if err != nil && !errors.Is(err, client.ErrNotRunning) {
			hc.LogError("Stoping healthd resources for a new image", "error", err)
			return errLogged.Wrap(err)
		}

		if err := downStack.ForAction(client.ActionDelete).Run(ctx, client.ActionDelete, client.OptionTimeout(params.timeout)); err != nil {
			hc.LogError("Deleting healthd resources for a new image", "error", err)
			return errLogged.Wrap(err)
		}

		if err := stack.ForAction(client.ActionEnsure).Run(ctx, client.ActionEnsure, ensureOpts...); err != nil {
			hc.LogError("Creating healthd resources", "error", err)
			return errLogged.Wrap(err)
		}
	}

	if err := stack.ForActionF(client.ActionStart, needed).Run(ctx, client.ActionStart, client.OptionTimeout(params.timeout)); err != nil {
		hc.LogError("Starting healthd resources", "error", err)
		return errLogged.Wrap(err)
	}

	return nil
}

func newHealthdUpCommand() *cli.Command {
	return &cli.Command{
		Name:  "up",
		Usage: "Create or recreate the ic-healthd sidecar",
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:    "image",
				Usage:   `Healthd OCI image to use; {version} is replaced with the incus-compose version`,
				Value:   DefaultHealthdImage,
				Sources: cli.EnvVars("INCUS_COMPOSE_HEALTHD_IMAGE"),
			},
			&cli.StringFlag{
				Name:    "binary",
				Usage:   "Path to local ic-healthd binary (uses images:alpine/edge instead of OCI image)",
				Sources: cli.EnvVars("INCUS_COMPOSE_HEALTHD_BINARY"),
			},
			&cli.StringFlag{
				Name:    "incus",
				Usage:   `Connection URL of the incus to connect to from inside the sidecar. Empty = detect the ip from the bridge we are connected too`,
				Sources: cli.EnvVars("INCUS_COMPOSE_HEALTHD_INCUS"),
			},
			&cli.StringFlag{
				Name:    "network",
				Usage:   "Incus bridge for healthd to use (default: the network of the project it runs in)",
				Sources: cli.EnvVars("INCUS_COMPOSE_HEALTHD_NETWORK"),
			},
			&cli.StringFlag{
				Name:    "scope",
				Usage:   "Which healthd watches this project: `global` (shared, in its own project) or `project` (a sidecar of its own); loses to a scope the project already carries",
				Sources: cli.EnvVars("INCUS_COMPOSE_HEALTHD_SCOPE"),
			},
			&cli.StringFlag{
				Name:    "pull",
				Usage:   `Pull image before running ("always"|"missing"|"never"|"policy")`,
				Value:   "policy",
				Sources: cli.EnvVars("INCUS_COMPOSE_HEALTHD_PULL"),
			},
			&cli.DurationFlag{
				Name:    "timeout",
				Usage:   "Timeout for creating and starting",
				Value:   10 * time.Second,
				Sources: cli.EnvVars("INCUS_COMPOSE_HEALTHD_TIMEOUT"),
			},
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			globalClient, err := clientFromContext(ctx)
			if err != nil {
				return err
			}
			if err := globalClient.Connect(); err != nil {
				return err
			}

			p, err := healthdProject(ctx, cmd)
			if err != nil {
				globalClient.LogError("Configuring the project", "error", err)
				return errLogged.Wrap(err)
			}

			upArgs := healthdUpArgs{
				Binary:  cmd.String("binary"),
				Image:   cmd.String("image"),
				Incus:   cmd.String("incus"),
				Network: cmd.String("network"),
				Scope:   cmd.String("scope"),
				Pull:    cmd.String("pull"),
				Timeout: cmd.Duration("timeout"),
				Workers: cmd.Root().Int("workers"),
				Debug:   cmd.Root().Bool("debug"),
				Trace:   cmd.Root().Bool("trace"),
				Writer:  cmd.Root().Writer,
			}

			// No compose file to read, so there is no project to mark and
			// nothing to gate on: just put the shared daemon on the server.
			if p == nil {
				return healthdUpGlobal(ctx, globalClient, upArgs)
			}

			c, err := globalClient.EnsureProject(
				p.Name,
				client.EnsureProjectWithCreate(),
				client.EnsureProjectWithConfig(p.ClientConfig.XIncus),
			)
			if err != nil {
				globalClient.LogError("Getting the incus project", "error", err)
				return errLogged.Wrap(err)
			}
			defer c.WarnError(c.Done, "Failure during Client.Done()")

			return healthdUp(ctx, p, c, upArgs)
		},
	}
}
