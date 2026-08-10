package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/bradleyjkemp/cupaloy/v2"
	"github.com/stretchr/testify/require"

	"github.com/lxc/incus-compose/client"
	"github.com/lxc/incus-compose/project"
	"github.com/lxc/incus-compose/shared"
)

var snapshotter = cupaloy.New(cupaloy.SnapshotSubdirectory(filepath.Join("..", "..", "test", "snapshots", "e2e")))

func skipLocal(t *testing.T) {
	_, ok := os.LookupEnv("INCUS_COMPOSE_TEST_LOCAL")
	if ok {
		t.Skip("Skipping: env INCUS_COMPOSE_TEST_LOCAL is set, run `just test` for this test")
	}
}

func skipE2E(t *testing.T) {
	_, ok := os.LookupEnv("INCUS_COMPOSE_TEST_E2E")
	if !ok {
		t.Skip("Skipping: env INCUS_COMPOSE_TEST_E2E is not set, run `just test-e2e` for this test")
	}
}

func skipNo73(t *testing.T, c *client.Client) {
	if !c.Global().HasExtension(shared.Incus73Extension) {
		t.Skip("nat tests with static ip require at least incus 7.3 or 7.0.2 LTS")
	}
}

func skipNotSameHost(t *testing.T, gc *client.GlobalClient) {
	if gc.SameHost() != nil {
		t.Skip("not on the same host")
	}
}

func runCommand(ctx context.Context, t *testing.T, projectName string, args ...string) (*bytes.Buffer, error) {
	t.Helper()

	projectName = strings.ToLower(strings.ReplaceAll(projectName, "/", "-"))

	mArgs := []string{"incus-compose", "--debug", "--project-name", projectName}
	mArgs = append(mArgs, args...)
	t.Log("Running", mArgs)

	stdout := &bytes.Buffer{}
	cmd := newRootCommand()
	cmd.Writer = stdout
	cmd.ErrWriter = t.Output()
	err := cmd.Run(ctx, mArgs)

	return stdout, err
}

// runCommandSnapshot runs args and snapshots its own stdout. Use this when the
// command itself is the interesting output (ps, exec, list, ...).
func runCommandSnapshot(ctx context.Context, t *testing.T, projectName string, strip bool, args ...string) {
	t.Helper()

	stdout, err := runCommand(ctx, t, projectName, args...)
	require.NoError(t, err)
	snapshotter.SnapshotT(t, stripOutput(t, stdout, strip))
}

// runCommandSnapshotList runs args (a mutating command with no interesting
// stdout of its own, e.g. up/down/start/stop/restart) and snapshots the
// project's `list` state afterward instead, forwarding args' -f/--file flag.
func runCommandSnapshotList(ctx context.Context, t *testing.T, projectName string, strip bool, args ...string) {
	t.Helper()

	_, err := runCommand(ctx, t, projectName, args...)
	require.NoError(t, err)

	// This makes sure that health status settles and makes tests less flaky.
	time.Sleep(500 * time.Millisecond)

	projectName = strings.ToLower(strings.ReplaceAll(projectName, "/", "-"))
	listArgs := []string{"incus-compose", "--debug", "--project-name", projectName}
	for i, a := range args {
		if (a == "-f" || a == "--file") && i+1 < len(args) {
			listArgs = append(listArgs, a, args[i+1])
		}
	}

	listArgs = append(listArgs, "list", "--format=json")

	t.Log("Running", listArgs)

	stdout := &bytes.Buffer{}
	cmd := newRootCommand()
	cmd.Writer = stdout
	cmd.ErrWriter = t.Output()
	err = cmd.Run(ctx, listArgs)

	require.NoError(t, err)
	snapshotter.SnapshotT(t, stripOutput(t, stdout, strip))
}

// stripOutput removes dynamic content (IP addresses, network hashes) for snapshot comparison.
func stripOutput(t *testing.T, output *bytes.Buffer, stripHealth bool) string {
	t.Helper()

	ipv4Regex := regexp.MustCompile(`\d+\.\d+\.\d+\.\d+`)
	ipv6Regex := regexp.MustCompile(`(?:[0-9a-fA-F]{0,4}:){2,7}[0-9a-fA-F]{0,4}`)
	healthdImageRegex := regexp.MustCompile("ic-healthd:[0-9a-z.-]+")
	outStr := ipv4Regex.ReplaceAllString(output.String(), "-stripped-")
	outStr = ipv6Regex.ReplaceAllString(outStr, "-stripped-")
	outStr = healthdImageRegex.ReplaceAllString(outStr, "ic-healthd:-stripped-")

	if stripHealth {
		healthRegex := regexp.MustCompile(`"health": "[a-zA-Z]+",`)
		outStr = healthRegex.ReplaceAllString(outStr, `"health": "-stripped-",`)
	}

	// Cupaloy adds a newline, 2 lines are bad for my editors format on save.
	return strings.Trim(outStr, "\n")
}

