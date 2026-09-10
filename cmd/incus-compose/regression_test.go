package main

import (
	"path/filepath"
	"testing"

	incusApi "github.com/lxc/incus/v7/shared/api"
	"github.com/stretchr/testify/require"

	"github.com/lxc/incus-compose/client"
	"github.com/lxc/incus-compose/internal/testlib"
)

// TestNoDanglingNetworksAfterDown is a regression test for the project default
// network not being removed after `down --project`.
func TestNoDanglingNetworksAfterDown(t *testing.T) {
	t.Parallel()
	testlib.SkipLocal(t)

	ctx := t.Context()
	pn := t.Name()
	compose := testlib.Fixture(t, "simple", "compose.yaml")

	testlib.CleanupCompose(t, pn, "-f", compose, "down", "--project")

	_, err := testlib.RunCompose(ctx, t, pn, "", nil, "-f", compose, "up", "--detach")
	require.NoError(t, err)

	_, err = testlib.RunCompose(ctx, t, pn, "", nil, "-f", compose, "down", "--project")
	require.NoError(t, err)

	gc, err := client.NewTestClient(ctx)
	require.NoError(t, err)

	conn, err := gc.Connection()
	require.NoError(t, err)

	networkName := client.SanitizeNetworkName(pn, "ic-", "default")
	networkNames, err := conn.GetNetworkNames(t.Context(), incusApi.ProjectDefaultName)
	require.NoError(t, err)

	require.NotContains(t, networkNames, networkName, "network %q was not removed by down --project", networkName)
}

// TestE2EStartStopIdempotent checks that running start/stop twice (idempotent) works without errors.
func TestE2EStartStopIdempotent(t *testing.T) {
	t.Parallel()
	testlib.SkipE2E(t)

	compose := testlib.Fixture(t, "simple", "compose.yaml")

	ctx := t.Context()
	pn := t.Name()

	testlib.CleanupCompose(t, pn, "-f", compose, "down", "--project")

	tests := []e2eTest{
		{
			name: "up",
			args: []string{"-f", compose, "up", "--detach"},
		},
		{
			name: "stop",
			args: []string{"-f", compose, "stop"},
		},
		{
			name: "stop idempotent",
			args: []string{"-f", compose, "stop"},
		},
		{
			name: "start",
			args: []string{"-f", compose, "start"},
		},
		{
			name: "start idempotent",
			args: []string{"-f", compose, "start"},
		},
	}

	for _, tt := range tests {
		_, err := testlib.RunCompose(ctx, t, pn, "", nil, tt.args...)
		require.NoError(t, err)
	}
}

func TestE2ENoImageCache(t *testing.T) {
	t.Parallel()
	testlib.SkipE2E(t)

	compose := testlib.Fixture(t, "simple", "compose.yaml")

	ctx := t.Context()
	pn := t.Name()

	testlib.CleanupCompose(t, pn, "-f", compose, "down", "--project")

	_, err := testlib.RunCompose(ctx, t, pn, "", nil, "-f", compose, "up", "--detach", "--image-cache", "")
	require.NoError(t, err)
}

// TestE2EUpRecreateDependents pins that `up --recreate` on a service brings back
// services that depend on it (#192).
func TestE2EUpRecreateDependents(t *testing.T) {
	t.Parallel()
	testlib.SkipE2E(t)

	ctx := t.Context()
	pn := t.Name()
	dir := testlib.WriteTempFiles(t, map[string]string{
		"compose.yaml": `services:
  alpha:
    image: docker.io/alpine:edge
  beta:
    image: docker.io/alpine:edge
    depends_on:
      - alpha
`})
	compose := filepath.Join(dir, "compose.yaml")

	testlib.CleanupCompose(t, pn, "-f", compose, "down", "--project")

	_, err := testlib.RunCompose(ctx, t, pn, "", nil, "-f", compose, "up", "--detach", "--no-healthd")
	require.NoError(t, err)

	c := projectClient(ctx, t, pn)
	conn, err := c.Connection()
	require.NoError(t, err)

	uuid := func(name string) string {
		inst, _, err := conn.GetInstance(ctx, c.IncusProject(), name, nil)
		require.NoError(t, err)

		return inst.Config["volatile.uuid"]
	}

	alphaUUID := uuid("alpha-1")
	betaUUID := uuid("beta-1")
	require.NotEmpty(t, alphaUUID)
	require.NotEmpty(t, betaUUID)

	_, err = testlib.RunCompose(ctx, t, pn, "", nil, "-f", compose, "up", "--detach", "--recreate", "--no-healthd", "alpha")
	require.NoError(t, err)

	require.NotEqual(t, alphaUUID, uuid("alpha-1"), "alpha must be recreated")
	require.NotEqual(t, betaUUID, uuid("beta-1"), "beta must be recreated")
}
