package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/compose-spec/compose-go/v2/format"
	"github.com/compose-spec/compose-go/v2/types"
	shellquote "github.com/kballard/go-shellquote"
	incusApi "github.com/lxc/incus/v7/shared/api"
	"github.com/lxc/incus/v7/shared/util"
	"github.com/mattn/go-isatty"
	"github.com/urfave/cli/v3"

	"github.com/lxc/incus-compose/client"
	"github.com/lxc/incus-compose/iclient"
	"github.com/lxc/incus-compose/project"
)

// DefaultInitImage ships the blocking helper a one-off runs as its entrypoint.
const DefaultInitImage = "ghcr.io/lxc/incus-compose/ic-sleep:{version}"

// runArgs holds the run() options, mirroring the run command's flags.
type runArgs struct {
	Service      string
	Command      []string
	Entrypoint   string
	Env          []string
	Labels       []string
	Volumes      []string
	Publish      []string
	ServicePorts bool
	User         string
	Group        string
	Workdir      string
	Name         string
	Init         string
	Remove       bool
	NoDeps       bool
	Detach       bool
	NoTTY        bool
	Build        client.BuildInfo
	Pull         client.PullMode
	Timeout      time.Duration
	Workers      int
	Debug        bool
	Writer       io.Writer
}

// run creates a one-off instance from a service and executes the command in it.
func run(ctx context.Context, p *project.Project, c *client.Client, args runArgs) error {
	service, ok := p.Services[args.Service]
	if !ok {
		return errLogged.Wrap(fmt.Errorf("service %q not found", args.Service))
	}

	service, err := applyRunOptions(service, args)
	if err != nil {
		return errLogged.Wrap(err)
	}

	p.Services[args.Service] = service

	c.IgnoreError(client.ActionEnsure, client.ErrNotFound)

	volume, entrypoint, err := runTools(ctx, c, args)
	if err != nil {
		return err
	}

	oneOff := project.OneOff{
		Service:      args.Service,
		Name:         args.Name,
		Entrypoint:   entrypoint,
		Volume:       volume,
		Mount:        toolsMount,
		ServicePorts: args.ServicePorts || len(args.Publish) > 0,
	}

	// Before the renderer below, which pull() brings one of its own. The tools
	// image is runTools' above, and a build is the ensure's further down.
	err = pull(ctx, p, c, pullArgs{
		Services:        []string{args.Service},
		WithDeps:        !args.NoDeps,
		IgnoreBuildable: true,
		NoHealthd:       true,
		Pull:            args.Pull,
		Workers:         args.Workers,
		Debug:           args.Debug,
		Writer:          args.Writer,
	})
	if err != nil {
		return err
	}

	var progress *progressRenderer
	if !args.Debug {
		progress = newProgressRenderer(args.Writer, noColor(ctx), isatty.IsTerminal(os.Stdout.Fd()))
		progress.Start(c)
		defer progress.Stop(c)
	}

	err = c.RegisterDNSWatcher()
	if err != nil {
		c.LogError("Registering the DNS watcher", "project", p.Name, "error", err)
		return errLogged.Wrap(err)
	}

	resources, err := p.Resources(c, project.ResourcesOneOff(oneOff))
	if err != nil {
		c.LogError("Getting project resources", "error", err)
		return errLogged.Wrap(err)
	}

	order, err := p.ServiceOrder(false)
	if err != nil {
		c.LogError("Getting the service dependency order", "error", err)
		return errLogged.Wrap(err)
	}

	myResources := filterResources(p, resources, filterResourcesArgs{
		OnlyServices:     []string{args.Service},
		WithDependencies: !args.NoDeps,
	})

	stack := client.NewStack(c, client.StackWorkers(args.Workers), client.StackFailFast())
	stack.AddOrdered(order, myResources)

	opts := []client.Option{
		client.OptionCreate(),
		client.OptionTimeout(args.Timeout),
		client.OptionPullMode(args.Pull),
	}
	if args.Build.Mode != client.BuildAuto || args.Build.PreferredBuilder != "" {
		opts = append(opts, client.OptionBuild(args.Build))
	}

	err = stack.ForAction(client.ActionEnsure).Run(ctx, client.ActionEnsure, opts...)
	if err != nil {
		c.LogError("Ensuring resources", "error", err)
		return errLogged.Wrap(err)
	}

	started := func(r client.Resource) bool { return r.IsEnsured() }

	err = stack.ForActionF(client.ActionStart, started).Run(ctx, client.ActionStart, opts...)
	if err != nil {
		c.LogError("Starting resources", "error", err)
		return errLogged.Wrap(err)
	}

	instance, image, err := oneOffResources(myResources[args.Service], args.Name)
	if err != nil {
		return errLogged.Wrap(err)
	}

	if args.Remove {
		defer removeOneOff(ctx, c, instance, args.Timeout)
	}

	if args.Detach {
		if progress != nil {
			progress.Stop(c)
		}

		_, err = fmt.Fprintln(args.Writer, instance.IncusName())

		return err
	}

	// The renderer owns the terminal until here, and the command wants it.
	if progress != nil {
		progress.Stop(c)
	}

	return execOneOff(ctx, c, instance, args, image, service)
}

