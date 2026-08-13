package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/urfave/cli/v3"
	"go.yaml.in/yaml/v4"

	"github.com/lxc/incus-compose/client"
	"github.com/lxc/incus-compose/project"
)

func newBackupListCommand() *cli.Command {
	return &cli.Command{
		Name:  "list",
		Usage: "List backups of project volumes",
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:  "format",
				Usage: "Format the output. Values: [table | yaml | json]",
				Value: "table",
				Action: func(ctx context.Context, cmd *cli.Command, v string) error {
					if !slices.Contains([]string{"table", "yaml", "json"}, v) {
						return fmt.Errorf("invalid format: %s (must be table, yaml or json)", v)
					}
					return nil
				},
				Sources: cli.EnvVars("INCUS_COMPOSE_BACKUP_LIST_FORMAT"),
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
				return errLogged.Wrap(err)
			}

			c, err := globalClient.EnsureProject(p.Name)
			if err != nil {
				globalClient.LogError("Getting the incus project", "error", err)
				return errLogged.Wrap(err)
			}

			backupConfig := p.ClientConfig.Backup
			if cmd.String("pool") != "" {
				backupConfig.Pool = cmd.String("pool")
			}
			if backupConfig.Pool == "" {
				backupConfig.Pool = c.Config().DefaultStoragePool
			}
			if backupConfig.MetaVolume == "" {
				backupConfig.MetaVolume = "ic-backup-manifest"
			}

			bc, err := globalClient.EnsureProject(c.Project() + "-backup")
			if errors.Is(err, client.ErrNotFound) {
				return printBackupList(cmd.Root().Writer, cmd.String("format"), nil, backupConfig)
			}
			if err != nil {
				c.LogError("Getting the backup project", "error", err)
				return errLogged.Wrap(err)
			}
			err = bc.Open()
			if err != nil {
				globalClient.LogError("Opening the backup client", "error", err)
				return errLogged.Wrap(err)
			}
			defer func() { _ = bc.Done() }()

			rBMVol, err := bc.Resource(client.KindStorageVolume, backupConfig.MetaVolume, &client.StorageVolumeConfig{Pool: backupConfig.Pool})
			if err != nil {
				c.LogError("Getting the backup manifest volume", "error", err)
				return errLogged.Wrap(err)
			}

			err = client.RunAction(ctx, rBMVol, client.ActionEnsure)
			if errors.Is(err, client.ErrNotFound) {
				return printBackupList(cmd.Root().Writer, cmd.String("format"), nil, backupConfig)
			}
			if err != nil {
				c.LogError("Reading the backup manifest volume", "error", err)
				return errLogged.Wrap(err)
			}

			bMVol, ok := rBMVol.(*client.StorageVolume)
			if !ok {
				c.LogError("Converting a backup resource to a StorageVolume", "error", client.ErrUnknownResource)
				return errLogged.Wrap(client.ErrUnknownResource)
			}

			sc, err := bMVol.SFTP(ctx)
			if err != nil {
				c.LogError("Opening an SFTP client", "error", err)
				return errLogged.Wrap(err)
			}
			defer c.WarnError(sc.Close, "Failed to close an SFTP connection")

			entries, err := sc.ReadDir("/")
			if err != nil {
				c.LogError("Listing the backup manifest volume", "error", err)
				return errLogged.Wrap(err)
			}

			manifests := []backupManifest{}
			for _, entry := range entries {
				if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
					continue
				}

				f, err := sc.Open(entry.Name())
				if err != nil {
					c.LogError("Opening a backup manifest", "manifest", entry.Name(), "error", err)
					return errLogged.Wrap(err)
				}

				m := backupManifest{}
				err = json.NewDecoder(f).Decode(&m)
				_ = f.Close()
				if err != nil {
					c.LogError("Decoding a backup manifest", "manifest", entry.Name(), "error", err)
					return errLogged.Wrap(err)
				}

				manifests = append(manifests, m)
			}

			sortBackupList(manifests)

			return printBackupList(cmd.Root().Writer, cmd.String("format"), manifests, backupConfig)
		},
	}
}

// sortBackupList orders manifests by timestamp, newest first. Malformed
// timestamps parse as the zero time and sort last.
func sortBackupList(manifests []backupManifest) {
	slices.SortFunc(manifests, func(a, b backupManifest) int {
		at, _ := time.Parse(time.RFC3339Nano, a.Timestamp)
		bt, _ := time.Parse(time.RFC3339Nano, b.Timestamp)
		return bt.Compare(at)
	})
}

// printBackupList renders the manifests in the requested format. nil means
// there are no backups, which renders like incus list: a bare table header
// or an empty list.
func printBackupList(w io.Writer, format string, manifests []backupManifest, cfg client.BackupConfig) error {
	switch format {
	case "table":
		tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		_, err := fmt.Fprintln(tw, "TIMESTAMP\tNAME\tVOLUMES\tPOOL")
		if err != nil {
			return err
		}

		for _, m := range manifests {
			pool := cfg.Pool
			if len(m.Volumes) > 0 && m.Volumes[0].Backup.Pool != "" {
				pool = m.Volumes[0].Backup.Pool
			}

			// The name is optional; the table shows a placeholder for it.
			name := m.Name
			if name == "" {
				name = "---"
			}

			_, err := fmt.Fprintf(tw, "%s\t%s\t%d\t%s\n", m.Timestamp, name, len(m.Volumes), pool)
			if err != nil {
				return err
			}
		}

		return tw.Flush()
	case "json":
		if manifests == nil {
			manifests = []backupManifest{}
		}

		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(manifests)
	case "yaml":
		if manifests == nil {
			manifests = []backupManifest{}
		}
		return yaml.NewEncoder(w).Encode(manifests)
	default:
		return fmt.Errorf("unknown format: %s", format)
	}
}
