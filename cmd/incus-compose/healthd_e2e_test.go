package main

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lxc/incus-compose/client"
	"github.com/lxc/incus-compose/shared"
	"github.com/lxc/incus-compose/testlib"
)

const healthdScopeCompose = "../../test/fixtures/healthd-scope/compose.yaml"

// projectScope returns the scope the Incus project carries.
func projectScope(t *testing.T, c *client.Client) string {
	t.Helper()

	config, err := c.Global().ProjectConfig(c.Project())
	require.NoError(t, err)

	return config[shared.HealthScopeKey]
}

// waitHealthy blocks until healthd reports the instance healthy.
func waitHealthy(t *testing.T, c *client.Client, name string) {
	t.Helper()

	conn, err := c.Connection()
	require.NoError(t, err)

	var status string
	require.Eventuallyf(t, func() bool {
		inst, _, err := conn.GetInstance(t.Context(), name, nil)
		if err != nil {
			return false
		}

		status = inst.Config[shared.HealthStatusKey]

		return status == shared.HealthStatusHealthy
	}, 90*time.Second, time.Second, "%s never became healthy, last status %q", name, status)
}

// TestE2EHealthdGlobalScope is the new default: no sidecar of the project's
// own, one shared daemon in its own project, and the project marked so the
// daemon picks it up.
func TestE2EHealthdGlobalScope(t *testing.T) {
	testlib.SkipLocal(t)
	testlib.SkipE2E(t)

	ctx := t.Context()
	pn := t.Name()

	t.Cleanup(func() {
		_, _ = runCommand(context.Background(), t, pn, "-f", healthdScopeCompose, "down", "--project")
	})

	_, err := runCommand(ctx, t, pn, "-f", healthdScopeCompose, "up", "--detach")
	require.NoError(t, err)

	c := projectClient(ctx, t, pn)

	assert.Equal(t, shared.HealthScopeGlobal, projectScope(t, c))

	local, err := c.InstanceExists(healthdInstanceName(c.IncusProject(), false))
	require.NoError(t, err)
	assert.False(t, local, "the project must not carry a sidecar of its own")

	hc := projectClient(ctx, t, systemProject)
	global, err := hc.InstanceExists(globalHealthdName)
	require.NoError(t, err)
	assert.True(t, global, "the shared daemon must exist in its own project")

	// It brings its own root disk and NIC, so nothing about the default
	// project's profile can break it.
	conn, err := hc.Connection()
	require.NoError(t, err)

	inst, _, err := conn.GetInstance(ctx, globalHealthdName, nil)
	require.NoError(t, err)
	assert.Equal(t, "disk", inst.Devices["root"]["type"])
	assert.Equal(t, globalHealthdNetwork, inst.Devices["eth0"]["network"])

	waitHealthy(t, c, "web-1")
}

// TestE2EHealthdGlobalComposeNetwork attaches the shared daemon to a network
// the compose file declares. Before it had a project of its own it took eth0
// from the default profile and the setting was warned about and dropped.
//
// Not parallel: it recreates the daemon every other project uses.
func TestE2EHealthdGlobalComposeNetwork(t *testing.T) {
	testlib.SkipLocal(t)
	testlib.SkipE2E(t)

	ctx := t.Context()
	pn := strings.ToLower(t.Name())

	dir := testlib.WriteTempFiles(t, map[string]string{"compose.yaml": `x-incus-compose:
  healthd:
    network: ` + pn + `:hnet
networks:
  hnet:
services:
  web:
    image: docker.io/library/busybox:glibc
    restart: unless-stopped
    networks: [hnet]
    entrypoint: ["/bin/sh", "-c", "mkdir -p /www && echo web-ok > /www/index.html && httpd -f -v -p 8080 -h /www"]
    healthcheck:
      test: ["CMD", "wget", "-q", "--spider", "http://127.0.0.1:8080"]
      interval: 5s
      timeout: 5s
      retries: 3
`})
	compose := filepath.Join(dir, "compose.yaml")

	// An empty directory is how a healthd command is left with no compose file
	// to read, which is what makes it act on the shared daemon.
	noProject := t.TempDir()

	t.Cleanup(func() {
		// Before the project, or the daemon still holds a NIC on hnet.
		_, _ = runCommand(context.Background(), t, pn, "-P", noProject, "healthd", "down", "--force")
		_, _ = runCommand(context.Background(), t, pn, "-f", compose, "down", "--project")
	})

	// The first project to create the daemon supplies its settings, so this only
	// takes effect with nothing running.
	_, _ = runCommand(ctx, t, pn, "-P", noProject, "healthd", "down", "--force")

	_, err := runCommand(ctx, t, pn, "-f", compose, "up", "--detach")
	require.NoError(t, err)

	c := projectClient(ctx, t, pn)
	hc := projectClient(ctx, t, systemProject)

	conn, err := hc.Connection()
	require.NoError(t, err)

	inst, _, err := conn.GetInstance(ctx, globalHealthdName, nil)
	require.NoError(t, err)
	assert.Equal(t, plannedNetworkNames(ctx, t, pn, compose), []string{inst.Devices["eth0"]["network"]})

	// It reaches Incus over that bridge, so it reports at all.
	waitHealthy(t, c, "web-1")
}