func plannedNetworkNames(ctx context.Context, t *testing.T, projectName, compose string) []string {
	t.Helper()

	projectName = strings.ToLower(strings.ReplaceAll(projectName, "/", "-"))

	p, err := project.New().Load(ctx, project.LoadFiles([]string{compose}))
	require.NoError(t, err)

	c := client.NewOfflineClient(ctx, projectName)
	allResources, err := p.Resources(c)
	require.NoError(t, err)

	names := []string{}
	for _, res := range allResources {
		for _, r := range res {
			if r.Kind() == client.KindNetwork {
				names = append(names, r.IncusName())
			}
		}
	}
	return names
}

func projectClient(ctx context.Context, t *testing.T, projectName string, opts ...client.EnsureProjectOption) *client.Client {
	t.Helper()

	gc, err := client.NewTestClient(ctx)
	require.NoError(t, err)

	err = gc.Connect()
	require.NoError(t, err)

	c, err := gc.EnsureProject(projectName, opts...)
	require.NoError(t, err)

	return c
}

type e2eTest struct {
	name    string
	args    []string
	wantErr bool
	// snapshot snapshots args' own stdout (ps, exec, list, ...).
	snapshot bool
	// snapshotList runs args (a mutating command with no interesting stdout
	// of its own) then snapshots the resulting `list` state instead.
	snapshotList    bool
	snapStripHealth bool
}

func runE2ETests(ctx context.Context, t *testing.T, projectName string, tests []e2eTest) {
	t.Helper()

	_, update := os.LookupEnv("UPDATE_SNAPSHOTS")
	prevFailed := false
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if prevFailed && !update {
				t.Skip("Previous failed")
			}

			switch {
			case tt.snapshotList:
				// This ugly sleep lets incus settle before we ask for "list".
				time.Sleep(time.Second)
				runCommandSnapshotList(ctx, t, projectName, tt.snapStripHealth, tt.args...)
			case tt.snapshot:
				runCommandSnapshot(ctx, t, projectName, tt.snapStripHealth, tt.args...)
			default:
				_, err := runCommand(ctx, t, projectName, tt.args...)
				if !tt.wantErr {
					if err != nil {
						prevFailed = true
					}
					require.NoError(t, err)
				} else {
					if err == nil {
						prevFailed = true
					}
					require.Error(t, err)
				}
			}
		})
	}
}

func TestConfigCommand(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		args     []string
		fixtures []string
		wantErr  bool
	}{
		{
			name:     "simple yaml",
			args:     []string{"config"},
			fixtures: []string{"../../test/fixtures/simple/compose.yaml"},
		},
		{
			name:     "simple json",
			args:     []string{"config", "--format", "json"},
			fixtures: []string{"../../test/fixtures/simple/compose.yaml"},
		},
		{
			name:     "two-services yaml",
			args:     []string{"config"},
			fixtures: []string{"../../test/fixtures/two-services/compose.yaml"},
		},
		{
			name:     "wordpress",
			args:     []string{"config"},
			fixtures: []string{"../../test/fixtures/wordpress/compose.yaml"},
		},
		{
			name:     "with-secrets",
			args:     []string{"config"},
			fixtures: []string{"../../test/fixtures/with-secrets/compose.yaml"},
		},
		{
			name:     "with-configs",
			args:     []string{"config"},
			fixtures: []string{"../../test/fixtures/with-configs/compose.yaml"},
		},
		{
			name:     "with-restart",
			args:     []string{"config"},
			fixtures: []string{"../../test/fixtures/with-restart/compose.yaml"},
		},
		{
			name:     "with-incus-options",
			args:     []string{"config"},
			fixtures: []string{"../../test/fixtures/with-incus-options/compose.yaml"},
		},
		{
			name:     "with-project-options",
			args:     []string{"config"},
			fixtures: []string{"../../test/fixtures/with-project-options/compose.yaml"},
		},
		{
			name:     "with-build",
			args:     []string{"config"},
			fixtures: []string{"../../test/fixtures/with-build/compose.yaml"},
		},
		{
			name:     "with-multi-files",
			args:     []string{"config"},
			fixtures: []string{"../../test/fixtures/with-multi-files/a.yaml", "../../test/fixtures/with-multi-files/b.yaml"},
		},
		{
			name: "project-directory simple",
			args: []string{"--project-directory", "../../test/fixtures/simple", "config"},
		},
		{
			name: "project-directory docker-compose with incus overlay",
			args: []string{"-P", "../../test/fixtures/with-docker-compose", "config"},
		},
		{
			name: "file docker-compose with incus overlay",
			args: []string{"-P", "../../test/fixtures/with-docker-compose", "config"},
		},
		{
			name:     "nonexistent file",
			args:     []string{"config"},
			fixtures: []string{"../../test/fixtures/i-dont-exists/compose.yaml"},
			wantErr:  true,
		},
		{
			name:     "invalid yaml",
			args:     []string{"config"},
			fixtures: []string{"../../test/fixtures/invalid/compose.yaml"},
			wantErr:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			args := []string{}
			for _, f := range tt.fixtures {
				args = append(args, "-f", f)
			}
			args = append(args, tt.args...)

			stdout, err := runCommand(t.Context(), t, "test-local-config", args...)

			if tt.wantErr {
				require.Error(t, err, "Stdout: %s", stdout.String())
			} else {
				require.NoError(t, err)

				output := ""
				if len(tt.fixtures) > 0 {
					absFixturePath, _ := filepath.Abs(filepath.Dir(tt.fixtures[0]))
					output = strings.ReplaceAll(stdout.String(), absFixturePath, "$FIXTURE_PATH")
				} else {
					output = stdout.String()
				}
				snapshotter.SnapshotT(t, strings.Trim(output, "\n"))
			}
		})
	}
}