// applyRunOptions rewrites the service the way docker's own run does, in memory
// and before the translation, so a one-off is created like anything else.
func applyRunOptions(service types.ServiceConfig, args runArgs) (types.ServiceConfig, error) {
	if args.Entrypoint != "" {
		parsed, err := shellquote.Split(args.Entrypoint)
		if err != nil {
			return service, fmt.Errorf("parsing --entrypoint: %w", err)
		}

		service.Entrypoint = parsed
	}

	if len(args.Command) > 0 {
		service.Command = args.Command
	}

	if args.User != "" {
		service.User = args.User
	}

	if args.Workdir != "" {
		service.WorkingDir = args.Workdir
	}

	service.Environment = maps.Clone(service.Environment)
	if service.Environment == nil {
		service.Environment = types.MappingWithEquals{}
	}

	for _, e := range args.Env {
		key, value, _ := strings.Cut(e, "=")
		service.Environment[key] = &value
	}

	service.Labels = maps.Clone(service.Labels)
	if service.Labels == nil {
		service.Labels = types.Labels{}
	}

	for _, l := range args.Labels {
		key, value, _ := strings.Cut(l, "=")
		service.Labels[key] = value
	}

	for _, v := range args.Volumes {
		parsed, err := format.ParseVolume(v)
		if err != nil {
			return service, fmt.Errorf("parsing --volume %q: %w", v, err)
		}

		service.Volumes = append(slices.Clone(service.Volumes), parsed)
	}

	if len(args.Publish) > 0 {
		ports, err := types.ParsePortConfig(strings.Join(args.Publish, ","))
		if err != nil {
			return service, fmt.Errorf("parsing --publish: %w", err)
		}

		service.Ports = ports
	}

	return service, nil
}

// runTools puts the blocking helper where the one-off can run it.
func runTools(ctx context.Context, c *client.Client, args runArgs) (*client.StorageVolume, string, error) {
	sys, err := c.Global().EnsureProject(systemProject, client.EnsureProjectWithCreate())
	if err != nil {
		c.LogError("Getting the system project", "project", systemProject, "error", err)
		return nil, "", errLogged.Wrap(err)
	}

	// Done removes the stopped instance the helpers image was read through.
	defer sys.WarnError(sys.Done, "Failure during Client.Done() on the system project")

	// Resolved once: {version} is what the flag holds, and an error naming that
	// sends the reader looking for a tag nobody ever asked a registry for.
	name := resolveImageVersion(args.Init)

	res, err := sys.Resource(client.KindImage, name, &client.ImageConfig{})
	if err != nil {
		return nil, "", errLogged.Wrap(err)
	}

	err = client.RunAction(ctx, res, client.ActionEnsure, client.OptionCreate())
	if err != nil {
		c.LogError("Fetching the tools image", "image", name, "error", err)
		c.LogError("`run` execs into it. Fetch it with `incus-compose pull` while connected, " +
			"or point --init or x-incus-compose.init at an image this server can reach")

		return nil, "", errLogged.Wrap(err)
	}

	image, ok := res.(*client.Image)
	if !ok {
		return nil, "", errLogged.Wrap(client.ErrUnknownResource.WithText(name))
	}

	volume, entrypoint, err := ensureTools(ctx, c, sys, image)
	if err != nil {
		c.LogError("Preparing the tools volume", "error", err)
		return nil, "", errLogged.Wrap(err)
	}

	return volume, entrypoint, nil
}

// oneOffResources picks the one-off and the image it runs out of what the
// stack ensured. The image carries the entrypoint the exec needs.
func oneOffResources(resources []client.Resource, name string) (*client.Instance, *client.Image, error) {
	var instance *client.Instance
	var image *client.Image

	for _, r := range resources {
		switch res := r.(type) {
		case *client.Instance:
			if res.Name() == name {
				instance = res
			}
		case *client.Image:
			image = res
		}
	}

	if instance == nil {
		return nil, nil, client.ErrNotFound.WithText("the one-off instance " + name)
	}

	if image == nil {
		return nil, nil, client.ErrNotFound.WithText("the image of " + name)
	}

	return instance, image, nil
}

