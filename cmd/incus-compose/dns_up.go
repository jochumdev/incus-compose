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

// dnsUpArgs holds the dnsUp() options, mirroring the `dns up` command's flags.
type dnsUpArgs struct {
	Image         string
	Incus         string
	Network       string
	IPv4Address   string
	IPv6Address   string
	Scope         string
	NoMetrics     bool
	Listen        string
	HTTP          string
	Forward       []string
	Suffix        string
	TTL           uint
	ProjectMarker string
	Pull          string
	Timeout       time.Duration
	Workers       int
	Debug         bool
	Writer        io.Writer
}

// resolveDNSScope returns the scope for the project, first match wins.
func resolveDNSScope(projectConfig map[string]string, cliScope, composeScope string) string {
	sources := []struct {
		where string
		value string
	}{
		{"the Incus project's " + shared.DNSScopeKey, projectConfig[shared.DNSScopeKey]},
		{"--dns-scope", cliScope},
		{"x-incus-compose.dns.scope", composeScope},
	}

	for _, source := range sources {
		if source.value == "" {
			continue
		}

		return source.value
	}

	return shared.DNSScopeGlobal
}

// dnsUp points the project at a dns, shared or its own.
func dnsUp(ctx context.Context, p *project.Project, c *client.Client, args dnsUpArgs) error {
	if p.ClientConfig.DNS.Disabled {
		c.LogError("DNS is disabled for this project")
		return errLogged.Wrap(errors.New("dns disabled"))
	}

	noColor := noColor(ctx)

	projectConfig, err := c.Global().ProjectConfig(p.Name)
	if err != nil {
		c.LogError("Reading the project config", "error", err)
		return errLogged.Wrap(err)
	}

	scope := resolveDNSScope(projectConfig, args.Scope, p.ClientConfig.DNS.Scope)

	dnsNetwork := p.ClientConfig.DNS.Network
	if args.Network != "" {
		dnsNetwork = args.Network
	}

	dnsIPv4 := p.ClientConfig.DNS.IPv4Address
	if args.IPv4Address != "" {
		dnsIPv4 = args.IPv4Address
	}

	dnsIPv6 := p.ClientConfig.DNS.IPv6Address
	if args.IPv6Address != "" {
		dnsIPv6 = args.IPv6Address
	}

	noMetrics := p.ClientConfig.DNS.NoMetrics || args.NoMetrics

	var incus *url.URL
	if args.Incus != "" {
		incus, err = url.Parse(args.Incus)
		if err != nil {
			c.LogError("Parsing the dns incus URL failed", "error", err)
			return errLogged.Wrap(errors.New("parsing error"))
		}
	}

	params := dnsParams{
		global:        scope == shared.DNSScopeGlobal,
		image:         resolveImageVersion(args.Image),
		pull:          args.Pull,
		incus:         incus,
		network:       dnsNetwork,
		ipv4Address:   dnsIPv4,
		ipv6Address:   dnsIPv6,
		scope:         scope,
		noMetrics:     noMetrics,
		listen:        args.Listen,
		http:          args.HTTP,
		forward:       args.Forward,
		suffix:        args.Suffix,
		ttl:           args.TTL,
		projectMarker: args.ProjectMarker,
		timeout:       args.Timeout,
		stackWorkers:  args.Workers,
	}

	c.LogDebug("DNS",
		"scope", scope, "image", params.image,
		"network", params.network, "ipv4", params.ipv4Address, "ipv6", params.ipv6Address)

	if !args.Debug {
		progress := newProgressRenderer(args.Writer, noColor, isatty.IsTerminal(os.Stdout.Fd()))
		progress.Start(c)
		defer progress.Stop(c)
	}

	hc := c

	if params.global {
		exists, err := c.InstanceExists(dnsInstanceName(c.IncusProject(), false))
		if err != nil {
			c.LogError("Looking for a project dns", "error", err)
			return errLogged.Wrap(err)
		}

		if exists {
			c.LogInfo("Replacing the project dns with the shared one")

			err = dnsTeardown(ctx, c, false, params.timeout, false)
			if err != nil {
				c.LogError("Removing the project dns", "error", err)
				return errLogged.Wrap(err)
			}
		}

		hc, err = c.Global().EnsureProject(
			systemProject,
			client.EnsureProjectWithCreate(),
			client.EnsureProjectWithConfig(map[string]string{managedKey: "true"}),
		)
		if err != nil {
			c.LogError("Getting the dns project", "error", err)
			return errLogged.Wrap(err)
		}

		err = hc.Open()
		if err != nil {
			c.LogError("Opening the dns project client", "error", err)
			return errLogged.Wrap(err)
		}
		defer hc.WarnError(hc.Done, "Failure during Client.Done()")
	}

	err = c.Global().AddMissingProjectConfig(p.Name, map[string]string{shared.DNSScopeKey: scope})
	if err != nil {
		c.LogError("Marking the project's dns scope", "error", err)
		return errLogged.Wrap(err)
	}

	hc.IgnoreError(client.ActionStart, client.ErrRunning)

	stack := client.NewStack(hc, client.StackWorkers(params.stackWorkers), client.StackFailFast())

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

	return dnsEnsure(ctx, hc, stack, params)
}