func TestConfigFilterByService(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		args    []string
		fixture string
	}{
		{
			name:    "wordpress filter db service",
			args:    []string{"-f", "../../test/fixtures/wordpress/compose.yaml", "config", "db"},
			fixture: "../../test/fixtures/wordpress",
		},
		{
			name:    "wordpress filter wordpress service includes db dependency",
			args:    []string{"-f", "../../test/fixtures/wordpress/compose.yaml", "config", "wordpress"},
			fixture: "../../test/fixtures/wordpress",
		},
		{
			name:    "config --services list",
			args:    []string{"-f", "../../test/fixtures/wordpress/compose.yaml", "config", "--services"},
			fixture: "../../test/fixtures/wordpress",
		},
		{
			name:    "config --volumes list",
			args:    []string{"-f", "../../test/fixtures/wordpress/compose.yaml", "config", "--volumes"},
			fixture: "../../test/fixtures/wordpress",
		},
		{
			name:    "config --quiet validation",
			args:    []string{"-f", "../../test/fixtures/wordpress/compose.yaml", "config", "--quiet"},
			fixture: "../../test/fixtures/wordpress",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			stdout, err := runCommand(t.Context(), t, "test-local-config-filter", tt.args...)
			require.NoError(t, err)

			if tt.fixture != "" {
				absFixturePath, _ := filepath.Abs(tt.fixture)
				output := strings.ReplaceAll(stdout.String(), absFixturePath, "$FIXTURE_PATH")
				snapshotter.SnapshotT(t, strings.Trim(output, "\n"))
			}
		})
	}
}

func TestUpDownUpSimple(t *testing.T) {
	skipLocal(t)
	t.Parallel()

	ctx := t.Context()
	pn := t.Name()
	compose := "../../test/fixtures/simple/compose.yaml"

	t.Cleanup(func() {
		_, _ = runCommand(context.Background(), t, pn, "-f", compose, "down", "--project")
	})

	tests := []e2eTest{
		{
			name:            "up simple",
			args:            []string{"-f", compose, "up", "--detach"},
			snapshotList:    true,
			snapStripHealth: false,
		},
		{
			name:            "down simple",
			args:            []string{"-f", compose, "down"},
			snapshotList:    true,
			snapStripHealth: true,
		},
		{
			name:            "up simple",
			args:            []string{"-f", compose, "up", "--detach"},
			snapshotList:    true,
			snapStripHealth: false,
		},
	}

	runE2ETests(ctx, t, pn, tests)
}

func TestNormalLifecycle(t *testing.T) {
	skipLocal(t)
	t.Parallel()

	ctx := t.Context()
	pn := t.Name()
	compose := "../../test/fixtures/two-services/compose.yaml"

	t.Cleanup(func() {
		_, _ = runCommand(context.Background(), t, pn, "-f", compose, "down", "--project")
	})

	tests := []e2eTest{
		{
			name:            "up",
			args:            []string{"-f", compose, "up", "--detach"},
			snapshotList:    true,
			snapStripHealth: false,
		},
	}

	runE2ETests(ctx, t, pn, tests)
}

