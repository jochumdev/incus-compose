package main

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/urfave/cli/v3"

	"github.com/lxc/incus-compose/client"
	"github.com/lxc/incus-compose/internal/testlib"
)

func TestDownFlags(t *testing.T) {
	tests := []struct {
		name        string
		args        []string
		env         map[string]string
		wantErr     string
		wantRmi     string
		wantVolumes bool
		wantImages  bool
	}{
		{
			name:    "down --rmi without arg errors",
			args:    []string{"down", "--rmi"},
			wantErr: "flag needs an argument: --rmi",
		},
		{
			name:    "down --rmi with following flag errors",
			args:    []string{"down", "--rmi", "--volumes"},
			wantErr: `must be "local" or "all"`,
		},
		{
			name:    "down --rmi invalid errors",
			args:    []string{"down", "--rmi", "invalid"},
			wantErr: `must be "local" or "all"`,
		},
		{
			name:       "down --rmi local",
			args:       []string{"down", "--rmi", "local"},
			wantRmi:    "local",
			wantImages: true,
		},
		{
			name:       "down --rmi all",
			args:       []string{"down", "--rmi", "all"},
			wantRmi:    "all",
			wantImages: true,
		},
		{
			name:        "down --rmi local --volumes",
			args:        []string{"down", "--rmi", "local", "--volumes"},
			wantRmi:     "local",
			wantImages:  true,
			wantVolumes: true,
		},
		{
			name:        "down --volumes",
			args:        []string{"down", "--volumes"},
			wantVolumes: true,
		},
		{
			name:    "down with invalid INCUS_COMPOSE_DOWN_RMI errors",
			args:    []string{"down"},
			env:     map[string]string{"INCUS_COMPOSE_DOWN_RMI": "invalid"},
			wantErr: `must be "local" or "all"`,
		},
		{
			name:       "down with valid INCUS_COMPOSE_DOWN_RMI",
			args:       []string{"down"},
			env:        map[string]string{"INCUS_COMPOSE_DOWN_RMI": "local"},
			wantRmi:    "local",
			wantImages: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for k, v := range tt.env {
				t.Setenv(k, v)
			}

			cmd := newDownCommand()
			var capturedRmi string
			var capturedVolumes bool
			var capturedImages bool

			cmd.Action = func(ctx context.Context, c *cli.Command) error {
				capturedRmi = c.String("rmi")
				capturedVolumes = c.Bool("volumes")
				capturedImages = c.Bool("images") || c.String("rmi") == "local" || c.String("rmi") == "all"
				return nil
			}

			err := cmd.Run(t.Context(), tt.args)
			if tt.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantErr)
				return
			}

			require.NoError(t, err)
			assert.Equal(t, tt.wantRmi, capturedRmi)
			assert.Equal(t, tt.wantVolumes, capturedVolumes)
			assert.Equal(t, tt.wantImages, capturedImages)
		})
	}

	t.Run("down <service> --volumes flag parsing", func(t *testing.T) {
		cmd := newDownCommand()
		var capturedVolumes bool
		var capturedArgs []string

		cmd.Action = func(ctx context.Context, c *cli.Command) error {
			capturedVolumes = c.Bool("volumes")
			capturedArgs = c.Args().Slice()
			return nil
		}

		err := cmd.Run(t.Context(), []string{"down", "app", "--volumes"})
		require.NoError(t, err)
		assert.True(t, capturedVolumes, "volumes flag should be true")
		assert.Equal(t, []string{"app"}, capturedArgs)
	})
}

// TestE2EDownServiceVolumes pins that down <service> --volumes deletes the service's volumes.
func TestE2EDownServiceVolumes(t *testing.T) {
	t.Parallel()
	testlib.SkipE2E(t)

	ctx := t.Context()
	pn := t.Name()
	compose := testlib.Fixture(t, "with-volume", "compose.yaml")

	testlib.CleanupCompose(t, pn, "-f", compose, "down", "--project")

	_, err := testlib.RunCompose(ctx, t, pn, "", nil, "-f", compose, "up", "--detach")
	require.NoError(t, err)

	gc, err := client.NewTestClient(ctx)
	require.NoError(t, err)

	pc, err := gc.EnsureProject(pn)
	require.NoError(t, err)

	pool := pc.Config().DefaultStoragePool

	volumes := func() []string {
		t.Helper()

		out, err := testlib.RunCompose(ctx, t, pn, "", nil, "-f", compose,
			"incus", "storage", "volume", "list", pool, "--format=csv", "-c", "n")
		require.NoError(t, err)

		names := []string{}
		for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
			if strings.HasPrefix(line, "vol-") {
				names = append(names, line)
			}
		}

		return names
	}

	require.NotEmpty(t, volumes(), "volume should exist after up")

	_, err = testlib.RunCompose(ctx, t, pn, "", nil, "-f", compose, "down", "app")
	require.NoError(t, err)
	assert.NotEmpty(t, volumes(), "volume should still exist after down without --volumes")

	_, err = testlib.RunCompose(ctx, t, pn, "", nil, "-f", compose, "down", "app", "--volumes")
	require.NoError(t, err)

	assert.Empty(t, volumes(), "down <service> --volumes should delete the service's volume")
}

// TestE2EDownUnknownService verifies that down with an unknown service name
// fails with an error rather than silently exiting 0.
func TestE2EDownUnknownService(t *testing.T) {
	t.Parallel()
	testlib.SkipE2E(t)

	ctx := t.Context()
	pn := t.Name()
	compose := testlib.Fixture(t, "with-volume", "compose.yaml")

	_, err := testlib.RunCompose(ctx, t, pn, "", nil, "-f", compose, "down", "does-not-exist")
	require.Error(t, err)

	_, err = testlib.RunCompose(ctx, t, pn, "", nil, "-f", compose, "down", "does-not-exist", "--volumes")
	require.Error(t, err)
}
