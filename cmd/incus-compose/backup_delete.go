package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/urfave/cli/v3"

	"github.com/lxc/incus-compose/client"
)

func newBackupDeleteCommand() *cli.Command {
	return &cli.Command{
		Name:      "delete",
		Usage:     "Delete a backup, or prune all but the newest ones",
		ArgsUsage: "[TIMESTAMP]",
		Flags: []cli.Flag{
			&cli.IntFlag{
				Name:    "keep-last",
				Usage:   "Delete every backup but the newest N",
				Value:   -1,
				Sources: cli.EnvVars("INCUS_COMPOSE_BACKUP_DELETE_KEEP_LAST"),
			},
			backupPoolFlag(),
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
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

			keepLast := cmd.Int("keep-last")
			timestamp := cmd.Args().First()

			switch {
			case timestamp == "" && keepLast < 0:
				return errors.New("pass either a timestamp or --keep-last")
			case timestamp != "" && keepLast >= 0:
				return errors.New("pass a timestamp or --keep-last, not both")
			}

			backupConfig := resolveBackupConfig(cmd, p, c)

			bc, err := backupClient(c)
			if err != nil {
				c.LogError("Getting the backup project", "error", err)
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

			manifests, err := readBackupManifests(ctx, bc, backupConfig)
			if err != nil {
				c.LogError("Reading the backup manifests", "error", err)
				return errLogged.Wrap(err)
			}

			doomed, err := backupsToDelete(manifests, timestamp, keepLast)
			if err != nil {
				c.LogError("Finding the backup", "error", err)
				return errLogged.Wrap(err)
			}

			if len(doomed) == 0 {
				c.LogInfo("Nothing to delete")
				return nil
			}

			// The newest run is what the next refresh sends a delta against.
			if doomed[0].Timestamp == manifests[0].Timestamp {
				c.LogWarn("Deleting the newest backup; the next one transfers everything again")
			}

			for _, m := range doomed {
				err = deleteBackupRun(ctx, bc, m, backupConfig)
				if err != nil {
					c.LogError("Deleting a backup", "timestamp", m.Timestamp, "error", err)
					return errLogged.Wrap(err)
				}

				_, err = fmt.Fprintf(cmd.Root().Writer, "Deleted the backup taken at %s\n", m.Timestamp)
				if err != nil {
					return err
				}
			}

			return nil
		},
	}
}

// backupsToDelete picks the runs a timestamp or a retention count names.
// manifests are newest first.
func backupsToDelete(manifests []backupManifest, timestamp string, keepLast int) ([]backupManifest, error) {
	if timestamp != "" {
		m, err := pickBackupManifest(manifests, timestamp)
		if err != nil {
			return nil, err
		}

		return []backupManifest{m}, nil
	}

	if keepLast >= len(manifests) {
		return nil, nil
	}

	return manifests[keepLast:], nil
}

// deleteBackupRun removes one run's restore points and its manifest. The backup
// volumes stay: they are the base every later refresh sends a delta against.
func deleteBackupRun(ctx context.Context, bc *client.Client, m backupManifest, cfg client.BackupConfig) error {
	for _, v := range m.Volumes {
		pool := v.Backup.Pool
		if pool == "" {
			pool = cfg.Pool
		}

		err := client.BackupDeleteSnapshot(ctx, bc, pool, v.Backup.Name, m.Timestamp)
		if err != nil {
			return err
		}
	}

	cfg.Timestamp = m.Timestamp

	return client.BackupDeleteManifest(ctx, bc, cfg)
}