// dnsServiceIPs aggregates the dnsmasq address records for a service across the
// project's managed networks.
func dnsServiceIPs(t *testing.T, c *client.Client, networks []string, service string) []string {
	t.Helper()

	conn, err := c.Connection()
	require.NoError(t, err)

	var ips []string
	for _, name := range networks {
		net, _, err := conn.GetNetwork(t.Context(), name)
		require.NoError(t, err, "for network %q", name)
		netIps, _, _ := client.DNSmasqParse(net.Config["raw.dnsmasq"])
		ips = append(ips, netIps[service]...)
	}
	return ips
}

// TestUpDownscaleRemovesInstancesAndDNS deploys the replicas=3 baseline, then
// downscales to a single instance with --scale and verifies both the surplus
// instances and their DNS records are removed while the survivor keeps resolving.
func TestUpDownscaleRemovesInstancesAndDNS(t *testing.T) {
	skipLocal(t)
	t.Parallel()

	ctx := t.Context()
	pn := t.Name()
	compose := "../../test/fixtures/downscale/compose.yaml"

	networks := plannedNetworkNames(ctx, t, pn, compose)
	require.NotEmpty(t, networks)

	t.Cleanup(func() {
		_, _ = runCommand(context.Background(), t, pn, "-f", compose, "down", "--project")
	})

	_, err := runCommand(ctx, t, pn, "-f", compose, "up", "--detach")
	require.NoError(t, err)

	c := projectClient(ctx, t, pn)
	for _, name := range []string{"web-1", "web-2", "web-3"} {
		ok, err := c.InstanceExists(name)
		require.NoError(t, err)
		require.True(t, ok, "instance %q should exist after up", name)
	}

	before := dnsServiceIPs(t, c, networks, "web")
	require.NotEmpty(t, before, "web should have DNS records for 3 replicas")

	_, err = runCommand(ctx, t, pn, "-f", compose, "up", "--detach", "--scale=web=1")
	require.NoError(t, err)

	survivor, err := c.InstanceExists("web-1")
	require.NoError(t, err)
	require.True(t, survivor, "web-1 should remain after downscale")

	for _, name := range []string{"web-2", "web-3"} {
		ok, err := c.InstanceExists(name)
		require.NoError(t, err)
		require.False(t, ok, "instance %q should be removed after downscale", name)
	}

	after := dnsServiceIPs(t, c, networks, "web")
	require.NotEmpty(t, after, "web-1 should still resolve after downscale")
	require.Less(t, len(after), len(before), "DNS must shed records for removed instances")
}

// TestDNSCnameAliasAcrossProjects brings up the dns and dns2 fixtures together.
// dns2's default network is external and points at dns's network via
// x-incus-compose.network: dns-default, so both projects register their
// service-network aliases (network.CNames) on the very same Incus network.
// Snapshotting dns's network raw.dnsmasq confirms both projects' cnames
// coexist without clobbering each other.
func TestDNSCnameAliasAcrossProjects(t *testing.T) {
	skipLocal(t)
	t.Parallel()

	ctx := t.Context()
	composeDNS := "../../test/fixtures/dns/compose.yaml"
	composeDNS2 := "../../test/fixtures/dns2/compose.yaml"

	// dns2's compose.yaml hardcodes x-incus-compose.network: dns-default, so
	// the dns project must be named literally "dns" for the names to line up.
	// Cleanups run LIFO, so dns2 (registered second) is torn down before dns.
	t.Cleanup(func() {
		_, _ = runCommand(context.Background(), t, "dns", "-f", composeDNS, "down", "--project")
	})
	t.Cleanup(func() {
		_, _ = runCommand(context.Background(), t, "dns2", "-f", composeDNS2, "down", "--project")
	})

	_, err := runCommand(ctx, t, "dns", "-f", composeDNS, "up", "--detach")
	require.NoError(t, err)

	_, err = runCommand(ctx, t, "dns2", "-f", composeDNS2, "up", "--detach")
	require.NoError(t, err)

	// Matches dns2's hardcoded x-incus-compose.network: dns-default.
	const networkName = "dns-default"

	c := projectClient(ctx, t, "dns")
	conn, err := c.Connection()
	require.NoError(t, err)

	net, _, err := conn.GetNetwork(ctx, networkName)
	require.NoError(t, err)

	ipv4Regex := regexp.MustCompile(`\d+\.\d+\.\d+\.\d+`)
	ipv6Regex := regexp.MustCompile(`(?:[0-9a-fA-F]{0,4}:){2,7}[0-9a-fA-F]{0,4}`)

	lines := strings.Split(net.Config["raw.dnsmasq"], "\n")
	kept := make([]string, 0, len(lines))
	for _, line := range lines {
		if ipv6Regex.MatchString(line) {
			continue
		}
		kept = append(kept, line)
	}

	outStr := ipv4Regex.ReplaceAllString(strings.Join(kept, "\n"), "-stripped-")

	snapshotter.SnapshotT(t, outStr)
}