// removeOneOff takes the instance down again, which --rm asks for.
func removeOneOff(ctx context.Context, c *client.Client, instance *client.Instance, timeout time.Duration) {
	// The command may have been interrupted, and the instance still has to go.
	ctx = context.WithoutCancel(ctx)

	c.IgnoreError(client.ActionStop, client.ErrNotRunning)

	err := client.RunAction(ctx, instance, client.ActionStop,
		client.OptionForce(), client.OptionTimeout(timeout), client.OptionNoHealthd())
	if err != nil {
		c.LogWarn("Stopping the one-off", "instance", instance.IncusName(), "error", err)
	}

	err = client.RunAction(ctx, instance, client.ActionDelete,
		client.OptionForce(), client.OptionNoHealthd())
	if err != nil {
		c.LogWarn("Deleting the one-off", "instance", instance.IncusName(), "error", err)
	}
}

// execOneOff runs the service's own command in the one-off and exits with its
// status. The instance runs the blocking helper, so this exec is the only
// thing that ever reports how the command ended.
func execOneOff(ctx context.Context, c *client.Client, instance *client.Instance, args runArgs, image *client.Image, service types.ServiceConfig) error {
	command, err := oneOffCommand(image, service)
	if err != nil {
		return errLogged.Wrap(err)
	}

	if len(command) == 0 {
		return errLogged.Wrap(errors.New("the service defines no command to run"))
	}

	execPath, err := exec.LookPath("incus")
	if err != nil {
		c.LogError("`incus` not found in PATH")
		return errLogged.Wrap(errors.New("'incus' not found in PATH"))
	}

	iArgs := []string{"exec"}
	if args.NoTTY {
		iArgs = append(iArgs, "--mode", "non-interactive")
	}

	for _, e := range args.Env {
		iArgs = append(iArgs, "--env", e)
	}

	// oci.cwd, oci.uid and oci.gid are the instance's, and an exec inherits
	// none of them.
	cwd := args.Workdir
	if cwd == "" {
		cwd = image.State().Cwd
	}

	if cwd != "" {
		iArgs = append(iArgs, "--cwd", cwd)
	}

	if args.Group != "" {
		iArgs = append(iArgs, "--group", args.Group)
	} else {
		iArgs = append(iArgs, "--group", strconv.FormatUint(instance.State().GID, 10))
	}

	iArgs = append(iArgs, "--user", strconv.FormatUint(instance.State().UID, 10))
	iArgs = append(iArgs, instance.IncusName(), "--")
	iArgs = append(iArgs, command...)

	execCmd := exec.CommandContext(ctx, execPath, iArgs...) //nolint:gosec
	execCmd.Stdin = os.Stdin
	execCmd.Stdout = args.Writer
	execCmd.Stderr = os.Stderr
	execCmd.Env = append(os.Environ(), "INCUS_PROJECT="+c.IncusProject())

	err = execCmd.Run()

	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return errExitCode(exitErr.ExitCode())
	}

	if err != nil {
		return errLogged.Wrap(err)
	}

	return nil
}

// oneOffCommand is the argv the exec runs. The instance itself no longer
// carries it: its entrypoint is the helper, so this is the only place the
// service's own entrypoint and command are still put together.
func oneOffCommand(image *client.Image, service types.ServiceConfig) ([]string, error) {
	// An entrypoint of its own drops the image's command, as docker's does.
	if service.Entrypoint != nil {
		return slices.Concat(service.Entrypoint, service.Command), nil
	}

	entrypoint, err := shellquote.Split(image.State().Entrypoint)
	if err != nil {
		return nil, fmt.Errorf("reading the image entrypoint: %w", err)
	}

	if len(service.Command) > 0 {
		return append(entrypoint, service.Command...), nil
	}

	command, err := shellquote.Split(image.State().Cmd)
	if err != nil {
		return nil, fmt.Errorf("reading the image command: %w", err)
	}

	return append(entrypoint, command...), nil
}

