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

	"github.com/mattn/go-isatty"
	"github.com/urfave/cli/v3"

	"github.com/lxc/incus-compose/client"
	"github.com/lxc/incus-compose/project"
)

// backupRestoreConfirm asks before overwriting live data.
func backupRestoreConfirm(w io.Writer, timestamp string, volumes []string) (bool, error) {
	_, err := fmt.Fprintf(w, "This overwrites %d volume(s) with the backup taken at %s: %s\n",
		len(volumes), timestamp, strings.Join(volumes, ", "))
	if err != nil {
		return false, err
	}

	if !isatty.IsTerminal(os.Stdin.Fd()) {
		return false, errors.New("refusing to restore without a terminal to confirm on, pass --yes")
	}

	_, err = fmt.Fprint(w, "Everything written since is lost. Continue? [y/N] ")
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

func newBackupRestoreCommand() *cli.Command {
	return &cli.Command{
		Name:      "restore",
		Usage:     "Restore project volumes from a backup",
		ArgsUsage: "[TIMESTAMP] [SERVICE...]",
		Flags: []cli.Flag{
			&cli.StringSliceFlag{
				Name:    "volume",
				Usage:   "Restore only this volume (repeatable)",
				Sources: cli.EnvVars("INCUS_COMPOSE_BACKUP_RESTORE_VOLUME"),
			},
			&cli.BoolFlag{
				Name:  "dry-run",
				Usage: "Print what would be restored and stop",
			},
			&cli.BoolFlag{
				Name:  "yes",
				Usage: "Restore without asking; required without a terminal",
			},
			backupPoolFlag(),
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			p, c, err := loadProjectClient(ctx, cmd)
			if err != nil {
				return err
			}
			defer func() { _ = c.Done() }()

			backupConfig := resolveBackupConfig(cmd, p, c)

			bc, err := backupClient(c)
			if err != nil {
				c.LogError("Getting the backup project", "error", err)
				return errLogged.Wrap(err)
			}
			defer func() { _ = bc.Done() }()

			manifests, err := readBackupManifests(ctx, bc, backupConfig)
			if err != nil {
				c.LogError("Reading the backup manifests", "error", err)
				return errLogged.Wrap(err)
			}

			timestamp, services := splitBackupArgs(manifests, cmd.Args().Slice())

			m, err := pickBackupManifest(manifests, timestamp)
			if err != nil {
				c.LogError("Finding the backup", "error", err)
				return errLogged.Wrap(err)
			}

			backupConfig.Timestamp = m.Timestamp

			resources, err := p.Resources(c)
			if err != nil {
				c.LogError("Getting project resources", "error", err)
				return errLogged.Wrap(err)
			}

			myResources := filterResources(p, resources, filterResourcesArgs{
				OnlyServices: services,
				IncludeKinds: []client.Kind{client.KindStorageVolume},
			})

			volumes := restorableVolumes(m, myResources, cmd.StringSlice("volume"))
			if len(volumes) == 0 {
				c.LogWarn("No volumes to restore found")
				return nil
			}

			names := make([]string, 0, len(volumes))
			for _, v := range volumes {
				names = append(names, v.IncusName())
			}

			if cmd.Bool("dry-run") {
				_, err = fmt.Fprintf(cmd.Root().Writer, "Would restore %d volume(s) from %s: %s\n",
					len(names), m.Timestamp, strings.Join(names, ", "))

				return err
			}

			running, err := runningInstances(ctx, p, c, services)
			if err != nil {
				c.LogError("Checking which services are running", "error", err)
				return errLogged.Wrap(err)
			}

			if len(running) > 0 {
				c.LogError("Refusing to restore into running services",
					"services", strings.Join(running, ", "), "hint", "incus-compose stop")

				return errLogged.Wrap(errors.New("the services holding these volumes are running"))
			}

			if !cmd.Bool("yes") {
				ok, err := backupRestoreConfirm(cmd.Root().Writer, m.Timestamp, names)
				if err != nil {
					c.LogError("Confirming", "error", err)
					return errLogged.Wrap(err)
				}

				if !ok {
					c.LogInfo("Leaving the volumes alone")
					return nil
				}
			}

			var progress *progressRenderer
			if !cmd.Root().Bool("debug") {
				progress = newProgressRenderer(cmd.Root().Writer, noColor(ctx), isatty.IsTerminal(os.Stdout.Fd()))
				progress.Start(c)
				defer progress.Stop(c)
			}

			stack := client.NewStack(c, client.StackWorkers(cmd.Root().Int("workers")))
			for _, v := range volumes {
				stack.Add(v)
			}

			err = stack.ForAction(client.ActionEnsure).Run(ctx, client.ActionEnsure)
			if err != nil {
				c.LogError("Ensuring resources", "error", err)
				return errLogged.Wrap(err)
			}

			err = stack.ForAction(client.ActionRestore).Run(ctx, client.ActionRestore, client.OptionBackup(bc, backupConfig))
			if err != nil {
				c.LogError("Restoring resources", "error", err)
				return errLogged.Wrap(err)
			}

			return nil
		},
	}
}

// splitBackupArgs separates a leading timestamp from the service names. An
// argument only counts as a timestamp when a run carries it, so a service
// named like one still resolves.
func splitBackupArgs(manifests []backupManifest, args []string) (string, []string) {
	if len(args) == 0 {
		return "", nil
	}

	for _, m := range manifests {
		if m.Timestamp == args[0] {
			return args[0], args[1:]
		}
	}

	return "", args
}

// restorableVolumes returns the ensured volumes the run covers, narrowed to
// the names when any were given.
func restorableVolumes(m backupManifest, resources map[string][]client.Resource, only []string) []*client.StorageVolume {
	backed := make([]string, 0, len(m.Volumes))
	for _, v := range m.Volumes {
		backed = append(backed, v.Source.Name)
	}

	volumes := []*client.StorageVolume{}
	for _, res := range resources {
		for _, r := range res {
			v, ok := r.(*client.StorageVolume)
			if !ok || !slices.Contains(backed, r.IncusName()) {
				continue
			}

			if len(only) > 0 && !slices.Contains(only, r.Name()) && !slices.Contains(only, r.IncusName()) {
				continue
			}

			if slices.Contains(volumes, v) {
				continue
			}

			volumes = append(volumes, v)
		}
	}

	return volumes
}

// runningInstances returns the in-scope instances that are still up.
func runningInstances(ctx context.Context, p *project.Project, c *client.Client, services []string) ([]string, error) {
	resources, err := p.Resources(c)
	if err != nil {
		return nil, err
	}

	instances := filterResources(p, resources, filterResourcesArgs{
		OnlyServices: services,
		IncludeKinds: []client.Kind{client.KindInstance},
	})

	running := []string{}
	for _, res := range instances {
		for _, r := range res {
			i, ok := r.(*client.Instance)
			if !ok {
				continue
			}

			err := client.RunAction(ctx, i, client.ActionEnsure)
			if errors.Is(err, client.ErrNotFound) {
				continue
			}
			if err != nil {
				return nil, err
			}

			if i.Running() {
				running = append(running, i.IncusName())
			}
		}
	}

	return running, nil
}
