package main

import (
	"context"
	"errors"
	"fmt"
	"net"

	incusApi "github.com/lxc/incus/v7/shared/api"
	"github.com/urfave/cli/v3"

	"github.com/lxc/incus-compose/client"
	"github.com/lxc/incus-compose/iclient"
)

func newDNSStatusCommand() *cli.Command {
	return &cli.Command{
		Name:  "status",
		Usage: "Prints the status of ic-dns",
		Action: func(ctx context.Context, cmd *cli.Command) error {
			globalClient, err := clientFromContext(ctx)
			if err != nil {
				return err
			}
			err = globalClient.Connect()
			if err != nil {
				return err
			}

			target, done, err := resolveDNSTarget(ctx, cmd, globalClient)
			if err != nil {
				globalClient.LogError("Finding dns", "error", err)
				return errLogged.Wrap(err)
			}
			defer done()

			err = client.RunAction(ctx, target.instance, client.ActionEnsure)
			if err != nil {
				return fmt.Errorf("while fetching dns: %w", err)
			}

			state := target.instance.State()
			if state == nil {
				return errors.New("no dns state after fetch")
			}

			if state.IncusInstance.Status != "Running" {
				_, _ = fmt.Fprintln(cmd.Root().Writer, "stopped")
				return nil
			}

			conn, err := target.client.Connection()
			if err != nil {
				return err
			}

			httpPort := "9153"
			envHTTP := state.IncusInstance.Config[envDNSHTTP]
			if envHTTP == "" {
				envHTTP = state.IncusInstance.Config["environment.DNS_HTTP"]
			}
			if envHTTP != "" {
				_, p, err := net.SplitHostPort(envHTTP)
				if err == nil && p != "" {
					httpPort = p
				}
			}

			checkReady := func(port string) bool {
				url := fmt.Sprintf("http://127.0.0.1:%s/ready", port)
				updates, err := conn.ExecInstance(ctx, target.client.IncusProject(), target.instance.IncusName(), incusApi.InstanceExecPost{
					Command: []string{"wget", "--quiet", "--spider", url},
				}, nil)
				if err != nil {
					return false
				}

				op, err := iclient.WaitOperation(ctx, updates)
				if err != nil {
					return false
				}

				code, ok := op.Metadata["return"].(float64)
				if ok && int(code) == 0 {
					return true
				}

				return false
			}

			ready := checkReady(httpPort)
			if !ready && httpPort == "9153" {
				ready = checkReady("8080")
			}

			if ready {
				_, _ = fmt.Fprintln(cmd.Root().Writer, "ready")
			} else {
				_, _ = fmt.Fprintln(cmd.Root().Writer, "unready")
			}

			return nil
		},
	}
}
