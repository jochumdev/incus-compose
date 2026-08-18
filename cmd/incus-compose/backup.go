package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
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

	// Size is read back from the volumes at list time, never written.
	Size int64 `json:"size,omitempty"`
}

const (
	backupManifestVolume = client.BackupVolumePrefix + "manifest"

	// backupProjectSuffix names the Incus project a compose project backs up into.
	backupProjectSuffix = "-backup"
)

func newBackupCommand() *cli.Command {
	return &cli.Command{
		Name:     "backup",
		Usage:    "Snapshot project data volumes into a backup project",
		Category: "extensions",
		Commands: []*cli.Command{
			newBackupCreateCommand(),
			newBackupListCommand(),
			newBackupVerifyCommand(),
			newBackupRestoreCommand(),
			newBackupDeleteCommand(),
		},
	}
}

// backupPoolFlag is the pool override every backup subcommand takes.
func backupPoolFlag() cli.Flag {
	return &cli.StringFlag{
		Name:    "pool",
		Usage:   "Storage pool for backup volumes (overrides x-incus-compose.backup.pool)",
		Sources: cli.EnvVars("INCUS_COMPOSE_BACKUP_POOL"),
	}
}

// resolveBackupConfig fills the compose file's backup config from the flags and
// the project's defaults. Timestamp and Name are the caller's to set.
func resolveBackupConfig(cmd *cli.Command, p *project.Project, c *client.Client) client.BackupConfig {
	cfg := p.ClientConfig.Backup

	if cmd.String("pool") != "" {
		cfg.Pool = cmd.String("pool")
	}

	if cfg.Pool == "" {
		cfg.Pool = c.Config().DefaultStoragePool
	}

	if cfg.MetaVolume == "" {
		cfg.MetaVolume = backupManifestVolume
	}

	return cfg
}

// backupClient opens the project a compose project's backups live in. It
// does not create it: only backup create may, and the rest have nothing to say
// about a project that has never been backed up.
func backupClient(c *client.Client) (*client.Client, error) {
	bc, err := c.Global().EnsureProject(c.Project() + backupProjectSuffix)
	if err != nil {
		return nil, err
	}

	err = bc.Open()
	if err != nil {
		return nil, err
	}

	return bc, nil
}

// readBackupManifests returns every recorded run, newest first. A project or
// manifest volume that is not there yet reads as no runs.
func readBackupManifests(ctx context.Context, bc *client.Client, cfg client.BackupConfig) ([]backupManifest, error) {
	rBMVol, err := bc.Resource(client.KindStorageVolume, cfg.MetaVolume, &client.StorageVolumeConfig{Pool: cfg.Pool})
	if err != nil {
		return nil, err
	}

	err = client.RunAction(ctx, rBMVol, client.ActionEnsure)
	if errors.Is(err, client.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	bMVol, ok := rBMVol.(*client.StorageVolume)
	if !ok {
		return nil, client.ErrUnknownResource.WithText("while converting a backup resource to a StorageVolume")
	}

	sc, err := bMVol.SFTP(ctx)
	if err != nil {
		return nil, err
	}
	defer bc.WarnError(sc.Close, "Failed to close an SFTP connection")

	entries, err := sc.ReadDir("/")
	if err != nil {
		return nil, err
	}

	manifests := []backupManifest{}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}

		f, err := sc.Open(entry.Name())
		if err != nil {
			return nil, err
		}

		m := backupManifest{}
		err = json.NewDecoder(f).Decode(&m)
		_ = f.Close()
		if err != nil {
			return nil, fmt.Errorf("decoding the backup manifest %s: %w", entry.Name(), err)
		}

		manifests = append(manifests, m)
	}

	sortBackupList(manifests)

	return manifests, nil
}

// pickBackupManifest returns the run the timestamp names, or the newest one
// when it is empty.
func pickBackupManifest(manifests []backupManifest, timestamp string) (backupManifest, error) {
	if len(manifests) == 0 {
		return backupManifest{}, errors.New("no backups found")
	}

	if timestamp == "" {
		return manifests[0], nil
	}

	for _, m := range manifests {
		if m.Timestamp == timestamp {
			return m, nil
		}
	}

	return backupManifest{}, fmt.Errorf("no backup at %s", timestamp)
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
			backupPoolFlag(),
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
			defer func() { _ = c.Done() }()

			backupConfig := resolveBackupConfig(cmd, p, c)
			if cmd.String("name") != "" {
				backupConfig.Name = cmd.String("name")
			}
			backupConfig.Timestamp = time.Now().UTC().Format(time.RFC3339Nano)

			bc, err := c.Global().EnsureProject(
				c.Project()+backupProjectSuffix,
				client.EnsureProjectWithCreate(),
				client.EnsureProjectWithConfig(map[string]string{"restricted": "false"}),
			)
			if err != nil {
				c.LogError("Ensuring the backup project", "project", c.Project()+backupProjectSuffix, "error", err)
				return errLogged.Wrap(err)
			}
			err = bc.Open()
			if err != nil {
				c.LogError("Opening the backup client", "error", err)
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
				c.LogError("Failed to lock metadata", "error", err)
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

			before := c.Resources()

			err = stack.ForAction(client.ActionEnsure).Run(ctx, client.ActionEnsure)
			if err != nil {
				c.LogError("Ensuring resources", "error", err)
				return errLogged.Wrap(err)
			}

			// Volumes an instance carries are named by the instance, not the compose file.
			stack.Add(discoveredResources(before, c.Resources(), nil)...)

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
