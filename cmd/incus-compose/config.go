package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"

	"github.com/compose-spec/compose-go/v2/template"
	"github.com/compose-spec/compose-go/v2/types"
	"github.com/urfave/cli/v3"
	"go.yaml.in/yaml/v4"

	"github.com/lxc/incus-compose/project"
)

func newConfigCommand() *cli.Command {
	return &cli.Command{
		Name:      "config",
		Usage:     "Parse, resolve and render compose file in canonical format",
		Category:  "compose",
		ArgsUsage: "[SERVICE...]",
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:  "format",
				Usage: "Format the output. Values: [yaml | json]",
				Value: "yaml",
				Action: func(ctx context.Context, cmd *cli.Command, v string) error {
					if v != "yaml" && v != "json" {
						return fmt.Errorf("invalid format: %s (must be yaml or json)", v)
					}
					return nil
				},
				Sources: cli.EnvVars("INCUS_COMPOSE_CONFIG_FORMAT"),
			},
			&cli.BoolFlag{
				Name:    "services",
				Usage:   "Print the service names, one per line",
				Sources: cli.EnvVars("INCUS_COMPOSE_CONFIG_SERVICES"),
			},
			&cli.BoolFlag{
				Name:    "volumes",
				Usage:   "Print the volume names, one per line",
				Sources: cli.EnvVars("INCUS_COMPOSE_CONFIG_VOLUMES"),
			},
			&cli.BoolFlag{
				Name:    "networks",
				Usage:   "Print the network names, one per line",
				Sources: cli.EnvVars("INCUS_COMPOSE_CONFIG_NETWORKS"),
			},
			&cli.BoolFlag{
				Name:    "profiles",
				Usage:   "Print the profile names, one per line",
				Sources: cli.EnvVars("INCUS_COMPOSE_CONFIG_PROFILES"),
			},
			&cli.BoolFlag{
				Name:    "quiet",
				Aliases: []string{"q"},
				Usage:   "Only validate the configuration, don't print anything",
				Sources: cli.EnvVars("INCUS_COMPOSE_CONFIG_QUIET"),
			},
			&cli.BoolFlag{
				Name:    "images",
				Usage:   "Print the image names, one per line",
				Sources: cli.EnvVars("INCUS_COMPOSE_CONFIG_IMAGES"),
			},
			&cli.BoolFlag{
				Name:    "environment",
				Usage:   "Print environment used for interpolation",
				Sources: cli.EnvVars("INCUS_COMPOSE_CONFIG_ENVIRONMENT"),
			},
			&cli.BoolFlag{
				Name:    "variables",
				Usage:   "Print model variables and default values",
				Sources: cli.EnvVars("INCUS_COMPOSE_CONFIG_VARIABLES"),
			},
			&cli.StringFlag{
				Name:    "output",
				Aliases: []string{"o"},
				Usage:   "Save to file (default to stdout)",
				Sources: cli.EnvVars("INCUS_COMPOSE_CONFIG_OUTPUT"),
			},
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			loadOpts := buildLoadOptions(cmd)
			p, err := project.New().Load(ctx, buildLoadOptions(cmd)...)
			if err != nil {
				return err
			}

			services := types.Services{}
			if len(cmd.Args().Slice()) > 0 {
				for _, n := range cmd.Args().Slice() {
					s := p.Services[n]
					services[n] = s
					for depName := range s.DependsOn {
						services[depName] = p.Services[depName]
					}
				}
			} else {
				services = p.AllServices()
			}

			// If quiet, just validate and return
			if cmd.Bool("quiet") {
				return nil
			}

			// Determine output writer
			writer := cmd.Root().Writer
			if cmd.String("output") != "" {
				fp, err := os.OpenFile(cmd.String("output"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
				if err != nil {
					return err
				}

				writer = fp

				defer func() { _ = fp.Close() }()
			}

			// Handle filter-only options
			if cmd.Bool("services") {
				names := make([]string, 0, len(services))
				for name := range p.Services {
					names = append(names, name)
				}
				sort.Strings(names)
				for _, name := range names {
					_, _ = fmt.Fprintln(writer, name)
				}
				return nil
			}

			if cmd.Bool("volumes") {
				names := make([]string, 0, len(p.Volumes))
				for name := range p.Volumes {
					names = append(names, name)
				}
				sort.Strings(names)
				for _, name := range names {
					_, _ = fmt.Fprintln(writer, name)
				}
				return nil
			}

			if cmd.Bool("networks") {
				names := make([]string, 0, len(p.Networks))
				for name := range p.Networks {
					names = append(names, name)
				}
				sort.Strings(names)
				for _, name := range names {
					_, _ = fmt.Fprintln(writer, name)
				}
				return nil
			}

			if cmd.Bool("profiles") {
				profiles := make([]string, len(p.Profiles))
				copy(profiles, p.Profiles)
				sort.Strings(profiles)
				for _, profile := range profiles {
					_, _ = fmt.Fprintln(writer, profile)
				}
				return nil
			}

			if cmd.Bool("images") {
				// Print images in service order (not sorted, matching docker-compose behavior)
				for _, svc := range services {
					if svc.Image != "" {
						_, _ = fmt.Fprintln(writer, svc.Image)
					}
					for _, hook := range svc.PreStart {
						if hook.Image != "" && hook.Image != svc.Image {
							_, _ = fmt.Fprintln(writer, hook.Image)
						}
					}
				}
				return nil
			}

			if cmd.Bool("environment") {
				// Get environment variable names and sort them
				names := make([]string, 0, len(p.Environment))
				for name := range p.Environment {
					names = append(names, name)
				}
				sort.Strings(names)
				for _, name := range names {
					_, _ = fmt.Fprintf(writer, "%s=%s\n", name, p.Environment[name])
				}
				return nil
			}

			if cmd.Bool("variables") {
				// Load the raw model without interpolation to extract variables
				model, err := project.LoadModel(ctx, loadOpts...)
				if err != nil {
					return fmt.Errorf("failed to load model for variables: %w", err)
				}

				variables := template.ExtractVariables(model, template.DefaultPattern)

				// Print header
				_, _ = fmt.Fprintf(writer, "%-23s %-19s %-19s %s\n", "NAME", "REQUIRED", "DEFAULT VALUE", "ALTERNATE VALUE")

				// Collect and sort variable names
				names := make([]string, 0, len(variables))
				for name := range variables {
					names = append(names, name)
				}
				sort.Strings(names)

				for _, name := range names {
					v := variables[name]
					required := "false"
					if v.Required {
						required = "true"
					}
					_, _ = fmt.Fprintf(writer, "%-23s %-19s %-19s %s\n", name, required, v.DefaultValue, v.PresenceValue)
				}
				return nil
			}

			// Filter project by specific services if requested
			p.Services = services

			// Output full config in requested format
			switch cmd.String("format") {
			case "json":
				// Render via YAML: compose-go tags every Extensions field
				// json:"-", so marshaling the project directly drops all
				// x-incus/x-incus-compose blocks from the JSON output.
				yamlBytes, err := yaml.Marshal(p)
				if err != nil {
					return err
				}
				var tree map[string]any
				err = yaml.Unmarshal(yamlBytes, &tree)
				if err != nil {
					return err
				}

				// Use a buffer to capture JSON output and remove trailing newline
				var buf bytes.Buffer
				encoder := json.NewEncoder(&buf)
				encoder.SetIndent("", "  ")
				err = encoder.Encode(tree)
				if err != nil {
					return err
				}

				// Remove trailing newline to match docker-compose behavior
				jsonBytes := buf.Bytes()
				if len(jsonBytes) > 0 && jsonBytes[len(jsonBytes)-1] == '\n' {
					jsonBytes = jsonBytes[:len(jsonBytes)-1]
				}

				_, err = writer.Write(jsonBytes)
				return err
			case "yaml":
				// Use a buffer to capture YAML output and remove trailing newline
				var buf bytes.Buffer
				encoder := yaml.NewEncoder(&buf)
				encoder.SetIndent(2)
				err := encoder.Encode(p)
				if err := encoder.Close(); err != nil {
					return err
				}
				if err != nil {
					return err
				}

				_, err = writer.Write(buf.Bytes())
				return err
			default:
				return fmt.Errorf("unsupported format: %s", cmd.String("format"))
			}
		},
	}
}