// newRunCommand implements `incus-compose run`, which `docker compose run`
// calls a one-off container.
func newRunCommand() *cli.Command {
	nthArg := 1

	return &cli.Command{
		Name:      "run",
		Usage:     "Run a one-off command on a service",
		Category:  "compose",
		ArgsUsage: "SERVICE [COMMAND] [ARGS...]",
		Flags: []cli.Flag{
			&cli.BoolFlag{
				Name:    "rm",
				Usage:   "Remove the instance after the command exits",
				Sources: cli.EnvVars("INCUS_COMPOSE_RUN_RM"),
			},
			&cli.BoolFlag{
				Name:    "detach",
				Aliases: []string{"d"},
				Usage:   "Detached mode: print the instance name and return",
				Sources: cli.EnvVars("INCUS_COMPOSE_RUN_DETACH"),
			},
			&cli.StringSliceFlag{
				Name:    "env",
				Aliases: []string{"e"},
				Usage:   "Set environment variables (KEY=VALUE). May be specified multiple times.",
				Sources: cli.EnvVars("INCUS_COMPOSE_RUN_ENV"),
			},
			&cli.StringSliceFlag{
				Name:    "label",
				Aliases: []string{"l"},
				Usage:   "Add a label (KEY=VALUE). May be specified multiple times.",
				Sources: cli.EnvVars("INCUS_COMPOSE_RUN_LABEL"),
			},
			&cli.StringSliceFlag{
				Name:    "volume",
				Aliases: []string{"v"},
				Usage:   "Bind mount a volume. May be specified multiple times.",
				Sources: cli.EnvVars("INCUS_COMPOSE_RUN_VOLUME"),
			},
			&cli.StringSliceFlag{
				Name:    "publish",
				Aliases: []string{"p"},
				Usage:   "Publish a port. May be specified multiple times.",
				Sources: cli.EnvVars("INCUS_COMPOSE_RUN_PUBLISH"),
			},
			&cli.BoolFlag{
				Name:    "service-ports",
				Aliases: []string{"P"},
				Usage:   "Keep the ports the service declares",
				Sources: cli.EnvVars("INCUS_COMPOSE_RUN_SERVICE_PORTS"),
			},
			&cli.StringFlag{
				Name:    "entrypoint",
				Usage:   "Override the image entrypoint",
				Sources: cli.EnvVars("INCUS_COMPOSE_RUN_ENTRYPOINT"),
			},
			&cli.StringFlag{
				Name:        "user",
				Aliases:     []string{"u"},
				Usage:       "Run as this user",
				DefaultText: `the instance's UID`,
				Sources:     cli.EnvVars("INCUS_COMPOSE_RUN_USER"),
			},
			&cli.StringFlag{
				Name:        "group",
				Usage:       "Run as this group",
				DefaultText: `the instance's GID`,
				Sources:     cli.EnvVars("INCUS_COMPOSE_RUN_GROUP"),
			},
			&cli.StringFlag{
				Name:    "workdir",
				Aliases: []string{"w"},
				Usage:   "Working directory for the command",
				Sources: cli.EnvVars("INCUS_COMPOSE_RUN_WORKDIR"),
			},
			&cli.StringFlag{
				Name:    "name",
				Usage:   "Name for the one-off instance",
				Sources: cli.EnvVars("INCUS_COMPOSE_RUN_NAME"),
			},
			&cli.BoolFlag{
				Name:    "no-tty",
				Aliases: []string{"T"},
				Usage:   "Disable pseudo-TTY allocation",
				Sources: cli.EnvVars("INCUS_COMPOSE_RUN_NO_TTY"),
			},
			&cli.BoolFlag{
				Name:    "no-deps",
				Usage:   "Don't start the services this one depends on",
				Sources: cli.EnvVars("INCUS_COMPOSE_RUN_NO_DEPS"),
			},
			&cli.BoolFlag{
				Name:    "build",
				Usage:   "Build the image before running, even when it is cached",
				Sources: cli.EnvVars("INCUS_COMPOSE_RUN_BUILD"),
			},
			&cli.BoolFlag{
				Name:    "no-build",
				Usage:   "Never build, fail if the image is missing",
				Sources: cli.EnvVars("INCUS_COMPOSE_RUN_NO_BUILD"),
			},
			&cli.StringFlag{
				Name:    "builder",
				Usage:   "Preferred builder, binary name or absolute path. Empty for auto-detect.",
				Sources: cli.EnvVars("INCUS_COMPOSE_RUN_BUILDER"),
			},
			&cli.StringFlag{
				Name:    "pull",
				Usage:   `Pull image before running ("always"|"missing"|"never")`,
				Value:   "missing",
				Sources: cli.EnvVars("INCUS_COMPOSE_RUN_PULL"),
			},
			&cli.StringFlag{
				Name:    "init",
				Usage:   "Image the blocking helper comes from",
				Value:   DefaultInitImage,
				Sources: cli.EnvVars("INCUS_COMPOSE_INIT_IMAGE"),
			},
			&cli.DurationFlag{
				Name:    "timeout",
				Usage:   "Timeout for creating and stopping the one-off",
				Value:   2 * time.Minute,
				Sources: cli.EnvVars("INCUS_COMPOSE_RUN_TIMEOUT"),
			},
		},
		// Everything after SERVICE is the command, not our flags.
		StopOnNthArg: &nthArg,
		Action: func(ctx context.Context, cmd *cli.Command) error {
			all := cmd.Args().Slice()
			if len(all) < 1 {
				return fmt.Errorf("usage: %s SERVICE [COMMAND] [ARGS...]", cmd.Name)
			}

			// Docker's daemon reclaims a detached one-off when it exits. Nothing
			// here watches for that, so the two together would delete the
			// instance the moment its name is printed.
			if cmd.Bool("rm") && cmd.Bool("detach") {
				return errors.New("--rm cannot be used with --detach; remove it yourself, or drop --detach")
			}

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

			buildMode := client.BuildAuto
			if cmd.Bool("build") {
				buildMode = client.BuildForce
			} else if cmd.Bool("no-build") {
				buildMode = client.BuildNever
			}

			pullMode := client.PullMissing
			switch cmd.String("pull") {
			case "always":
				pullMode = client.PullAlways
			case "never":
				pullMode = client.PullNever
			}

			name := cmd.String("name")
			if name == "" {
				name = all[0] + "-run-" + strings.ToLower(client.RandString(8))
			}

			return run(ctx, p, c, runArgs{
				Service:      all[0],
				Command:      all[1:],
				Entrypoint:   cmd.String("entrypoint"),
				Env:          cmd.StringSlice("env"),
				Labels:       cmd.StringSlice("label"),
				Volumes:      cmd.StringSlice("volume"),
				Publish:      cmd.StringSlice("publish"),
				ServicePorts: cmd.Bool("service-ports"),
				User:         cmd.String("user"),
				Group:        cmd.String("group"),
				Workdir:      cmd.String("workdir"),
				Name:         name,
				Init:         cmd.String("init"),
				Remove:       cmd.Bool("rm"),
				NoDeps:       cmd.Bool("no-deps"),
				Detach:       cmd.Bool("detach"),
				NoTTY:        cmd.Bool("no-tty"),
				Build:        client.BuildInfo{Mode: buildMode, PreferredBuilder: cmd.String("builder")},
				Pull:         pullMode,
				Timeout:      cmd.Duration("timeout"),
				Workers:      cmd.Root().Int("workers"),
				Debug:        cmd.Root().Bool("debug"),
				Writer:       cmd.Root().Writer,
			})
		},
	}
}