// dnsUpGlobal brings the shared daemon up with no compose project to read.
// It marks nothing, so projects still opt in on their own `up`.
func dnsUpGlobal(ctx context.Context, gc *client.GlobalClient, args dnsUpArgs) error {
	var incus *url.URL
	if args.Incus != "" {
		var err error

		incus, err = url.Parse(args.Incus)
		if err != nil {
			gc.LogError("Parsing the dns incus URL failed", "error", err)
			return errLogged.Wrap(errors.New("parsing error"))
		}
	}

	params := dnsParams{
		global:        true,
		image:         resolveImageVersion(args.Image),
		pull:          args.Pull,
		incus:         incus,
		network:       args.Network,
		ipv4Address:   args.IPv4Address,
		ipv6Address:   args.IPv6Address,
		scope:         shared.DNSScopeGlobal,
		noMetrics:     args.NoMetrics,
		listen:        args.Listen,
		http:          args.HTTP,
		forward:       args.Forward,
		suffix:        args.Suffix,
		ttl:           args.TTL,
		projectMarker: args.ProjectMarker,
		timeout:       args.Timeout,
		stackWorkers:  args.Workers,
	}

	hc, err := gc.EnsureProject(
		systemProject,
		client.EnsureProjectWithCreate(),
		client.EnsureProjectWithConfig(map[string]string{managedKey: "true"}),
	)
	if err != nil {
		gc.LogError("Getting the dns project", "error", err)
		return errLogged.Wrap(err)
	}

	err = hc.Open()
	if err != nil {
		gc.LogError("Opening the dns project client", "error", err)
		return errLogged.Wrap(err)
	}
	defer hc.WarnError(hc.Done, "Failure during Client.Done()")

	hc.LogDebug("DNS", "scope", shared.DNSScopeGlobal, "image", params.image)

	if !args.Debug {
		progress := newProgressRenderer(args.Writer, noColor(ctx), isatty.IsTerminal(os.Stdout.Fd()))
		progress.Start(hc)
		defer progress.Stop(hc)
	}

	hc.IgnoreError(client.ActionStart, client.ErrRunning)

	return dnsEnsure(ctx, hc, client.NewStack(hc, client.StackWorkers(params.stackWorkers), client.StackFailFast()), params)
}

