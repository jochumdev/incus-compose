package main

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os/exec"
	"slices"
	"strconv"
	"strings"

	"github.com/urfave/cli/v3"
)

// copyPath is one side of a cp argument: a path inside a service's instance
// when service is set, a local path otherwise.
type copyPath struct {
	service string
	path    string
}

// splitCopyPath reads "SERVICE:PATH" when the part before the colon names a
// compose service. Everything else is local, which is what keeps "-", a
// Windows drive and a relative path holding a colon out of the service side.
func splitCopyPath(arg string, services []string) copyPath {
	name, path, ok := strings.Cut(arg, ":")
	if !ok || !slices.Contains(services, name) {
		return copyPath{path: arg}
	}

	return copyPath{service: name, path: path}
}

// instancePath renders the instance-relative half as incus spells it,
// "<instance>/<path>", which resolves from the instance's root.
func instancePath(instance string, path string) string {
	return instance + "/" + strings.TrimPrefix(path, "/")
}

// newCpCommand implements `incus-compose cp` similar to `docker compose cp`.
func newCpCommand() *cli.Command {
	return &cli.Command{
		Name:      "cp",
		Usage:     "Copy files between a service's instance and the local filesystem",
		Category:  "compose",
		ArgsUsage: "SERVICE:SRC_PATH DEST_PATH|- | SRC_PATH|- SERVICE:DEST_PATH",
		Flags: []cli.Flag{
			&cli.IntFlag{
				Name:    "index",
				Usage:   "Index of the container if service has multiple replicas",
				Value:   0,
				Sources: cli.EnvVars("INCUS_COMPOSE_CP_INDEX"),
			},
			&cli.BoolFlag{
				Name:    "archive",
				Aliases: []string{"a"},
				Usage:   "Archive mode: keep the source's uid/gid instead of the instance's",
				Sources: cli.EnvVars("INCUS_COMPOSE_CP_ARCHIVE"),
			},
			&cli.BoolFlag{
				Name:    "follow-link",
				Aliases: []string{"L"},
				Usage:   "Always follow symbolic links in SRC_PATH",
				Sources: cli.EnvVars("INCUS_COMPOSE_CP_FOLLOW_LINK"),
			},
			&cli.BoolFlag{
				Name:  "dry-run",
				Usage: "Print the incus command instead of running it",
			},
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			args := cmd.Args().Slice()
			if len(args) != 2 {
				return fmt.Errorf("usage: %s SERVICE:SRC_PATH DEST_PATH | %s SRC_PATH SERVICE:DEST_PATH", cmd.Name, cmd.Name)
			}

			p, c, err := loadProject(ctx, cmd)
			if err != nil {
				return err
			}

			err = c.Open()
			if err != nil {
				c.LogError("Opening the project client", "error", err)
				return errLogged.Wrap(err)
			}
			defer func() { _ = c.Done() }()

			services := slices.Sorted(maps.Keys(p.Services))
			src := splitCopyPath(args[0], services)
			dst := splitCopyPath(args[1], services)

			switch {
			case src.service != "" && dst.service != "":
				c.LogError("Copying between services is not supported", "source", src.service, "destination", dst.service)
				return errLogged.Wrap(fmt.Errorf("copying between services is not supported"))
			case src.service == "" && dst.service == "":
				c.LogError("Neither path names a service", "source", args[0], "destination", args[1],
					"services", strings.Join(services, ", "))
				return errLogged.Wrap(fmt.Errorf("one path must be SERVICE:PATH"))
			}

			service := src.service
			if service == "" {
				service = dst.service
			}

			inst, err := serviceInstance(ctx, c, p, service, cmd.Int("index"))
			if err != nil {
				return err
			}

			// Recursive throughout: a directory needs it, a plain file does
			// not mind, and it is what leaves a symlink a symlink as docker does.
			iArgs := []string{"file", "", "--recursive"}
			if cmd.Bool("follow-link") {
				iArgs = append(iArgs, "--dereference")
			}

			if src.service != "" {
				iArgs[1] = "pull"
				iArgs = append(iArgs, instancePath(inst.IncusName(), src.path), dst.path)
			} else {
				iArgs[1] = "push"

				// As with configs and secrets, a file lands owned by the
				// instance's user rather than root; --archive keeps the source's.
				if !cmd.Bool("archive") {
					iArgs = append(iArgs,
						"--uid", strconv.FormatUint(inst.State().UID, 10),
						"--gid", strconv.FormatUint(inst.State().GID, 10))
				}

				iArgs = append(iArgs, src.path, instancePath(inst.IncusName(), dst.path))
			}

			if cmd.Bool("dry-run") {
				execPath, err := exec.LookPath("incus")
				if err != nil {
					return errors.New("'incus' not found in PATH")
				}

				_, err = fmt.Fprintf(cmd.Root().Writer, "%s %s", execPath, strings.Join(iArgs, " "))
				return err
			}

			return runIncus(ctx, cmd.Root().Writer, cmd.Root().ErrWriter, []string{"INCUS_PROJECT=" + c.IncusProject()}, iArgs...)
		},
	}
}
