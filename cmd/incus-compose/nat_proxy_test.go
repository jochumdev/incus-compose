package main

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lxc/incus-compose/client"
	"github.com/lxc/incus-compose/internal/testlib"
	"github.com/lxc/incus-compose/shared"
)

// TestE2ENatProxy verifies that published ports create NAT proxy devices
// with the correct configuration (nat=true, wildcard connect address).
func TestE2ENATProxy(t *testing.T) {
	t.Parallel()
	testlib.SkipE2E(t)

	ctx := t.Context()
	pn := t.Name()
	compose := testlib.Fixture(t, "with-ports", "compose.yaml")

	testlib.CleanupCompose(t, pn, "-f", compose, "down", "--project")

	tests := []e2eTest{
		{
			name:            "up",
			args:            []string{"-f", compose, "up", "--detach"},
			snapshotList:    true,
			snapStripHealth: true,
		},
	}

	runE2ETests(ctx, t, pn, tests)

	c := projectClient(ctx, t, pn)
	conn, err := c.Connection()
	require.NoError(t, err)
	inst, _, err := conn.GetInstance(ctx, c.IncusProject(), "web-nat-1", nil)
	require.NoError(t, err)

	proxyDev, ok := inst.Devices["proxy-8081"]
	require.True(t, ok, "proxy-8081 device should exist")
	assert.Equal(t, "proxy", proxyDev["type"])
	assert.Equal(t, "true", proxyDev["nat"])
	assert.Equal(t, "tcp:0.0.0.0:8081", proxyDev["listen"])
	assert.Equal(t, "tcp:0.0.0.0:80", proxyDev["connect"])
}

func TestE2ENATProxyWithPort(t *testing.T) {
	t.Parallel()
	testlib.SkipE2E(t)

	ctx := t.Context()
	pn := t.Name()

	dir := testlib.WriteTempFiles(t, map[string]string{
		"compose.yaml": `services:
  web:
    image: docker.io/nginx:alpine
    ports:
      - published: "8080"
        target: "80"
        x-incus-compose:
          nat: true
`,
	})
	compose := filepath.Join(dir, "compose.yaml")

	testlib.CleanupCompose(t, pn, "-f", compose, "down", "--project")

	_, err := testlib.RunCompose(ctx, t, pn, "", nil, "-f", compose, "up", "--detach")
	require.NoError(t, err)

	c := projectClient(ctx, t, pn)
	conn, err := c.Connection()
	require.NoError(t, err)

	inst, _, err := conn.GetInstance(ctx, c.IncusProject(), "web-1", nil)
	require.NoError(t, err)

	proxy, ok := inst.Devices["proxy-8080"]
	require.True(t, ok, "proxy-8080 device should exist")
	assert.Contains(t, proxy["connect"], "0.0.0.0:80")
	assert.Equal(t, "true", proxy["nat"])
}

func TestE2ENATProxyWithPortAndStaticIP(t *testing.T) {
	t.Parallel()
	testlib.SkipE2E(t)
	testlib.SkipNoExtension(t, shared.Incus73Extension, "nat tests with static ip require at least incus 7.3 or 7.0.2 LTS")

	ctx := t.Context()
	pn := t.Name()

	c := projectClient(ctx, t, pn, client.EnsureProjectWithCreate())
	conn, err := c.Connection()
	require.NoError(t, err)

	dir := testlib.WriteTempFiles(t, map[string]string{
		"compose.yaml": `services:
  web:
    image: docker.io/nginx:alpine
    ports:
      - published: "8080"
        target: "80"
        x-incus-compose:
          nat: true
    networks:
      frontend:
        ipv4_address: 10.131.245.2/24

networks:
  frontend:
    x-incus:
      ipv4.address: 10.131.245.1/24
`,
	})
	compose := filepath.Join(dir, "compose.yaml")

	testlib.CleanupCompose(t, pn, "-f", compose, "down", "--project")

	_, err = testlib.RunCompose(ctx, t, pn, "", nil, "-f", compose, "up", "--detach")
	require.NoError(t, err)

	inst, _, err := conn.GetInstance(ctx, c.IncusProject(), "web-1", nil)
	require.NoError(t, err)

	proxy, ok := inst.Devices["proxy-8080"]
	require.True(t, ok, "proxy-8080 device should exist")
	assert.Contains(t, proxy["connect"], "10.131.245.2:80")
	assert.Equal(t, "true", proxy["nat"])
}
