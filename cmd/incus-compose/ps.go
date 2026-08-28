package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"text/tabwriter"

	"github.com/lxc/incus/v7/shared/util"
	"github.com/urfave/cli/v3"

	"github.com/lxc/incus-compose/client"
	"github.com/lxc/incus-compose/iclient"
	"github.com/lxc/incus-compose/project"
)

// newPsCommand implements `incus-compose ps`
// Mirrors docker-compose ps semantics (instances-only, -a, -q, --services, format table/json).
func newPsCommand() *cli.Command {
	return &cli.Command{
		Name:      "ps",
		Usage:     "List containers (instances)",
		Category:  "compose",
		ArgsUsage: "[SERVICE...]",
		Flags: []cli.Flag{
			&cli.BoolFlag{
				Name:    "all",
				Aliases: []string{"a"},
				Usage:   "Show all containers (including stopped ones)",
				Sources: cli.EnvVars("INCUS_COMPOSE_PS_ALL"),
			},
			&cli.BoolFlag{
				Name:    "quiet",
				Aliases: []string{"q"},
				Usage:   "Only display Incus instance names",
				Sources: cli.EnvVars("INCUS_COMPOSE_PS_QUIET"),
			},
			&cli.BoolFlag{
				Name:    "services",
				Usage:   "Display services (compose service names) instead of instances",
				Sources: cli.EnvVars("INCUS_COMPOSE_PS_SERVICES"),
			},
			&cli.StringFlag{
				Name:  "format",
				Usage: "Format the output. Values: [table | json]",
				Value: "table",
				Action: func(ctx context.Context, cmd *cli.Command, v string) error {
					if !slices.Contains([]string{"table", "json"}, v) {
						return fmt.Errorf("invalid format: %s (must be table or json)", v)
					}
					return nil
				},
				Sources: cli.EnvVars("INCUS_COMPOSE_PS_FORMAT"),
			},
			&cli.BoolFlag{
				Name:    "with-deps",
				Usage:   "Also list linked services",
				Sources: cli.EnvVars("INCUS_COMPOSE_PS_WITH_DEPS"),
			},
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			p, c, err := loadProject(ctx, cmd)
			if err != nil {
				return err
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

			// Run ensure (without create) to populate resource metadata/state where possible.
			if err := stack.Run(ctx, client.ActionEnsure); err != nil {
				c.LogWarn("Ensuring the stack", "error", err)
			}

			// Collect instance statuses.
			type psEntry struct {
				Service   string   `json:"service,omitempty"`
				Name      string   `json:"name,omitempty"`       // compose resource name
				IncusName string   `json:"incus_name,omitempty"` // actual incus instance name
				Image     string   `json:"image,omitempty"`
				Status    string   `json:"status,omitempty"`
				Addresses []string `json:"addresses,omitempty"`
			}

			entries := []psEntry{}

			// Helper to add entry if it matches filters (-a and default-running)
			addIfMatches := func(e psEntry) {
				// By default omit non-running unless --all. A paused service
				// still counts, as it does in docker.
				if !cmd.Bool("all") && e.Status != "Running" && e.Status != "Frozen" {
					return
				}
				entries = append(entries, e)
			}

			seenServices := map[string]struct{}{}

			for _, r := range sortResources(stack.All()) {
				if r == nil {
					c.LogDebug("Found a nil resource")
					continue
				}

				if r.Kind() != client.KindInstance {
					// ps only lists instances
					continue
				}

				inst, ok := r.(*client.Instance)
				if !ok {
					continue
				}

				status := "Unknown"
				if r.IsEnsured() {
					status = "Exists"
				}

				// Default entry with minimal info. We'll try to fill from Instance resource if available.
				entry := psEntry{
					Service:   inst.ServiceName(),
					Name:      inst.Name(),
					IncusName: inst.IncusName(),
					Image:     "",
					Status:    status,
					Addresses: []string{},
				}

				// If resource is an Instance resource and has state, use it.
				if inst.IsEnsured() && inst.HasState() {
					base := inst.State().IncusInstance
					state := inst.State().IncusInstanceState
					if base == nil || state == nil {
						continue
					}

					if util.IsTrue(base.Config[client.HealthKeyPrefix+"daemon"]) {
						continue
					}

					entry.Status = state.Status
					entry.Image = inst.Config.Image

					// collect addresses
					for _, nw := range state.Network {
						for _, a := range nw.Addresses {
							if a.Family == "inet" && a.Scope == "global" {
								entry.Addresses = append(entry.Addresses, a.Address)
							}
						}
					}
				}

				addIfMatches(entry)
				if cmd.Bool("services") {
					seenServices[entry.Service] = struct{}{}
				}
			}

			// Include orphaned instances (instances present in the Incus project but not defined in compose).
			func() {
				incus, err := c.Connection()
				if err != nil {
					return
				}

				instances, err := incus.GetInstances(ctx, c.IncusProject(), &iclient.GetInstancesArgs{Full: true})
				if err != nil {
					// Non-fatal: if we cannot list instances, skip orphan inclusion.
					c.LogDebug("Listing instances for orphans failed", "error", err)
					return
				}

				type instMinimal struct {
					Name    string
					Status  string
					Service string
				}
				orphanMap := map[string]instMinimal{}

				for _, inst := range instances {
					name := inst.Name
					status := "Unknown"

					if util.IsTrue(inst.Config[client.HealthKeyPrefix+"daemon"]) {
						continue
					}

					if inst.State != nil && inst.State.Status != "" {
						status = inst.State.Status
					}

					// A one-off is in no resource map, but it is not an orphan
					// either: it says which service it was made from.
					service := "<orphan>"
					if util.IsTrue(inst.Config[project.OneOffKey]) {
						service = inst.Config[project.ServiceLabelKey]
					}

					orphanMap[name] = instMinimal{Name: name, Status: status, Service: service}
				}

				// Remove instances that are present in stack
				for _, r := range stack.All() {
					if r == nil {
						continue
					}
					if r.Kind() != client.KindInstance {
						continue
					}
					delete(orphanMap, r.IncusName())
				}

				// Add orphans to entries
				for _, o := range orphanMap {
					name := "<orphan>"
					if o.Service != "<orphan>" {
						name = o.Name
					}

					e := psEntry{
						Service:   o.Service,
						Name:      name,
						IncusName: o.Name,
						Image:     "",
						Status:    o.Status,
						Addresses: []string{},
					}
					if !cmd.Bool("services") {
						addIfMatches(e)
					}
				}
			}()

			// Orphans come from a map, sort for a stable output.
			slices.SortFunc(entries, func(a, b psEntry) int {
				return strings.Compare(a.IncusName, b.IncusName)
			})

			// Handle quiet and services flags
			w := cmd.Root().Writer
			if w == nil {
				w = os.Stdout
			}

			// If --services: print deduped service names (respecting -a filter)
			if cmd.Bool("services") {
				services := []string{}
				for s := range seenServices {
					services = append(services, s)
				}
				// Ensure stable order (sort by name)
				slices.Sort(services)
				for _, s := range services {
					_, _ = fmt.Fprintln(w, s)
				}
				return nil
			}

			// If quiet: print Incus instance names only
			if cmd.Bool("quiet") {
				for _, e := range entries {
					_, _ = fmt.Fprintln(w, e.IncusName)
				}
				return nil
			}

			// Output formatting
			switch cmd.String("format") {
			case "table":
				tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
				_, _ = fmt.Fprintln(tw, "NAME\tSERVICE\tINCUS_NAME\tIMAGE\tSTATUS\tADDRESSES")
				for _, e := range entries {
					addrs := ""
					if len(e.Addresses) > 0 {
						addrs = e.Addresses[0]
						for _, a := range e.Addresses[1:] {
							addrs += ", " + a
						}
					}
					_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n",
						e.Name,
						e.Service,
						e.IncusName,
						e.Image,
						e.Status,
						addrs,
					)
				}
				_ = tw.Flush()
				return nil
			case "json":
				enc := json.NewEncoder(w)
				enc.SetIndent("", "  ")
				return enc.Encode(entries)
			default:
				// should never happen due to flag validation
				return errLogged.Wrap(errors.New("invalid format"))
			}
		},
	}
}
