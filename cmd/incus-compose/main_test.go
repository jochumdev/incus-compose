package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bradleyjkemp/cupaloy/v2"
	"github.com/stretchr/testify/require"

	"github.com/lxc/incus-compose/client"
	"github.com/lxc/incus-compose/internal/testlib"
	"github.com/lxc/incus-compose/project"
	"github.com/lxc/incus-compose/shared"
)

var snapshotter = cupaloy.New(cupaloy.SnapshotSubdirectory(filepath.Join("..", "..", "test", "snapshots", "e2e")))

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

func TestMain(m *testing.M) {
	os.Exit(testlib.Main(m))
}

// runCommandSnapshot runs args and snapshots its own stdout. Use this when the
// command itself is the interesting output (ps, exec, list, ...).
func runCommandSnapshot(ctx context.Context, t *testing.T, projectName string, strip bool, args ...string) {
	t.Helper()

	stdout, err := testlib.RunCompose(ctx, t, projectName, "", nil, args...)
	require.NoError(t, err)
	snapshotter.SnapshotT(t, stripOutput(t, stdout, strip))
}

// runCommandSnapshotList runs args (a mutating command with no interesting
// stdout of its own, e.g. up/down/start/stop/restart) and snapshots the
// project's `list` state afterward instead, forwarding args' -f/--file flag.
func runCommandSnapshotList(ctx context.Context, t *testing.T, projectName string, strip bool, args ...string) {
	t.Helper()

	_, err := testlib.RunCompose(ctx, t, projectName, "", nil, args...)
	require.NoError(t, err)

	// This makes sure that health status settles and makes tests less flaky.
	time.Sleep(500 * time.Millisecond)

	forwarded := []string{}
	for i, a := range args {
		if (a == "-f" || a == "--file") && i+1 < len(args) {
			forwarded = append(forwarded, a, args[i+1])
		}
	}

	forwarded = append(forwarded, "list", "--format=json")

	stdout, err := testlib.RunCompose(ctx, t, projectName, "", nil, forwarded...)
	require.NoError(t, err)
	snapshotter.SnapshotT(t, stripOutput(t, stdout, strip))
}

func stripOutput(t *testing.T, output string, stripHealth bool) string {
	t.Helper()

	if stripHealth {
		return testlib.StripHealth(output)
	}

	return testlib.Strip(output)
}

func plannedNetworkNames(ctx context.Context, t *testing.T, projectName, compose string) []string {
	t.Helper()

	p, err := project.New().Load(ctx, project.LoadFiles([]string{compose}))
	require.NoError(t, err)

	c := client.NewOfflineClient(ctx, testlib.ProjectName(projectName))
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
				_, err := testlib.RunCompose(ctx, t, projectName, "", nil, tt.args...)
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
			fixtures: []string{testlib.Fixture(t, "simple", "compose.yaml")},
		},
		{
			name:     "simple json",
			args:     []string{"config", "--format", "json"},
			fixtures: []string{testlib.Fixture(t, "simple", "compose.yaml")},
		},
		{
			name:     "two-services yaml",
			args:     []string{"config"},
			fixtures: []string{testlib.Fixture(t, "two-services", "compose.yaml")},
		},
		{
			name:     "wordpress",
			args:     []string{"config"},
			fixtures: []string{testlib.Fixture(t, "wordpress", "compose.yaml")},
		},
		{
			name:     "with-secrets",
			args:     []string{"config"},
			fixtures: []string{testlib.Fixture(t, "with-secrets", "compose.yaml")},
		},
		{
			name:     "with-configs",
			args:     []string{"config"},
			fixtures: []string{testlib.Fixture(t, "with-configs", "compose.yaml")},
		},
		{
			name:     "with-restart",
			args:     []string{"config"},
			fixtures: []string{testlib.Fixture(t, "with-restart", "compose.yaml")},
		},
		{
			name:     "with-incus-options",
			args:     []string{"config"},
			fixtures: []string{testlib.Fixture(t, "with-incus-options", "compose.yaml")},
		},
		{
			name:     "with-project-options",
			args:     []string{"config"},
			fixtures: []string{testlib.Fixture(t, "with-project-options", "compose.yaml")},
		},
		{
			name:     "with-build",
			args:     []string{"config"},
			fixtures: []string{testlib.Fixture(t, "with-build", "compose.yaml")},
		},
		{
			name:     "with-multi-files",
			args:     []string{"config"},
			fixtures: []string{testlib.Fixture(t, "with-multi-files", "a.yaml"), testlib.Fixture(t, "with-multi-files", "b.yaml")},
		},
		{
			name: "project-directory simple",
			args: []string{"--project-directory", testlib.Fixture(t, "simple"), "config"},
		},
		{
			name: "project-directory docker-compose with incus overlay",
			args: []string{"-P", testlib.Fixture(t, "with-docker-compose"), "config"},
		},
		{
			name: "file docker-compose with incus overlay",
			args: []string{"-P", testlib.Fixture(t, "with-docker-compose"), "config"},
		},
		{
			name:     "nonexistent file",
			args:     []string{"config"},
			fixtures: []string{testlib.Fixture(t, "i-dont-exists", "compose.yaml")},
			wantErr:  true,
		},
		{
			name:     "invalid yaml",
			args:     []string{"config"},
			fixtures: []string{testlib.Fixture(t, "invalid", "compose.yaml")},
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

			stdout, err := testlib.RunCompose(t.Context(), t, "test-local-config", "", nil, args...)

			if tt.wantErr {
				require.Error(t, err, "Stdout: %s", stdout)
			} else {
				require.NoError(t, err)

				output := stdout
				if len(tt.fixtures) > 0 {
					output = strings.ReplaceAll(stdout, filepath.Dir(tt.fixtures[0]), "$FIXTURE_PATH")
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
			args:    []string{"-f", testlib.Fixture(t, "wordpress", "compose.yaml"), "config", "db"},
			fixture: testlib.Fixture(t, "wordpress"),
		},
		{
			name:    "wordpress filter wordpress service includes db dependency",
			args:    []string{"-f", testlib.Fixture(t, "wordpress", "compose.yaml"), "config", "wordpress"},
			fixture: testlib.Fixture(t, "wordpress"),
		},
		{
			name:    "config --services list",
			args:    []string{"-f", testlib.Fixture(t, "wordpress", "compose.yaml"), "config", "--services"},
			fixture: testlib.Fixture(t, "wordpress"),
		},
		{
			name:    "config --volumes list",
			args:    []string{"-f", testlib.Fixture(t, "wordpress", "compose.yaml"), "config", "--volumes"},
			fixture: testlib.Fixture(t, "wordpress"),
		},
		{
			name:    "config --quiet validation",
			args:    []string{"-f", testlib.Fixture(t, "wordpress", "compose.yaml"), "config", "--quiet"},
			fixture: testlib.Fixture(t, "wordpress"),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			stdout, err := testlib.RunCompose(t.Context(), t, "test-local-config-filter", "", nil, tt.args...)
			require.NoError(t, err)

			if tt.fixture != "" {
				output := strings.ReplaceAll(stdout, tt.fixture, "$FIXTURE_PATH")
				snapshotter.SnapshotT(t, strings.Trim(output, "\n"))
			}
		})
	}
}

