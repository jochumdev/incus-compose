package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"text/tabwriter"
	"time"

	"github.com/lxc/incus/v7/shared/units"
	"github.com/urfave/cli/v3"
	"go.yaml.in/yaml/v4"

	"github.com/lxc/incus-compose/client"
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
			backupPoolFlag(),
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			p, c, err := loadProject(ctx, cmd)
			if err != nil {
				return err
			}

			backupConfig := resolveBackupConfig(cmd, p, c)

			bc, err := c.Global().EnsureProject(c.Project() + backupProjectSuffix)
			if errors.Is(err, client.ErrNotFound) {
				return printBackupList(cmd.Root().Writer, cmd.String("format"), nil, backupConfig)
			}
			if err != nil {
				c.LogError("Getting the backup project", "error", err)
				return errLogged.Wrap(err)
			}

			err = bc.Open()
			if err != nil {
				c.LogError("Opening the backup client", "error", err)
				return errLogged.Wrap(err)
			}
			defer func() { _ = bc.Done() }()

			manifests, err := readBackupManifests(ctx, bc, backupConfig)
			if err != nil {
				c.LogError("Reading the backup manifests", "error", err)
				return errLogged.Wrap(err)
			}

			for i := range manifests {
				manifests[i].Size, err = backupRunSize(ctx, bc, manifests[i], backupConfig)
				if err != nil {
					c.LogError("Reading the backup volume sizes", "error", err)
					return errLogged.Wrap(err)
				}
			}

			return printBackupList(cmd.Root().Writer, cmd.String("format"), manifests, backupConfig)
		},
	}
}

// backupRunSize sums what the run's backup volumes occupy. Incus reports usage
// per volume and not per snapshot, so runs sharing a volume report the same
// bytes rather than a per-restore-point figure.
func backupRunSize(ctx context.Context, bc *client.Client, m backupManifest, cfg client.BackupConfig) (int64, error) {
	var total int64

	for _, v := range m.Volumes {
		pool := v.Backup.Pool
		if pool == "" {
			pool = cfg.Pool
		}

		used, err := client.BackupVolumeUsage(ctx, bc, pool, v.Backup.Name)
		if err != nil {
			return 0, err
		}

		total += used
	}

	return total, nil
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
		_, err := fmt.Fprintln(tw, "TIMESTAMP\tNAME\tVOLUMES\tPOOL\tSIZE")
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

			_, err := fmt.Fprintf(tw, "%s\t%s\t%d\t%s\t%s\n", m.Timestamp, name, len(m.Volumes), pool, units.GetByteSizeString(m.Size, 1))
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
