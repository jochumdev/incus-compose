package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/urfave/cli/v3"

	"github.com/lxc/incus-compose/client"
	"github.com/lxc/incus-compose/project"
	"github.com/lxc/incus-compose/shared"
)

// errNoDNS is what a dns sub-command reports when there is no daemon to act on.
var errNoDNS = errors.New(
	"no ic-dns is running; bring one up with `incus-compose dns up`")

// dnsTarget is the daemon a dns sub-command acts on.
type dnsTarget struct {
	project  *project.Project
	client   *client.Client
	instance *client.Instance
	scope    string
}

// dnsProject loads the compose project a dns command was run in.
func dnsProject(ctx context.Context, cmd *cli.Command) (*project.Project, error) {
	p, err := project.New().Load(ctx, buildLoadOptions(cmd)...)
	if errors.Is(err, project.ErrNoComposeFile) {
		return nil, nil
	}

	return p, err
}

// matchDNSScope reports whether daemonScope matches the target project.
func matchDNSScope(daemonScope, daemonProject, targetScope, targetProject string) bool {
	if daemonScope == "" || targetScope == "" {
		return false
	}

	if daemonScope == shared.DNSScopeProject {
		return targetScope == shared.DNSScopeProject && daemonProject == targetProject
	}

	if daemonScope == targetScope {
		return true
	}

	for _, s := range strings.Split(daemonScope, ",") {
		if strings.TrimSpace(s) == targetScope {
			return true
		}
	}

	return false
}

// findDNS returns the name and project of the matching ic-dns daemon.
func findDNS(ctx context.Context, c *client.Client) (string, string, error) {
	conn, err := c.Connection()
	if err != nil {
		return "", "", err
	}

	targetConfig, err := c.Global().ProjectConfig(c.IncusProject())
	if err != nil {
		return "", "", err
	}

	targetScope := targetConfig[shared.DNSScopeKey]
	if targetScope == "" {
		targetScope = shared.DNSScopeGlobal
	}

	// 1. Check current project.
	instances, err := conn.GetInstances(ctx, c.IncusProject(), nil)
	if err != nil {
		return "", "", fmt.Errorf("listing instances: %w", err)
	}

	for _, inst := range instances {
		scope := inst.Config[shared.DNSDaemonScopeKey]
		if scope == "" {
			scope = inst.Config[shared.DNSScopeKey]
		}

		if matchDNSScope(scope, c.IncusProject(), targetScope, c.IncusProject()) {
			return inst.Name, c.IncusProject(), nil
		}
	}

	// 2. Check systemProject if different from current project.
	sysProj := systemProject
	if sysProj != c.IncusProject() {
		sysInstances, err := conn.GetInstances(ctx, sysProj, nil)
		if err == nil {
			for _, inst := range sysInstances {
				scope := inst.Config[shared.DNSDaemonScopeKey]
				if scope == "" {
					scope = inst.Config[shared.DNSScopeKey]
				}

				if matchDNSScope(scope, sysProj, targetScope, c.IncusProject()) {
					return inst.Name, sysProj, nil
				}
			}
		}
	}

	// 3. Check across all visible projects.
	allInstances, err := conn.GetInstancesAllProjects(ctx, nil)
	if err == nil {
		for _, inst := range allInstances {
			if inst.Project == c.IncusProject() || inst.Project == sysProj {
				continue
			}

			scope := inst.Config[shared.DNSDaemonScopeKey]
			if scope == "" {
				scope = inst.Config[shared.DNSScopeKey]
			}

			if matchDNSScope(scope, inst.Project, targetScope, c.IncusProject()) {
				return inst.Name, inst.Project, nil
			}
		}
	}

	return "", "", errNoDNS
}

// resolveDNSTarget finds the daemon to act on and opens the client owning it.
func resolveDNSTarget(ctx context.Context, cmd *cli.Command, gc *client.GlobalClient) (*dnsTarget, func(), error) {
	p, err := dnsProject(ctx, cmd)
	if err != nil {
		return nil, nil, fmt.Errorf("configuring the project: %w", err)
	}

	target := &dnsTarget{project: p, scope: shared.DNSScopeGlobal}

	var searchClient *client.Client
	if p != nil {
		c, err := gc.EnsureProject(p.Name)
		if err != nil {
			return nil, nil, fmt.Errorf("getting the incus project: %w", err)
		}

		searchClient = c
	} else {
		c, err := gc.EnsureProject(systemProject)
		if errors.Is(err, client.ErrNotFound) {
			return nil, nil, errNoDNS
		}
		if err != nil {
			return nil, nil, fmt.Errorf("getting the %s project: %w", systemProject, err)
		}

		searchClient = c
	}

	err = searchClient.Open()
	if err != nil {
		return nil, nil, fmt.Errorf("opening the project client: %w", err)
	}

	name, proj, err := findDNS(ctx, searchClient)
	if err != nil {
		searchClient.WarnError(searchClient.Done, "Failure during Client.Done()")

		return nil, nil, errNoDNS
	}

	if proj == searchClient.IncusProject() {
		target.client = searchClient
	} else {
		searchClient.WarnError(searchClient.Done, "Failure during Client.Done()")

		target.client, err = gc.EnsureProject(proj)
		if err != nil {
			return nil, nil, fmt.Errorf("getting daemon project %s: %w", proj, err)
		}

		err = target.client.Open()
		if err != nil {
			return nil, nil, fmt.Errorf("opening daemon project client: %w", err)
		}
	}

	done := func() { target.client.WarnError(target.client.Done, "Failure during Client.Done()") }

	res, err := target.client.Resource(client.KindInstance, name, &client.InstanceConfig{})
	if err != nil {
		done()

		return nil, nil, err
	}

	inst, ok := res.(*client.Instance)
	if !ok {
		done()

		return nil, nil, errors.New("unexpected resource type for dns")
	}

	target.instance = inst

	return target, done, nil
}

// resolveDNSIP finds the matching ic-dns daemon and returns its IP address.
func resolveDNSIP(ctx context.Context, c *client.Client, timeout time.Duration) (string, error) {
	name, proj, err := findDNS(ctx, c)
	if err != nil {
		return "", err
	}

	targetClient := c
	if proj != c.IncusProject() {
		targetClient, err = c.Global().EnsureProject(proj)
		if err != nil {
			return "", fmt.Errorf("getting daemon project %s: %w", proj, err)
		}

		err = targetClient.Open()
		if err != nil {
			return "", fmt.Errorf("opening daemon project client: %w", err)
		}
		defer targetClient.WarnError(targetClient.Done, "Failure during Client.Done()")
	}

	res, err := targetClient.Resource(client.KindInstance, name, &client.InstanceConfig{})
	if err != nil {
		return "", err
	}

	inst, ok := res.(*client.Instance)
	if !ok {
		return "", errors.New("unexpected resource type for dns")
	}

	ips, err := inst.WaitIPs(ctx, timeout)
	if err != nil {
		return "", fmt.Errorf("waiting for dns daemon IP: %w", err)
	}

	for _, iface := range ips {
		if len(iface.IPv4s) > 0 {
			return iface.IPv4s[0], nil
		}
	}

	for _, iface := range ips {
		if len(iface.IPv6s) > 0 {
			return iface.IPv6s[0], nil
		}
	}

	return "", errors.New("dns daemon has no IP address")
}
