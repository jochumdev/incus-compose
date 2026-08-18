package main

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"text/tabwriter"

	"github.com/urfave/cli/v3"
	"go.yaml.in/yaml/v4"
	"golang.org/x/text/cases"
	"golang.org/x/text/language"

	"github.com/lxc/incus-compose/client"
)

// ProjectStatus holds the status of the project for ps output.
type ProjectStatus struct {
	Kind        string   `json:"kind" yaml:"kind"`
	Name        string   `json:"name" yaml:"name"`
	Description string   `json:"description" yaml:"description"`
	IncusName   string   `json:"incus_name" yaml:"incus_name"`
	Image       string   `json:"image" yaml:"image"`
	Status      string   `json:"status" yaml:"status"`
	Health      string   `json:"health" yaml:"health"`
	Addresses   []string `json:"addresses" yaml:"addresses"`
}

// ContainerStatuses collects and formats instance statuses.
type ContainerStatuses struct {
	Writer io.Writer

	Statuses []ProjectStatus
}

// NewContainerStatuses creates a new ContainerStatuses collector.
func NewContainerStatuses(w io.Writer) *ContainerStatuses {
	return &ContainerStatuses{Writer: w, Statuses: []ProjectStatus{}}
}

// Add appends a status to the collection.
func (c *ContainerStatuses) Add(status ProjectStatus) {
	c.Statuses = append(c.Statuses, status)
}

// sortResources orders resources by priority, then Incus name.
func sortResources(resources []client.Resource) []client.Resource {
	sorted := slices.Clone(resources)
	slices.SortFunc(sorted, func(a, b client.Resource) int {
		if c := cmp.Compare(a.Priority(), b.Priority()); c != 0 {
			return c
		}
		return strings.Compare(a.IncusName(), b.IncusName())
	})

	return sorted
}

// Yaml outputs statuses as YAML.
func (c *ContainerStatuses) Yaml() error {
	return yaml.NewEncoder(c.Writer).Encode(c.Statuses)
}

// JSON outputs statuses as indented JSON.
func (c *ContainerStatuses) JSON() error {
	enc := json.NewEncoder(c.Writer)
	enc.SetIndent("", "  ")
	return enc.Encode(c.Statuses)
}

// Table outputs statuses as a formatted table.
func (c *ContainerStatuses) Table() error {
	tw := tabwriter.NewWriter(c.Writer, 0, 0, 2, ' ', 0)
	_, err := fmt.Fprintln(tw, "KIND\tNAME\tINCUSNAME\tIMAGE\tSTATUS\tHEALTH\tADDRESSES")
	if err != nil {
		return err
	}

	var errs error
	for _, s := range c.Statuses {
		addrs := ""
		if len(s.Addresses) > 0 {
			addrs = s.Addresses[0]
			for _, a := range s.Addresses[1:] {
				addrs += ", " + a
			}
		}
		_, err := fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			s.Kind,
			s.Name,
			s.IncusName,
			s.Image,
			s.Status,
			s.Health,
			addrs,
		)

		errs = errors.Join(errs, err)
	}

	return errors.Join(errs, tw.Flush())
}

