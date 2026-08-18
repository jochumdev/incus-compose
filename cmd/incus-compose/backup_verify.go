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

	"github.com/urfave/cli/v3"
	"go.yaml.in/yaml/v4"

	"github.com/lxc/incus-compose/client"
	"github.com/lxc/incus-compose/project"
)

// Backup verification outcomes, in the order the table reports them.
const (
	backupVerifyOK         = "ok"
	backupVerifyNoVolume   = "backup volume missing"
	backupVerifyNoSnapshot = "restore point missing"
	backupVerifyGone       = "no longer in the project"
	backupVerifyUncovered  = "not in this backup"
)

// backupVerifyResult is one volume's verdict.
type backupVerifyResult struct {
	Volume string `json:"volume"`
	Status string `json:"status"`
}

// backupVerifyReport is what the non-table formats render.
type backupVerifyReport struct {
	Timestamp string               `json:"timestamp"`
	Volumes   []backupVerifyResult `json:"volumes"`
}

func newBackupVerifyCommand() *cli.Command {
	return &cli.Command{
		Name:      "verify",
		Usage:     "Check that a backup's restore points are all there",
		ArgsUsage: "[TIMESTAMP]",
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
				Sources: cli.EnvVars("INCUS_COMPOSE_BACKUP_VERIFY_FORMAT"),
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

			backupConfig := resolveBackupConfig(cmd, p, c)

			bc, err := c.Global().EnsureProject(c.Project() + backupProjectSuffix)
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

			m, err := pickBackupManifest(manifests, cmd.Args().First())
			if err != nil {
				c.LogError("Finding the backup", "error", err)
				return errLogged.Wrap(err)
			}

			results := []backupVerifyResult{}
			for _, v := range m.Volumes {
				pool := v.Backup.Pool
				if pool == "" {
					pool = backupConfig.Pool
				}

				snapshots, err := client.BackupSnapshots(ctx, bc, pool, v.Backup.Name)
				if err != nil {
					c.LogError("Reading the backup volume", "volume", v.Backup.Name, "error", err)
					return errLogged.Wrap(err)
				}

				results = append(results, backupVerifyResult{
					Volume: v.Source.Name,
					Status: backupVerifyStatus(snapshots, m.Timestamp),
				})
			}

			current, err := projectVolumeNames(p, c)
			if err != nil {
				c.LogError("Listing the project volumes", "error", err)
				return errLogged.Wrap(err)
			}

			results = append(results, backupVerifyDrift(m, current)...)
			slices.SortFunc(results, func(a, b backupVerifyResult) int {
				return strings.Compare(a.Volume, b.Volume)
			})

			err = printBackupVerify(cmd.Root().Writer, cmd.String("format"), m, results)
			if err != nil {
				return err
			}

			for _, r := range results {
				if r.Status != backupVerifyOK {
					return errLogged.Wrap(errors.New("the backup does not match the project"))
				}
			}

			return nil
		},
	}
}

// backupVerifyStatus reports what a backup volume's snapshots say about one run.
func backupVerifyStatus(snapshots []string, timestamp string) string {
	if len(snapshots) == 0 {
		return backupVerifyNoVolume
	}

	if !slices.Contains(snapshots, timestamp) {
		return backupVerifyNoSnapshot
	}

	return backupVerifyOK
}

// backupVerifyDrift reports the volumes the run and the project disagree about.
func backupVerifyDrift(m backupManifest, current []string) []backupVerifyResult {
	backed := make([]string, 0, len(m.Volumes))
	for _, v := range m.Volumes {
		backed = append(backed, v.Source.Name)
	}

	results := []backupVerifyResult{}

	for _, name := range backed {
		if !slices.Contains(current, name) {
			results = append(results, backupVerifyResult{Volume: name, Status: backupVerifyGone})
		}
	}

	for _, name := range current {
		if !slices.Contains(backed, name) {
			results = append(results, backupVerifyResult{Volume: name, Status: backupVerifyUncovered})
		}
	}

	return results
}

// projectVolumeNames returns the Incus names of the volumes the compose file
// declares right now.
func projectVolumeNames(p *project.Project, c *client.Client) ([]string, error) {
	resources, err := p.Resources(c)
	if err != nil {
		return nil, err
	}

	names := []string{}
	for _, res := range resources {
		for _, r := range res {
			if r.Kind() != client.KindStorageVolume || slices.Contains(names, r.IncusName()) {
				continue
			}

			names = append(names, r.IncusName())
		}
	}

	return names, nil
}

// printBackupVerify renders the verdicts in the requested format.
func printBackupVerify(w io.Writer, format string, m backupManifest, results []backupVerifyResult) error {
	report := backupVerifyReport{Timestamp: m.Timestamp, Volumes: results}

	switch format {
	case "table":
		tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		_, err := fmt.Fprintln(tw, "VOLUME\tSTATUS")
		if err != nil {
			return err
		}

		for _, r := range results {
			_, err := fmt.Fprintf(tw, "%s\t%s\n", r.Volume, r.Status)
			if err != nil {
				return err
			}
		}

		return tw.Flush()
	case "json":
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(&report)
	case "yaml":
		return yaml.NewEncoder(w).Encode(&report)
	default:
		return fmt.Errorf("unknown format: %s", format)
	}
}
