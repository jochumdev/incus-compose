package main

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"

	"github.com/urfave/cli/v3"

	"github.com/lxc/incus-compose/client"
	"github.com/lxc/incus-compose/shared"
)

// splitProxyEndpoint splits an Incus proxy endpoint ("tcp:0.0.0.0:8080") into
// its protocol and its address:port half.
func splitProxyEndpoint(endpoint string) (string, string, bool) {
	proto, addr, ok := strings.Cut(endpoint, ":")
	if !ok {
		return "", "", false
	}

	_, _, err := net.SplitHostPort(addr)
	if err != nil {
		return "", "", false
	}

	return proto, addr, true
}

// publishedPort returns the listen side of the proxy device forwarding
// protocol/port into the instance.
func publishedPort(devices map[string]map[string]string, protocol string, port string) (string, bool) {
	for _, name := range slices.Sorted(maps.Keys(devices)) {
		device := devices[name]
		if device["type"] != client.InstanceDeviceTypeProxy {
			continue
		}

		proto, connect, ok := splitProxyEndpoint(device["connect"])
		if !ok || proto != protocol {
			continue
		}

		_, connectPort, _ := net.SplitHostPort(connect)
		if connectPort != port {
			continue
		}

		_, listen, ok := splitProxyEndpoint(device["listen"])
		if ok {
			return listen, true
		}
	}

	return "", false
}

// proxyPorts lists the instance-side ports of every proxy device, as "80/tcp".
func proxyPorts(devices map[string]map[string]string) []string {
	ports := []string{}

	for _, name := range slices.Sorted(maps.Keys(devices)) {
		device := devices[name]
		if device["type"] != client.InstanceDeviceTypeProxy {
			continue
		}

		proto, connect, ok := splitProxyEndpoint(device["connect"])
		if !ok {
			continue
		}

		_, port, _ := net.SplitHostPort(connect)
		ports = append(ports, port+"/"+proto)
	}

	return ports
}

// newPortCommand implements `incus-compose port` similar to `docker compose port`.
func newPortCommand() *cli.Command {
	return &cli.Command{
		Name:      "port",
		Usage:     "Print the host binding of a published port",
		Category:  "compose",
		ArgsUsage: "SERVICE PRIVATE_PORT",
		Flags: []cli.Flag{
			&cli.IntFlag{
				Name:    "index",
				Usage:   "Index of the container if service has multiple replicas",
				Value:   0,
				Sources: cli.EnvVars("INCUS_COMPOSE_PORT_INDEX"),
			},
			&cli.StringFlag{
				Name:    "protocol",
				Usage:   "Protocol of the port, tcp or udp",
				Value:   "tcp",
				Sources: cli.EnvVars("INCUS_COMPOSE_PORT_PROTOCOL"),
			},
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			args := cmd.Args().Slice()
			if len(args) != 2 {
				return fmt.Errorf("usage: %s SERVICE PRIVATE_PORT", cmd.Name)
			}

			service, private := args[0], args[1]

			protocol := strings.ToLower(cmd.String("protocol"))
			if protocol != "tcp" && protocol != "udp" {
				return fmt.Errorf("unknown protocol %q, want tcp or udp", protocol)
			}

			_, err := strconv.ParseUint(private, 10, 16)
			if err != nil {
				return fmt.Errorf("bad port %q must be a number: %w", private, err)
			}

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

			inst, err := serviceInstance(ctx, c, p, service, cmd.Int("index"))
			if err != nil {
				return err
			}

			devices := inst.State().IncusInstance.Devices

			listen, ok := publishedPort(devices, protocol, private)
			if !ok {
				c.LogError("No published port", "service", service, "port", private+"/"+protocol,
					"published", strings.Join(proxyPorts(devices), ", "))
				return errLogged.Wrap(client.ErrNotFound.WithText("published port not found"))
			}

			_, err = fmt.Fprintln(cmd.Root().Writer, listen)
			return err
		},
	}
}

// newPortForwardCommand implements `incus-compose port-forward`, an
// incus-compose extension without a `docker compose` counterpart.
func newPortForwardCommand() *cli.Command {
	return &cli.Command{
		Name:      "port-forward",
		Usage:     "Forward a local TCP port into an instance",
		Category:  "extensions",
		ArgsUsage: "SERVICE TARGET_PORT [LISTEN_PORT]",
		Flags: []cli.Flag{
			&cli.IntFlag{
				Name:    "index",
				Usage:   "Index of the container if service has multiple replicas",
				Value:   0,
				Sources: cli.EnvVars("INCUS_COMPOSE_PORT_FORWARD_INDEX"),
			},
			&cli.BoolFlag{
				Name:  "dry-run",
				Usage: "Print the incus command instead of running it",
			},
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			args := cmd.Args().Slice()
			if len(args) < 2 || len(args) > 3 {
				return fmt.Errorf("usage: %s SERVICE TARGET_PORT [LISTEN_PORT]", cmd.Name)
			}

			service, target := args[0], args[1]

			listen := target
			if len(args) == 3 {
				listen = args[2]
			}

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

			if !c.Global().HasExtension(shared.Incus73Extension) {
				c.LogError("For port-forward you need at least incus 7.3 or 7.0.1 LTS")
				return errLogged.Wrap(errors.New("the server does not support port forwarding"))
			}

			inst, err := serviceInstance(ctx, c, p, service, cmd.Int("index"))
			if err != nil {
				return err
			}

			execPath, err := exec.LookPath("incus")
			if err != nil {
				c.LogError("`incus` not found in PATH")
				return errLogged.Wrap(errors.New("'incus' not found in PATH"))
			}

			iArgs := []string{"port-forward", inst.IncusName(), target, listen}

			if cmd.Bool("dry-run") {
				_, err = fmt.Fprintf(cmd.Root().Writer, "%s %s", execPath, strings.Join(iArgs, " "))
				return err
			}

			execCmd := exec.CommandContext(ctx, execPath, iArgs...) //nolint:gosec
			execCmd.Stdin = os.Stdin
			execCmd.Stdout = cmd.Root().Writer
			execCmd.Stderr = cmd.Root().ErrWriter
			execCmd.Env = append(os.Environ(), "INCUS_PROJECT="+c.IncusProject())

			err = execCmd.Run()
			if err != nil {
				return errLogged.Wrap(err)
			}

			return nil
		},
	}
}
