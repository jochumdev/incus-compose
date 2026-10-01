package main

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lxc/incus-compose/client"
	"github.com/lxc/incus-compose/internal/testlib"
	"github.com/lxc/incus-compose/project"
	"github.com/lxc/incus-compose/shared"
)

func projectDNSZone(t *testing.T, c *client.Client) string {
	t.Helper()

	config, err := c.Global().ProjectConfig(c.Project())
	require.NoError(t, err)

	return config[shared.DNSZoneKey]
}

func TestE2EDNSUpProjectScopeWithZone(t *testing.T) {
	testlib.SkipE2E(t)

	ctx := t.Context()
	pn := strings.ToLower(t.Name())

	dir := testlib.WriteTempFiles(t, map[string]string{
		"compose.yaml": `name: ` + pn + `
x-incus-compose:
  dns:
    scope: project
    zone: testzone.incus
services:
  web:
    image: docker.io/alpine:edge
    command: sh
`,
	})
	compose := filepath.Join(dir, "compose.yaml")

	testlib.CleanupCompose(t, pn, "-f", compose, "down", "--project")

	_, err := testlib.RunCompose(ctx, t, pn, "", nil, "-f", compose, "up", "--detach", "--no-healthd")
	require.NoError(t, err)

	c := projectClient(ctx, t, pn)

	assert.Equal(t, "testzone.incus", projectDNSZone(t, c))

	dnsName := dnsInstanceName(c.IncusProject(), false)
	exists, err := c.InstanceExists(dnsName)
	require.NoError(t, err)
	assert.True(t, exists, "the project-scoped dns sidecar must exist")

	conn, err := c.Connection()
	require.NoError(t, err)

	inst, _, err := conn.GetInstance(ctx, c.IncusProject(), "web-1", nil)
	require.NoError(t, err)

	assert.NotEmpty(t, inst.Config["oci.dns.nameservers"], "instance must have oci.dns.nameservers set")
	assert.Equal(t, "testzone.incus", inst.Config["oci.dns.search"], "instance must have oci.dns.search set to the zone")

	statusOut, err := testlib.RunCompose(ctx, t, pn, "", nil, "-f", compose, "dns", "status")
	require.NoError(t, err)
	assert.Contains(t, statusOut, "Status: ready")
	assert.Contains(t, statusOut, "IPv4:")
	assert.Contains(t, statusOut, "IPv6:")
	assert.NotContains(t, statusOut, "Metrics:")

	metricsOut, err := testlib.RunCompose(ctx, t, pn, "", nil, "-f", compose, "dns", "status", "--metrics")
	require.NoError(t, err)
	assert.Contains(t, metricsOut, "Status: ready")
	assert.Contains(t, metricsOut, "Metrics:")

	jsonOut, err := testlib.RunCompose(ctx, t, pn, "", nil, "-f", compose, "dns", "status", "--format", "json")
	require.NoError(t, err)
	assert.Contains(t, jsonOut, `"status": "ready"`)
	assert.Contains(t, jsonOut, `"ipv4":`)
	assert.Contains(t, jsonOut, `"ipv6":`)
	assert.NotContains(t, jsonOut, `"metrics":`)

	jsonMetricsOut, err := testlib.RunCompose(ctx, t, pn, "", nil, "-f", compose, "dns", "status", "--metrics", "--format", "json")
	require.NoError(t, err)
	assert.Contains(t, jsonMetricsOut, `"status": "ready"`)
	assert.Contains(t, jsonMetricsOut, `"metrics":`)
}

