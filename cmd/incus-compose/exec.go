package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"

	"github.com/urfave/cli/v3"

	"github.com/lxc/incus-compose/client"
	"github.com/lxc/incus-compose/project"
)

// execCommand implements `incus-compose exec` similar to `docker compose exec`.
func newExecCommand() *cli.Command {
	nthArg := 1
	return &cli.Command{
		Name:      "exec",
		Usage:     "Execute a command in a running instance",
		Category:  "compose",
		ArgsUsage: "SERVICE COMMAND [ARGS...]",
		Flags: []cli.Flag{
			&cli.BoolFlag{
				Name:    "detach",
				Aliases: []string{"d"},
				Usage:   "Detached mode: Run command in the background",
				Sources: cli.EnvVars("INCUS_COMPOSE_EXEC_DETACH"),
			},
			&cli.BoolFlag{
				Name:  "dry-run",
				Usage: "Execute command in dry run mode",
			},
			&cli.StringSliceFlag{
				Name:    "env",
				Aliases: []string{"e"},
				Usage:   "Set environment variables (KEY=VALUE). May be specified multiple times.",
				Sources: cli.EnvVars("INCUS_COMPOSE_EXEC_ENV"),
			},
			&cli.IntFlag{
				Name:    "index",
				Usage:   "Index of the container if service has multiple replicas",
				Value:   0,
				Sources: cli.EnvVars("INCUS_COMPOSE_EXEC_INDEX"),
			},
			&cli.BoolFlag{
				Name:    "no-tty",
				Usage:   "Disable pseudo-TTY allocation. By default a TTY is allocated when available.",
				Aliases: []string{"T"},
				Sources: cli.EnvVars("INCUS_COMPOSE_EXEC_NO_TTY"),
			},
			&cli.BoolFlag{
				Name:    "privileged",
				Usage:   "Give extended privileges to the process (accepted but not implemented)",
				Sources: cli.EnvVars("INCUS_COMPOSE_EXEC_PRIVILEGED"),
			},
			&cli.StringFlag{
				Name:        "user",
				Aliases:     []string{"u"},
				Usage:       "Run the command as this user",
				DefaultText: `the instance's UID`,
				Sources:     cli.EnvVars("INCUS_COMPOSE_EXEC_USER"),
			},
			&cli.StringFlag{
				Name:        "group",
				Aliases:     []string{"g"},
				Usage:       "Run the command as this group",
				DefaultText: `the instance's GID`,
				Sources:     cli.EnvVars("INCUS_COMPOSE_EXEC_GROUP"),
			},
			&cli.StringFlag{
				Name:    "workdir",
				Aliases: []string{"w"},
				Usage:   "Path to workdir directory for this command",
				Sources: cli.EnvVars("INCUS_COMPOSE_EXEC_WORKDIR"),
			},
		},
		StopOnNthArg: &nthArg,
		Action: func(ctx context.Context, cmd *cli.Command) error {
			// Validate args
			args := cmd.Args().Slice()
			if len(args) < 2 {
				return fmt.Errorf("usage: %s SERVICE COMMAND [ARGS...]", cmd.Name)
			}
			service := args[0]
			args = args[1:]

			// Get global client from context
			globalClient, err := clientFromContext(ctx)
			if err != nil {
				return err
			}
			if err := globalClient.Connect(); err != nil {
				return err
			}

			// Load project
			p, err := project.New().Load(ctx, buildLoadOptions(cmd)...)
			if err != nil {
				globalClient.LogError("Configuring the project", "error", err)
				return errLogged.Wrap(err)
			}

			// Get the per Project client - don't create if it doesn't exist
			c, err := globalClient.EnsureProject(p.Name)
			if err != nil {
				globalClient.LogError("Getting the incus project", "error", err)
				return errLogged.Wrap(err)
			}
			if err := c.Open(); err != nil {
				globalClient.LogError("Opening the project client", "error", err)
				return errLogged.Wrap(err)
			}
			defer func() { _ = c.Done() }()

			allResources, err := p.Resources(c, project.ResourcesFull())
			if err != nil {
				c.LogError("Getting project resources in reCreate", "error", err)
				return errLogged.Wrap(err)
			}

			resources, ok := allResources[service]
			if !ok {
				c.LogError("No service", "service", service)
				return errLogged.Wrap(client.ErrNotFound.WithText("service not found"))
			}

			instances := []*client.Instance{}
			for _, r := range resources {
				if r.Kind() == client.KindInstance {
					i, ok := r.(*client.Instance)
					if !ok {
						continue
					}

					if i.ServiceName() == service {
						instances = append(instances, i)
					}
				}
			}

			if len(instances) == 0 {
				c.LogError("No instance for service", "service", service)
				return errLogged.Wrap(client.ErrNotFound.WithText("service instance not found"))
			}

			if cmd.Int("index") >= len(instances) || cmd.Int("index") < 0 {
				c.LogError("Not enough instances", "have", len(instances), "expected", cmd.Int("index"))
				return errLogged.Wrap(client.ErrNotFound.WithText("not enough instances"))
			}

			inst := instances[cmd.Int("index")]

			err = client.RunAction(ctx, inst, client.ActionEnsure)
			if err != nil {
				globalClient.LogError("Failed to ensure the instance", "error", err)
				return errLogged.Wrap(fmt.Errorf("failed to ensure the instance: %w", err))
			}

			execPath, err := exec.LookPath("incus")
			if err != nil {
				globalClient.LogError("`incus` not found in PATH")
				return errLogged.Wrap(errors.New("'incus' not found in PATH"))
			}

			iArgs := []string{"exec"}
			if cmd.Bool("no-tty") {
				iArgs = append(iArgs, "--mode", "non-interactive")
			}

			for _, e := range cmd.StringSlice("env") {
				iArgs = append(iArgs, "--env", e)
			}

			if cmd.String("workdir") != "" {
				iArgs = append(iArgs, "--cwd", cmd.String("workdir"))
			}

			if cmd.String("user") != "" {
				iArgs = append(iArgs, "--user", cmd.String("user"))
			} else {
				iArgs = append(iArgs, "--user", strconv.FormatUint(inst.State().UID, 10))
			}

			if cmd.String("group") != "" {
				iArgs = append(iArgs, "--group", cmd.String("group"))
			} else {
				iArgs = append(iArgs, "--group", strconv.FormatUint(inst.State().GID, 10))
			}

			iArgs = append(iArgs, instances[cmd.Int("index")].IncusName())
			// Terminate incus's own flag parsing so a command with leading dashes
			// (e.g. `ls -ln`, `sh -c`) is passed through verbatim.
			iArgs = append(iArgs, "--")
			iArgs = append(iArgs, args...)

			if cmd.Bool("dry-run") {
				_, err = fmt.Fprintf(cmd.Root().Writer, "%s %s", execPath, strings.Join(iArgs, " "))
				return err
			}

			execCmd := exec.CommandContext(ctx, execPath, iArgs...) //nolint:gosec
			execCmd.Stdin = os.Stdin
			execCmd.Stdout = cmd.Root().Writer
			execCmd.Stderr = cmd.Root().ErrWriter
			execCmd.Env = append(os.Environ(), "INCUS_PROJECT="+c.IncusProject())

			if err := execCmd.Run(); err != nil {
				return errLogged.Wrap(err)
			}
			return nil
		},
	}
}