func TestUpDownUpSimple(t *testing.T) {
	testlib.SkipLocal(t)
	t.Parallel()

	ctx := t.Context()
	pn := t.Name()
	compose := testlib.Fixture(t, "simple", "compose.yaml")

	testlib.CleanupCompose(t, pn, "-f", compose, "down", "--project")

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
	testlib.SkipLocal(t)
	t.Parallel()

	ctx := t.Context()
	pn := t.Name()
	compose := testlib.Fixture(t, "two-services", "compose.yaml")

	testlib.CleanupCompose(t, pn, "-f", compose, "down", "--project")

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
	testlib.SkipLocal(t)
	t.Parallel()

	ctx := t.Context()
	pn := t.Name()
	compose := testlib.Fixture(t, "downscale", "compose.yaml")

	networks := plannedNetworkNames(ctx, t, pn, compose)
	require.NotEmpty(t, networks)

	testlib.CleanupCompose(t, pn, "-f", compose, "down", "--project")

	_, err := testlib.RunCompose(ctx, t, pn, "", nil, "-f", compose, "up", "--detach")
	require.NoError(t, err)

	c := projectClient(ctx, t, pn)
	for _, name := range []string{"web-1", "web-2", "web-3"} {
		ok, err := c.InstanceExists(name)
		require.NoError(t, err)
		require.True(t, ok, "instance %q should exist after up", name)
	}

	before := dnsServiceIPs(t, c, networks, "web")
	require.NotEmpty(t, before, "web should have DNS records for 3 replicas")

	_, err = testlib.RunCompose(ctx, t, pn, "", nil, "-f", compose, "up", "--detach", "--scale=web=1")
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
	testlib.SkipLocal(t)
	t.Parallel()

	ctx := t.Context()
	composeDNS := testlib.Fixture(t, "dns", "compose.yaml")
	composeDNS2 := testlib.Fixture(t, "dns2", "compose.yaml")

	// dns2's compose.yaml hardcodes x-incus-compose.network: dns-default, so
	// the dns project must be named literally "dns" for the names to line up.
	// Cleanups run LIFO, so dns2 (registered second) is torn down before dns.
	testlib.CleanupCompose(t, "dns", "-f", composeDNS, "down", "--project")
	testlib.CleanupCompose(t, "dns2", "-f", composeDNS2, "down", "--project")

	_, err := testlib.RunCompose(ctx, t, "dns", "", nil, "-f", composeDNS, "up", "--detach")
	require.NoError(t, err)

	_, err = testlib.RunCompose(ctx, t, "dns2", "", nil, "-f", composeDNS2, "up", "--detach")
	require.NoError(t, err)

	// Matches dns2's hardcoded x-incus-compose.network: dns-default.
	const networkName = "dns-default"

	c := projectClient(ctx, t, "dns")
	conn, err := c.Connection()
	require.NoError(t, err)

	net, _, err := conn.GetNetwork(ctx, networkName)
	require.NoError(t, err)

	snapshotter.SnapshotT(t, testlib.Strip(testlib.StripIPv6Lines(net.Config["raw.dnsmasq"])))
}