// oneOffInstances returns the project's one-offs, which are nobody's declared
// service and so appear in no resource map.
func oneOffInstances(ctx context.Context, c *client.Client) ([]incusApi.InstanceFull, error) {
	conn, err := c.Connection()
	if err != nil {
		return nil, err
	}

	instances, err := conn.GetInstances(ctx, nil)
	if err != nil {
		return nil, err
	}

	found := []incusApi.InstanceFull{}

	for _, inst := range instances {
		if util.IsTrue(inst.Config[project.OneOffKey]) {
			found = append(found, inst)
		}
	}

	return found, nil
}

// removeOneOffs takes the project's one-offs down with it, as `docker compose
// down` does. Nothing else ever reclaims one that ran without --rm.
func removeOneOffs(ctx context.Context, c *client.Client, timeout time.Duration) {
	instances, err := oneOffInstances(ctx, c)
	if err != nil {
		c.LogWarn("Listing the one-off instances", "error", err)

		return
	}

	conn, err := c.Connection()
	if err != nil {
		return
	}

	for _, inst := range instances {
		if inst.StatusCode == incusApi.Running || inst.StatusCode == incusApi.Frozen {
			op, err := conn.UpdateInstanceState(ctx, inst.Name, incusApi.InstanceStatePut{
				Action:  "stop",
				Force:   true,
				Timeout: int(timeout.Seconds()),
			}, "")
			if err == nil {
				_, err = iclient.WaitOperation(ctx, op)
			}

			if err != nil {
				c.LogWarn("Stopping a one-off", "instance", inst.Name, "error", err)
			}
		}

		op, err := conn.DeleteInstance(ctx, inst.Name)
		if err == nil {
			_, err = iclient.WaitOperation(ctx, op)
		}

		if err != nil {
			c.LogWarn("Deleting a one-off", "instance", inst.Name, "error", err)
		}
	}
}