// TestE2EHealthdProjectScope keeps the old topology when asked for it.
func TestE2EHealthdProjectScope(t *testing.T) {
	testlib.SkipLocal(t)
	testlib.SkipE2E(t)

	ctx := t.Context()
	pn := t.Name()

	t.Cleanup(func() {
		_, _ = runCommand(context.Background(), t, pn, "-f", healthdScopeCompose, "down", "--project")
	})

	_, err := runCommand(ctx, t, pn, "-f", healthdScopeCompose, "up", "--detach", "--healthd-scope", "project")
	require.NoError(t, err)

	c := projectClient(ctx, t, pn)

	assert.Equal(t, shared.HealthScopeProject, projectScope(t, c))

	local, err := c.InstanceExists(healthdInstanceName(c.IncusProject(), false))
	require.NoError(t, err)
	assert.True(t, local, "the project must carry a sidecar of its own")

	waitHealthy(t, c, "web-1")
}

// TestE2EHealthdCoexistence is the load-bearing case: a project-scoped daemon
// and the shared one must both work and neither may watch the other's project.
func TestE2EHealthdCoexistence(t *testing.T) {
	testlib.SkipLocal(t)
	testlib.SkipE2E(t)

	ctx := t.Context()
	globalPN := t.Name() + "-global"
	projectPN := t.Name() + "-project"

	t.Cleanup(func() {
		_, _ = runCommand(context.Background(), t, globalPN, "-f", healthdScopeCompose, "down", "--project")
		_, _ = runCommand(context.Background(), t, projectPN, "-f", healthdScopeCompose, "down", "--project")
	})

	_, err := runCommand(ctx, t, globalPN, "-f", healthdScopeCompose, "up", "--detach")
	require.NoError(t, err)

	_, err = runCommand(ctx, t, projectPN, "-f", healthdScopeCompose, "up", "--detach", "--healthd-scope", "project")
	require.NoError(t, err)

	gc := projectClient(ctx, t, globalPN)
	pc := projectClient(ctx, t, projectPN)

	assert.Equal(t, shared.HealthScopeGlobal, projectScope(t, gc))
	assert.Equal(t, shared.HealthScopeProject, projectScope(t, pc))

	// The scope-marked project has no sidecar, the project-scoped one does, and
	// the shared daemon is in neither.
	local, err := gc.InstanceExists(healthdInstanceName(gc.IncusProject(), false))
	require.NoError(t, err)
	assert.False(t, local)

	local, err = pc.InstanceExists(healthdInstanceName(pc.IncusProject(), false))
	require.NoError(t, err)
	assert.True(t, local)

	// Both report healthy, so neither daemon is starved by the other.
	waitHealthy(t, gc, "web-1")
	waitHealthy(t, pc, "web-1")

	// The project-scoped one stays out of the shared daemon's scope, which is
	// what stops the two from both restarting the same instance.
	watched, err := gc.Global().ProjectsWithConfig(shared.HealthScopeKey, shared.HealthScopeGlobal)
	require.NoError(t, err)
	assert.Contains(t, watched, gc.IncusProject())
	assert.NotContains(t, watched, pc.IncusProject())

	// Taking the project-scoped daemon down leaves the shared one alone.
	_, err = runCommand(ctx, t, projectPN, "-f", healthdScopeCompose, "healthd", "down")
	require.NoError(t, err)

	local, err = pc.InstanceExists(healthdInstanceName(pc.IncusProject(), false))
	require.NoError(t, err)
	assert.False(t, local)

	dc := projectClient(ctx, t, systemProject)
	global, err := dc.InstanceExists(globalHealthdName)
	require.NoError(t, err)
	assert.True(t, global, "the shared daemon must survive a project-scoped healthd down")
}

