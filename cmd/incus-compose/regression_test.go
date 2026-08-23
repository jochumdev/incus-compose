package main

import (
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