func newListCommand() *cli.Command {
	return &cli.Command{
		Name:      "list",
		Usage:     "List resources",
		Category:  "extensions",
		ArgsUsage: "[SERVICE...]",
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
				Sources: cli.EnvVars("INCUS_COMPOSE_LIST_FORMAT"),
			},
			&cli.BoolFlag{
				Name:    "no-healthd",
				Usage:   "List the healthd sidecar",
				Sources: cli.EnvVars("INCUS_COMPOSE_NO_HEALTHD"),
			},
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

			resources, err := p.Resources(c)
			if err != nil {
				c.LogError("Getting project resources in reCreate", "error", err)
				return errLogged.Wrap(err)
			}

			args := filterResourcesArgs{
				OnlyServices:     cmd.Args().Slice(),
				WithDependencies: cmd.Bool("with-deps"),
			}
			myResources := filterResources(p, resources, args)

			stack := client.NewStack(c, client.StackWorkers(cmd.Root().Int("workers")))

			order, err := p.ServiceOrder(false)
			if err != nil {
				c.LogError("Getting the service dependency order", "error", err)
				return errLogged.Wrap(err)
			}
			stack.AddOrdered(order, myResources)

			if !cmd.Bool("no-healthd") {
				// The shared daemon lives outside this project.
				hc, scope, err := healthdClient(p, c)

				switch {
				// No project to hold the shared daemon means no daemon.
				case errors.Is(err, client.ErrNotFound):
					c.LogWarn("Couldn't find healthd")
				case err != nil:
					c.LogError("Resolving the healthd scope", "error", err)
					return errLogged.Wrap(err)
				default:
					name, err := hc.FindHealthd()
					if err != nil {
						c.LogWarn("Couldn't find healthd")
						break
					}

					c.LogDebug("Found healthd name", "name", name, "scope", scope, "project", hc.IncusProject())

					inst, err := hc.Resource(client.KindInstance, name, &client.InstanceConfig{})
					if err != nil {
						c.LogWarn(err.Error())
						return errLogged.Wrap(err)
					}

					stack.Add(inst)
				}
			}

			err = stack.Run(ctx, client.ActionEnsure)
			if err != nil {
				c.LogWarn(err.Error())
			}

			var rErr error

			w := cmd.Root().Writer
			if cmd.String("output") != "" {
				fd, err := os.OpenFile(cmd.String("output"), os.O_APPEND|os.O_CREATE, 0o600)
				if err != nil {
					c.LogError(err.Error())
					return errLogged.Wrap(err)
				}
				defer c.WarnError(fd.Close, "Failure during close")
				w = fd
			}

			err = stack.ForAction(client.ActionEnsure).Run(ctx, client.ActionEnsure)
			if err != nil {
				c.LogWarn("Ensuring the stack", "error", "err")
			}

			titleCaser := cases.Title(language.English)

			statuses := NewContainerStatuses(w)

			for _, r := range sortResources(stack.All()) {
				if r == nil {
					c.LogDebug("Found a nil resource")
					continue
				}

				s := "Unknown"
				if r.IsEnsured() {
					s = "Exists"
				}

				// Images have 3-stage status: Unknown -> Cached -> Exists
				if r.Kind() == client.KindImage {
					if img, ok := r.(*client.Image); ok {
						s = img.Status()
					}
				}

				status := ProjectStatus{
					Kind:        string(r.Kind()),
					Name:        r.Name(),
					Description: "",
					IncusName:   r.IncusName(),
					Image:       "",
					Status:      s,
					Health:      "",
					Addresses:   []string{},
				}

				if r.Kind() == client.KindImage {
					continue
				}

				if r.Kind() == client.KindInstance {
					if !r.IsEnsured() {
						c.LogDebug("Getting an instance", "error", client.NewError("not ensured").WithResource(r))
						statuses.Add(status)
						continue
					}

					instance, ok := r.(*client.Instance)
					if !ok {
						err = client.ErrUnknown.WithResource(r)
						c.LogError("Getting an instance", err)
						rErr = errors.Join(rErr, err)
						statuses.Add(status)
						continue
					}

					if !instance.HasState() {
						c.LogWarn("Skipping", "error", client.NewError("not all instance details provided").WithResource(r))
						statuses.Add(status)
						continue
					}

					base := instance.State().IncusInstance
					state := instance.State().IncusInstanceState

					status.Name = instance.ServiceName()
					status.Status = state.Status
					status.Image = instance.Config.Image
					status.Description = base.Description

					// Get IP addresses
					for _, network := range state.Network {
						for _, addr := range network.Addresses {
							if addr.Family == "inet" && addr.Scope == "global" {
								status.Addresses = append(status.Addresses, addr.Address)
							}
						}
					}

					// Use the healthcheck status if available.
					if val, ok := base.Config[client.HealthStatusKey]; ok {
						status.Health = titleCaser.String(val)
					}
				}

				statuses.Add(status)
			}

			if rErr != nil {
				return rErr
			}

			switch cmd.String("format") {
			case "table":
				err := statuses.Table()
				if err != nil {
					c.LogError(err.Error())
					return errLogged.Wrap(err)
				}
			case "yaml":
				err := statuses.Yaml()
				if err != nil {
					c.LogError(err.Error())
					return errLogged.Wrap(err)
				}
			case "json":
				err := statuses.JSON()
				if err != nil {
					c.LogError(err.Error())
					return errLogged.Wrap(err)
				}
			default:
				// This should never happen.
				c.LogError("Unknown format", "format", cmd.String("format"))
				return errLogged.Wrap(err)
			}

			return nil
		},
	}
}
