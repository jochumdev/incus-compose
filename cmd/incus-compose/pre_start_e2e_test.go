package main

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lxc/incus-compose/client"
	"github.com/lxc/incus-compose/internal/testlib"
	"github.com/lxc/incus-compose/shared"
)

// TestE2EPreStart verifies that pre_start hooks run before the main service
// container starts, that shared volume modifications are visible to the service,
// and that the ephemeral runner container is removed on success.
func TestE2EPreStart(t *testing.T) {
	t.Parallel()
	testlib.SkipE2E(t)
	testlib.SkipNoExtension(t, shared.Incus73Extension, "pre_start tests work best with incus 7.3 or higher")

	ctx := t.Context()
	pn := t.Name()

	dir := testlib.WriteTempFiles(t, map[string]string{
		"compose.yaml": `services:
  web:
    image: docker.io/alpine:edge
    volumes:
      - data:/data
    x-incus:
      oci.entrypoint: sh
    pre_start:
      - command: ["sh", "-c", "echo pre_start_ok > /data/init.txt"]
volumes:
  data:
`,
	})
	compose := filepath.Join(dir, "compose.yaml")

	testlib.CleanupCompose(t, pn, "-f", compose, "down", "--project")

	_, err := testlib.RunCompose(ctx, t, pn, "", nil, "-f", compose, "up", "--detach")
	require.NoError(t, err)

	stdout, err := testlib.RunCompose(ctx, t, pn, "", nil,
		"-f", compose, "exec", "--no-tty", "web", "--", "cat", "/data/init.txt")
	require.NoError(t, err)
	assert.Equal(t, "pre_start_ok", strings.TrimSpace(stdout))

	c := projectClient(ctx, t, pn)
	exists, err := c.InstanceExists("web-pre_start-0")
	require.NoError(t, err)
	assert.False(t, exists, "runner container must be deleted on success")

	exists, err = c.InstanceExists("web-1")
	require.NoError(t, err)
	assert.True(t, exists, "main service instance must exist")

	listOut, err := testlib.RunCompose(ctx, t, pn, "", nil, "-f", compose, "list")
	require.NoError(t, err)
	assert.NotContains(t, listOut, "pre_start")

	_, err = testlib.RunCompose(ctx, t, pn, "", nil, "-f", compose, "restart")
	require.NoError(t, err)

	exists, err = c.InstanceExists("web-pre_start-0")
	require.NoError(t, err)
	assert.False(t, exists, "runner container must be deleted on success after restart")
}

// TestE2EPreStartFailure verifies that a failing pre_start hook prevents the main
// container from starting and leaves the runner container stopped for inspection.
func TestE2EPreStartFailure(t *testing.T) {
	t.Parallel()
	testlib.SkipE2E(t)
	testlib.SkipNoExtension(t, shared.Incus73Extension, "pre_start tests work best with incus 7.3 or higher")

	ctx := t.Context()
	pn := t.Name()

	dir := testlib.WriteTempFiles(t, map[string]string{
		"compose.yaml": `services:
  web:
    image: docker.io/alpine:edge
    x-incus:
      oci.entrypoint: sh
    pre_start:
      - command: ["sh", "-c", "exit 1"]
`,
	})
	compose := filepath.Join(dir, "compose.yaml")

	testlib.CleanupCompose(t, pn, "-f", compose, "down", "--project")

	_, err := testlib.RunCompose(ctx, t, pn, "", nil, "-f", compose, "up", "--detach")
	require.Error(t, err, "up must fail when pre_start hook exits non-zero")

	c := projectClient(ctx, t, pn)
	exists, err := c.InstanceExists("web-pre_start-0")
	require.NoError(t, err)
	assert.True(t, exists, "runner container must be retained on failure for inspection")

	res, err := c.Resource(client.KindInstance, "web-pre_start-0", &client.InstanceConfig{})
	require.NoError(t, err)
	runnerInst, ok := res.(*client.Instance)
	require.True(t, ok)
	assert.False(t, runnerInst.Running(), "runner container should be stopped")

	exists, err = c.InstanceExists("web-1")
	require.NoError(t, err)
	if exists {
		mainRes, err := c.Resource(client.KindInstance, "web-1", &client.InstanceConfig{})
		require.NoError(t, err)
		mainInst, ok := mainRes.(*client.Instance)
		require.True(t, ok)
		assert.False(t, mainInst.Running(), "main container must never start if pre_start fails")
	}
}

// TestE2EPreStartPerReplica verifies that when per_replica is true, each replica
// executes its own init container before starting.
func TestE2EPreStartPerReplica(t *testing.T) {
	t.Parallel()
	testlib.SkipE2E(t)
	testlib.SkipNoExtension(t, shared.Incus73Extension, "pre_start tests work best with incus 7.3 or higher")

	ctx := t.Context()
	pn := t.Name()

	dir := testlib.WriteTempFiles(t, map[string]string{
		"compose.yaml": `services:
  web:
    image: docker.io/alpine:edge
    x-incus:
      oci.entrypoint: sh
    deploy:
      replicas: 2
    pre_start:
      - command: ["sh", "-c", "echo replica_init"]
        per_replica: true
`,
	})
	compose := filepath.Join(dir, "compose.yaml")

	testlib.CleanupCompose(t, pn, "-f", compose, "down", "--project")

	_, err := testlib.RunCompose(ctx, t, pn, "", nil, "-f", compose, "up", "--detach")
	require.NoError(t, err)

	c := projectClient(ctx, t, pn)

	for _, name := range []string{"web-1", "web-2"} {
		exists, err := c.InstanceExists(name)
		require.NoError(t, err)
		assert.True(t, exists, "service replica %s must exist", name)
	}

	for _, runner := range []string{"web-1-pre_start-0", "web-2-pre_start-0"} {
		exists, err := c.InstanceExists(runner)
		require.NoError(t, err)
		assert.False(t, exists, "per_replica runner %s must be deleted on success", runner)
	}
}