// TestE2EHealthdNoComposeFile covers the sub-commands run with no compose file:
// they act on the shared daemon, and say so when there is none.
//
// Not parallel: it creates and removes the daemon every other project uses.
func TestE2EHealthdNoComposeFile(t *testing.T) {
	testlib.SkipLocal(t)
	testlib.SkipE2E(t)

	ctx := t.Context()
	dir := t.TempDir()

	// -P moves the working directory, which is what leaves the commands with no
	// compose file to find.
	noProject := func(args ...string) (*bytes.Buffer, error) {
		return runCommand(ctx, t, t.Name(), append([]string{"-P", dir}, args...)...)
	}

	dc := projectClient(ctx, t, systemProject)

	// Start from no daemon at all, whatever earlier tests left behind.
	_, _ = noProject("healthd", "down", "--force")

	_, err := noProject("healthd", "logs")
	require.Error(t, err, "with no daemon and no project there is nothing to act on")

	_, err = noProject("healthd", "up")
	require.NoError(t, err)

	global, err := dc.InstanceExists(globalHealthdName)
	require.NoError(t, err)
	assert.True(t, global, "healthd up with no compose file must create the shared daemon")

	// It marks no project, so it watches nothing of its own making.
	_, err = noProject("healthd", "logs")
	require.NoError(t, err)

	_, err = noProject("healthd", "down", "--force")
	require.NoError(t, err)

	global, err = dc.InstanceExists(globalHealthdName)
	require.NoError(t, err)
	assert.False(t, global)
}

// TestE2EHealthdDownNeedsForce covers the gate on the shared daemon: with other
// projects relying on it, `healthd down` has to be told twice.
//
// To prove it still bites, disable the gate in healthd_down.go and re-run:
//
//	if false && global && !cmd.Bool("force") {
//
// The first assertion below must fail with "an error is expected but got nil".
//
// Not parallel: it removes the daemon every other project uses.
func TestE2EHealthdDownNeedsForce(t *testing.T) {
	testlib.SkipLocal(t)
	testlib.SkipE2E(t)

	ctx := t.Context()
	one := t.Name() + "-one"
	two := t.Name() + "-two"

	t.Cleanup(func() {
		_, _ = runCommand(context.Background(), t, one, "-f", healthdScopeCompose, "down", "--project")
		_, _ = runCommand(context.Background(), t, two, "-f", healthdScopeCompose, "down", "--project")
	})

	for _, pn := range []string{one, two} {
		_, err := runCommand(ctx, t, pn, "-f", healthdScopeCompose, "up", "--detach")
		require.NoError(t, err)
	}

	dc := projectClient(ctx, t, systemProject)

	// Two projects carry scope=global, so taking the daemon down is refused:
	// the tests have no terminal to confirm on.
	_, err := runCommand(ctx, t, one, "-f", healthdScopeCompose, "healthd", "down")
	require.Error(t, err)

	global, err := dc.InstanceExists(globalHealthdName)
	require.NoError(t, err)
	assert.True(t, global, "a refused healthd down must leave the daemon running")

	// --force is the second telling.
	_, err = runCommand(ctx, t, one, "-f", healthdScopeCompose, "healthd", "down", "--force")
	require.NoError(t, err)

	global, err = dc.InstanceExists(globalHealthdName)
	require.NoError(t, err)
	assert.False(t, global, "--force must remove the shared daemon")
}

// TestE2EHealthdMigratesToGlobal covers the upgrade path: the project sidecar is
// removed before the project is marked, so the two never overlap.
func TestE2EHealthdMigratesToGlobal(t *testing.T) {
	testlib.SkipLocal(t)
	testlib.SkipE2E(t)

	ctx := t.Context()
	pn := t.Name()

	t.Cleanup(func() {
		_, _ = runCommand(context.Background(), t, pn, "-f", healthdScopeCompose, "down", "--project")
	})

	_, err := runCommand(ctx, t, pn, "-f", healthdScopeCompose, "up", "--detach", "--healthd-scope", "project")
	require.NoError(t, err)

	c := projectClient(ctx, t, pn)

	local, err := c.InstanceExists(healthdInstanceName(c.IncusProject(), false))
	require.NoError(t, err)
	require.True(t, local)

	// The stored scope wins over everything, so a migration is a deliberate
	// change to the project itself.
	conn, err := c.Global().Connection()
	require.NoError(t, err)

	incusProject, etag, err := conn.GetProject(ctx, c.IncusProject())
	require.NoError(t, err)

	writable := incusProject.Writable()
	writable.Config[shared.HealthScopeKey] = shared.HealthScopeGlobal
	require.NoError(t, conn.UpdateProject(ctx, c.IncusProject(), writable, etag))

	_, err = runCommand(ctx, t, pn, "-f", healthdScopeCompose, "up", "--detach")
	require.NoError(t, err)

	assert.Equal(t, shared.HealthScopeGlobal, projectScope(t, c))

	local, err = c.InstanceExists(healthdInstanceName(c.IncusProject(), false))
	require.NoError(t, err)
	assert.False(t, local, "the project sidecar must be gone after the migration")

	dc := projectClient(ctx, t, systemProject)
	global, err := dc.InstanceExists(globalHealthdName)
	require.NoError(t, err)
	assert.True(t, global)

	waitHealthy(t, c, "web-1")
}