func TestE2EDNSUpNoDNSFlag(t *testing.T) {
	testlib.SkipE2E(t)

	ctx := t.Context()
	pn := strings.ToLower(t.Name())

	dir := testlib.WriteTempFiles(t, map[string]string{
		"compose.yaml": `name: ` + pn + `
x-incus-compose:
  dns:
    scope: project
    zone: testzone.incus
services:
  web:
    image: docker.io/alpine:edge
    command: sh
`,
	})
	compose := filepath.Join(dir, "compose.yaml")

	testlib.CleanupCompose(t, pn, "-f", compose, "down", "--project")

	_, err := testlib.RunCompose(ctx, t, pn, "", nil, "-f", compose, "up", "--detach", "--no-healthd", "--no-dns")
	require.NoError(t, err)

	c := projectClient(ctx, t, pn)

	dnsName := dnsInstanceName(c.IncusProject(), false)
	exists, err := c.InstanceExists(dnsName)
	require.NoError(t, err)
	assert.False(t, exists, "dns daemon must not exist when --no-dns is passed")

	conn, err := c.Connection()
	require.NoError(t, err)

	inst, _, err := conn.GetInstance(ctx, c.IncusProject(), "web-1", nil)
	require.NoError(t, err)

	assert.Empty(t, projectDNSZone(t, c), "project dns zone must not be set when DNS is disabled")
	assert.Empty(t, inst.Config["oci.dns.nameservers"], "instance must not have nameservers set when DNS is disabled")
	assert.Empty(t, inst.Config["oci.dns.search"], "instance must not have search domain set when DNS is disabled")
}

func TestE2EDNSUpDisabledInCompose(t *testing.T) {
	testlib.SkipE2E(t)

	ctx := t.Context()
	pn := strings.ToLower(t.Name())

	dir := testlib.WriteTempFiles(t, map[string]string{
		"compose.yaml": `name: ` + pn + `
x-incus-compose:
  dns:
    disabled: true
    scope: project
    zone: testzone.incus
services:
  web:
    image: docker.io/alpine:edge
    command: sh
`,
	})
	compose := filepath.Join(dir, "compose.yaml")

	testlib.CleanupCompose(t, pn, "-f", compose, "down", "--project")

	_, err := testlib.RunCompose(ctx, t, pn, "", nil, "-f", compose, "up", "--detach", "--no-healthd")
	require.NoError(t, err)

	c := projectClient(ctx, t, pn)

	dnsName := dnsInstanceName(c.IncusProject(), false)
	exists, err := c.InstanceExists(dnsName)
	require.NoError(t, err)
	assert.False(t, exists, "dns daemon must not exist when x-incus-compose.dns.disabled is true")

	conn, err := c.Connection()
	require.NoError(t, err)

	inst, _, err := conn.GetInstance(ctx, c.IncusProject(), "web-1", nil)
	require.NoError(t, err)

	assert.Empty(t, projectDNSZone(t, c), "project dns zone must not be set when DNS is disabled")
	assert.Empty(t, inst.Config["oci.dns.nameservers"], "instance must not have nameservers set when DNS is disabled")
	assert.Empty(t, inst.Config["oci.dns.search"], "instance must not have search domain set when DNS is disabled")
}

func TestE2EDNSUpDefaultZone(t *testing.T) {
	testlib.SkipE2E(t)

	ctx := t.Context()
	pn := strings.ToLower(t.Name())

	dir := testlib.WriteTempFiles(t, map[string]string{
		"compose.yaml": `name: ` + pn + `
x-incus-compose:
  dns:
    scope: project
services:
  web:
    image: docker.io/alpine:edge
    command: sh
`,
	})
	compose := filepath.Join(dir, "compose.yaml")

	testlib.CleanupCompose(t, pn, "-f", compose, "down", "--project")

	_, err := testlib.RunCompose(ctx, t, pn, "", nil, "-f", compose, "up", "--detach", "--no-healthd")
	require.NoError(t, err)

	c := projectClient(ctx, t, pn)

	assert.Equal(t, pn+"."+project.DefaultDNSZoneSuffix, projectDNSZone(t, c))

	dnsName := dnsInstanceName(c.IncusProject(), false)
	exists, err := c.InstanceExists(dnsName)
	require.NoError(t, err)
	assert.True(t, exists, "the project-scoped dns sidecar must exist")

	conn, err := c.Connection()
	require.NoError(t, err)

	inst, _, err := conn.GetInstance(ctx, c.IncusProject(), "web-1", nil)
	require.NoError(t, err)

	assert.NotEmpty(t, inst.Config["oci.dns.nameservers"], "instance must have oci.dns.nameservers set")
	assert.Equal(t, pn+"."+project.DefaultDNSZoneSuffix, inst.Config["oci.dns.search"], "instance must have oci.dns.search set to default zone")
}
