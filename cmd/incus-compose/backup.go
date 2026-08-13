package main

import (
	"context"
	"encoding/json"
	"os"
	"time"

	"github.com/mattn/go-isatty"
	"github.com/urfave/cli/v3"

	"github.com/lxc/incus-compose/client"
	"github.com/lxc/incus-compose/project"
)

// backupManifest represents a single backup run.
type backupManifest struct {
	Timestamp string                `json:"timestamp"`
	Name      string                `json:"name"`
	Volumes   []client.BackupVolume `json:"volumes"`
}

const (
	backupManifestVolume = client.BackupVolumePrefix + "manifest"
)

func newBackupCommand() *cli.Command {
	return &cli.Command{
		Name:     "backup",
		Usage:    "Snapshot project data volumes into a backup project",
		Category: "extensions",
		Commands: []*cli.Command{
			newBackupCreateCommand(),
			newBackupListCommand(),
		},
	}
}

func newBackupCreateCommand() *cli.Command {
	return &cli.Command{
		Name:      "create",
		Usage:     "Create a backup of project volumes",
		ArgsUsage: "[SERVICE...]",
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:  "name",
				Usage: "Name for this backup",
			},
			&cli.BoolFlag{
				Name:  "live",
				Usage: "Snapshot volumes while services are running (crash-consistent)",
			},
			&cli.StringFlag{
				Name:    "pool",
				Usage:   "Storage pool for backup volumes (overrides x-incus-compose.backup.pool)",
				Sources: cli.EnvVars("INCUS_COMPOSE_BACKUP_POOL"),
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

			p, err := project.New().Load(ctx, buildLoadOptions(cmd)...)
			if err != nil {
				globalClient.LogError("Configuring the project", "error", err)
				return err
			}

			c, err := globalClient.EnsureProject(p.Name, client.EnsureProjectWithCreate(), client.EnsureProjectWithConfig(p.ClientConfig.XIncus))
			if err != nil {
				globalClient.LogError("Getting the incus project", "error", err)
				return errLogged
			}

			err = c.Open()
			if err != nil {
				globalClient.LogError("Opening the project client", "error", err)
				return errLogged.Wrap(err)
			}
			defer func() { _ = c.Done() }()

			backupConfig := p.ClientConfig.Backup
			if cmd.String("pool") != "" {
				backupConfig.Pool = cmd.String("pool")
			}
			if backupConfig.Pool == "" {
				backupConfig.Pool = c.Config().DefaultStoragePool
			}
			if cmd.String("name") != "" {
				backupConfig.Name = cmd.String("name")
			}
			if backupConfig.MetaVolume == "" {
				backupConfig.MetaVolume = "ic-backup-manifest"
			}
			backupConfig.Timestamp = time.Now().UTC().Format(time.RFC3339Nano)

			bc, err := c.Global().EnsureProject(
				c.Project()+"-backup",
				client.EnsureProjectWithCreate(),
				client.EnsureProjectWithConfig(map[string]string{"restricted": "false"}),
			)
			if err != nil {
				c.LogError("Ensuring the backup project", "project", c.Project()+"-backup", "error", err)
				return errLogged.Wrap(err)
			}
			err = bc.Open()
			if err != nil {
				globalClient.LogError("Opening the backup client", "error", err)
				return errLogged.Wrap(err)
			}
			defer func() { _ = bc.Done() }()

			bMVol, err := client.BackupManifestVolume(ctx, bc, backupConfig)
			if err != nil {
				c.LogError("Opening the backup manifest volume", "error", err)
				return errLogged.Wrap(err)
			}

			sc, err := bMVol.SFTP(ctx)
			if err != nil {
				c.LogError("Opening an SFTP client", "error", err)
				return errLogged.Wrap(err)
			}

			lock, err := client.BackupLock(ctx, bc, sc, backupConfig, 1*time.Minute, "metadata.lock")
			if err != nil {
				globalClient.LogError("Failed to lock metadata", "error", err)
				c.WarnError(sc.Close, "Failed to close the metadata lock sFTP connection")
				return errLogged.Wrap(err)
			}
			defer func() {
				c.WarnError(lock.Unlock, "Failed to release the metadata lock")
				c.WarnError(sc.Close, "Failed to close the metadata lock sFTP connection")
			}()

			resources, err := p.Resources(c)
			if err != nil {
				c.LogError("Getting project resources", "error", err)
				return errLogged.Wrap(err)
			}

			myResources := filterResources(p, resources, filterResourcesArgs{
				OnlyServices: cmd.Args().Slice(),
				IncludeKinds: []client.Kind{client.KindStorageVolume},
			})

			if len(myResources) == 0 {
				c.LogWarn("No volumes to backup found")
				return nil
			}

			if !cmd.Bool("live") {
				err = stop(ctx, p, c.Clone(), stopArgs{
					Services: cmd.Args().Slice(),
					Timeout:  2 * time.Minute,
					Workers:  cmd.Root().Int("workers"),
					Debug:    cmd.Root().Bool("debug"),
					Writer:   cmd.Root().Writer,
				})
				if err != nil {
					c.LogError("Stopping services for backup", "error", err)
					return err
				}
			}

			var progress *progressRenderer
			if !cmd.Root().Bool("debug") {
				progress = newProgressRenderer(cmd.Root().Writer, noColor(ctx), isatty.IsTerminal(os.Stdout.Fd()))
				progress.Start(c)
				defer progress.Stop(c)
			}

			order, err := p.ServiceOrder(false)
			if err != nil {
				c.LogError("Getting the service dependency order", "error", err)
				return errLogged.Wrap(err)
			}

			stack := client.NewStack(c, client.StackWorkers(cmd.Root().Int("workers")))
			stack.AddOrdered(order, myResources)

			c.LogDebug("Ensure", "resources", stack.All())

			err = stack.ForAction(client.ActionEnsure).Run(ctx, client.ActionEnsure)
			if err != nil {
				c.LogError("Ensuring resources", "error", err)
				return errLogged.Wrap(err)
			}

			ensuredFilter := func(r client.Resource) bool { return r.IsEnsured() }

			err = stack.ForActionF(client.ActionBackup, ensuredFilter).Run(ctx, client.ActionBackup, client.OptionBackup(bc, backupConfig))
			if err != nil {
				c.LogError("Backing up resources", "error", err)
				return errLogged.Wrap(err)
			}

			m := backupManifest{
				Name:      backupConfig.Name,
				Timestamp: backupConfig.Timestamp,
				Volumes:   []client.BackupVolume{},
			}
			for _, res := range myResources {
				for _, r := range res {
					if r.Kind() != client.KindStorageVolume {
						continue
					}

					v, ok := r.(*client.StorageVolume)
					if !ok {
						continue
					}

					m.Volumes = append(m.Volumes, v.BackupEntry(backupConfig, bc.IncusProject()))
				}
			}

			data, err := json.MarshalIndent(&m, "", "  ")
			if err != nil {
				c.LogError("Failed to marshal the backup manifest", "error", err)
				return errLogged.Wrap(err)
			}

			err = client.BackupWriteManifest(ctx, bc, backupConfig, data)
			if err != nil {
				c.LogError("Failed to write the backup manifest", "error", err)
				return errLogged.Wrap(err)
			}

			if progress != nil {
				progress.Stop(c)
			}

			if !cmd.Bool("live") {
				err = start(ctx, p, c.Clone(), startArgs{
					Services: cmd.Args().Slice(),
					Timeout:  2 * time.Minute,
					Workers:  cmd.Root().Int("workers"),
					Debug:    cmd.Root().Bool("debug"),
					Writer:   cmd.Root().Writer,
				})
				if err != nil {
					c.LogWarn("Restarting services after backup", "error", err)
				}
			}

			return nil
		},
	}
}