// dnsEnsure adds the sidecar to stack, brings it up, and replaces it when
// the image asked for is newer than the one it runs.
func dnsEnsure(ctx context.Context, hc *client.Client, stack *client.Stack, params dnsParams) error {
	params.carry = map[string]string{}

	dInst, dResources, err := dnsGetResources(hc, params)
	if err != nil {
		hc.LogError("Creating dns resources", "error", err)
		return errLogged.Wrap(err)
	}

	stack.Add(dResources...)
	stack.Add(dInst)

	hc.LogDebug("Ensure", "resources", stack.All())

	ensureOpts := []client.Option{client.OptionCreate(), client.OptionTimeout(params.timeout)}

	var wantAlias string
	for _, r := range dResources {
		if r.Kind() == client.KindImage {
			wantAlias = r.IncusName()
			break
		}
	}

	hc.AddHookAfter(func(_ context.Context, action client.Action, r client.Resource, options client.Options, err error) error {
		if options.Create || action != client.ActionEnsure || r.IncusName() != dInst.IncusName() {
			return err
		}

		if errors.Is(err, client.ErrNotFound) {
			return nil
		}

		return err
	})

	lookup := client.NewStack(hc, client.StackWorkers(params.stackWorkers))
	lookup.Add(dInst)

	err = lookup.ForAction(client.ActionEnsure).Run(ctx, client.ActionEnsure, client.OptionTimeout(params.timeout))
	if err != nil {
		hc.LogError("Reading the running dns instance", "error", err)
		return errLogged.Wrap(err)
	}

	var running string
	if info := dInst.State().IncusInstance; info != nil {
		running = info.Config["user.image_alias"]
	}

	fetchImage := !dInst.IsEnsured() ||
		params.pull == "always" ||
		dnsNeedsUpgrade(running, wantAlias)

	needed := func(r client.Resource) bool {
		if fetchImage {
			return true
		}

		return r.Kind() != client.KindImage && r.Kind() != client.KindStorageVolume
	}

	if fetchImage && params.pull == "always" {
		err = refreshImages(ctx, hc, dResources, params.stackWorkers)
		if err != nil {
			return err
		}
	}

	err = stack.ForActionF(client.ActionEnsure, needed).Run(ctx, client.ActionEnsure, ensureOpts...)
	if err != nil {
		hc.LogError("Creating dns resources", "error", err)
		return errLogged.Wrap(err)
	}

	if info := dInst.State().IncusInstance; info != nil && dnsNeedsUpgrade(info.Config["user.image_alias"], wantAlias) {
		maps.Copy(params.carry, dnsCarriedConfig(info.Config))

		downStack := client.NewStack(hc, client.StackSortDescending(), client.StackWorkers(params.stackWorkers))

		for _, r := range dResources {
			if r.Kind() != client.KindNetwork && r.Kind() != client.KindImage && r.Kind() != client.KindStorageVolume {
				downStack.Add(r)
			}
		}
		downStack.Add(dInst)

		err := downStack.ForAction(client.ActionStop).Run(ctx, client.ActionStop, client.OptionTimeout(params.timeout))
		if err != nil && !errors.Is(err, client.ErrNotRunning) {
			hc.LogError("Stopping dns resources for a new image", "error", err)
			return errLogged.Wrap(err)
		}

		err = downStack.ForAction(client.ActionDelete).Run(ctx, client.ActionDelete, client.OptionTimeout(params.timeout))
		if err != nil {
			hc.LogError("Deleting dns resources for a new image", "error", err)
			return errLogged.Wrap(err)
		}

		err = stack.ForAction(client.ActionEnsure).Run(ctx, client.ActionEnsure, ensureOpts...)
		if err != nil {
			hc.LogError("Creating dns resources", "error", err)
			return errLogged.Wrap(err)
		}
	}

	err = stack.ForActionF(client.ActionStart, needed).Run(ctx, client.ActionStart, client.OptionTimeout(params.timeout), client.OptionNoHealthd())
	if err != nil {
		hc.LogError("Starting dns resources", "error", err)
		return errLogged.Wrap(err)
	}

	return nil
}

func newDNSUpCommand() *cli.Command {
	return &cli.Command{
		Name:  "up",
		Usage: "Create or recreate the ic-dns sidecar",
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:    "image",
				Usage:   `DNS OCI image to use; {version} is replaced with the incus-compose version`,
				Value:   DefaultDNSImage,
				Sources: cli.EnvVars("INCUS_COMPOSE_DNS_IMAGE"),
			},
			&cli.StringFlag{
				Name:    "incus",
				Usage:   `Connection URL of the incus to connect to from inside the sidecar. Empty = detect the ip from the bridge we are connected to`,
				Sources: cli.EnvVars("INCUS_COMPOSE_DNS_INCUS"),
			},
			&cli.StringFlag{
				Name:    "network",
				Usage:   "Incus bridge for dns to use (default: the network of the project it runs in)",
				Sources: cli.EnvVars("INCUS_COMPOSE_DNS_NETWORK"),
			},
			&cli.StringFlag{
				Name:    "ipv4",
				Usage:   "Static IPv4 address on the daemon's nic",
				Sources: cli.EnvVars("INCUS_COMPOSE_DNS_IPV4"),
			},
			&cli.StringFlag{
				Name:    "ipv6",
				Usage:   "Static IPv6 address on the daemon's nic",
				Sources: cli.EnvVars("INCUS_COMPOSE_DNS_IPV6"),
			},
			&cli.StringFlag{
				Name:    "scope",
				Usage:   "Which scope dns uses: `global` (shared, in its own project) or `project` (a sidecar of its own); loses to a scope the project already carries",
				Sources: cli.EnvVars("INCUS_COMPOSE_DNS_SCOPE"),
			},
			&cli.BoolFlag{
				Name:    "no-metrics",
				Usage:   "Disable metrics reporting",
				Sources: cli.EnvVars("INCUS_COMPOSE_DNS_NO_METRICS"),
			},
			&cli.StringFlag{
				Name:    "listen",
				Usage:   "Address to answer DNS on, UDP and TCP",
				Sources: cli.EnvVars("INCUS_COMPOSE_DNS_LISTEN"),
			},
			&cli.StringFlag{
				Name:    "http",
				Usage:   "Address to serve /metrics, /health and /ready on",
				Sources: cli.EnvVars("INCUS_COMPOSE_DNS_HTTP"),
			},
			&cli.StringSliceFlag{
				Name:    "forward",
				Usage:   "Upstream DNS server(s) for names we do not serve",
				Sources: cli.EnvVars("INCUS_COMPOSE_DNS_FORWARD"),
			},
			&cli.StringFlag{
				Name:    "suffix",
				Usage:   "TLD every project's zone is built under",
				Sources: cli.EnvVars("INCUS_COMPOSE_DNS_SUFFIX"),
			},
			&cli.UintFlag{
				Name:    "ttl",
				Usage:   "Seconds a record is served for",
				Sources: cli.EnvVars("INCUS_COMPOSE_DNS_TTL"),
			},
			&cli.StringFlag{
				Name:    "project-marker",
				Usage:   "Project config `KEY=VALUE` that opts a project in",
				Sources: cli.EnvVars("INCUS_COMPOSE_DNS_PROJECT_MARKER"),
			},
			&cli.StringFlag{
				Name:    "pull",
				Usage:   `Pull image before running ("always"|"missing"|"never"|"policy")`,
				Value:   "policy",
				Sources: cli.EnvVars("INCUS_COMPOSE_DNS_PULL"),
			},
			&cli.DurationFlag{
				Name:    "timeout",
				Usage:   "Timeout for creating and starting",
				Value:   10 * time.Second,
				Sources: cli.EnvVars("INCUS_COMPOSE_DNS_TIMEOUT"),
			},
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			globalClient, err := clientFromContext(ctx)
			if err != nil {
				return err
			}
			err = globalClient.Connect()
			if err != nil {
				return err
			}

			p, err := dnsProject(ctx, cmd)
			if err != nil {
				globalClient.LogError("Configuring the project", "error", err)
				return errLogged.Wrap(err)
			}

			upArgs := dnsUpArgs{
				Image:         cmd.String("image"),
				Incus:         cmd.String("incus"),
				Network:       cmd.String("network"),
				IPv4Address:   cmd.String("ipv4"),
				IPv6Address:   cmd.String("ipv6"),
				Scope:         cmd.String("scope"),
				NoMetrics:     cmd.Bool("no-metrics"),
				Listen:        cmd.String("listen"),
				HTTP:          cmd.String("http"),
				Forward:       cmd.StringSlice("forward"),
				Suffix:        cmd.String("suffix"),
				TTL:           cmd.Uint("ttl"),
				ProjectMarker: cmd.String("project-marker"),
				Pull:          cmd.String("pull"),
				Timeout:       cmd.Duration("timeout"),
				Workers:       cmd.Root().Int("workers"),
				Debug:         cmd.Root().Bool("debug"),
				Writer:        cmd.Root().Writer,
			}

			if p == nil {
				return dnsUpGlobal(ctx, globalClient, upArgs)
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

			return dnsUp(ctx, p, c, upArgs)
		},
	}
}
