package main

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lxc/incus-compose/client"
	"github.com/lxc/incus-compose/internal/testlib"
	"github.com/lxc/incus-compose/project"
)

func TestParseHealthdNetwork(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		network string
		global  bool
		want    healthdNetworkRef
		wantErr bool
	}{
		{
			name:    "empty is the project default network",
			network: "",
			want:    healthdNetworkRef{name: "default", deflt: true},
		},
		{
			name:    "empty is the shared daemon's own bridge",
			network: "",
			global:  true,
			want: healthdNetworkRef{
				name:      globalHealthdNetwork,
				deflt:     true,
				incusName: globalHealthdNetwork,
			},
		},
		{
			name:    "an explicit bridge wins for the shared daemon too",
			network: "incusbr0",
			global:  true,
			want:    healthdNetworkRef{name: "incusbr0"},
		},
		{
			name:    "project:network references a managed network",
			network: "default:default",
			want:    healthdNetworkRef{project: "default", name: "default"},
		},
		{
			name:    "project:network with distinct names",
			network: "infra:backend",
			want:    healthdNetworkRef{project: "infra", name: "backend"},
		},
		{
			name:    "no colon is a bridge name",
			network: "incusbr0",
			want:    healthdNetworkRef{name: "incusbr0"},
		},
		{
			name:    "missing network errors",
			network: "default:",
			wantErr: true,
		},
		{
			name:    "too many colons errors",
			network: "a:b:c",
			wantErr: true,
		},
	}

	c := client.NewOfflineClient(t.Context(), "default")
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := parseHealthdNetwork(c, tt.network, tt.global)
			if tt.wantErr {
				require.Error(t, err)
				return
			}

			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

// This test is very buggy and the root of a lot of pain for me.
// func TestLifecycleHealthd(t *testing.T) {
// 	t.Parallel()
// 	testlib.SkipLocal(t)
// 	testlib.SkipE2E(t)

// 	ctx := context.Background()
// 	pn := t.Name()
// 	compose := testlib.Fixture(t, "healthd-debug", "compose.yaml")

// 	t.Cleanup(func() {
// 		_, _ = testlib.RunCompose(context.Background(), t, pn, "", nil, "-f", compose, "down", "--project")
// 	})

// 	tests := []struct {
// 		name string
// 		args []string
// 	}{
// 		{
// 			name: "up",
// 			args: []string{"-f", compose, "up", "--detach"},
// 		},
// 		{
// 			name: "list",
// 			args: []string{"-f", compose, "list"},
// 		},
// 		{
// 			name: "healthd logs",
// 			args: []string{"-f", compose, "healthd", "logs"},
// 		},
// 		{
// 			name: "healthd reload",
// 			args: []string{"-f", compose, "healthd", "reload"},
// 		},
// 		{
// 			name: "healthd restart",
// 			args: []string{"-f", compose, "healthd", "restart"},
// 		},
// 		{
// 			name: "healthd down",
// 			args: []string{"-f", compose, "healthd", "down"},
// 		},
// 		{
// 			name: "down",
// 			args: []string{"-f", compose, "down", "--project"},
// 		},
// 	}

// 	for _, tt := range tests {
// 		_, err := testlib.RunCompose(ctx, t, pn, "", nil, tt.args...)
// 		require.NoError(t, err)
// 	}
// }

func TestNoHealthdSkipsHealthdInstance(t *testing.T) {
	testlib.SkipLocal(t)
	testlib.SkipE2E(t)
	t.Parallel()

	ctx := t.Context()
	pn := t.Name()
	compose := testlib.Fixture(t, "with-restart", "compose.yaml")

	testlib.CleanupCompose(t, pn, "-f", compose, "down", "--project")

	_, err := testlib.RunCompose(ctx, t, pn, "", nil, "-f", compose, "up", "--detach", "--no-healthd")
	require.NoError(t, err)

	gc, err := client.NewTestClient(ctx)
	require.NoError(t, err)

	c, err := gc.EnsureProject(pn)
	require.NoError(t, err)

	p, err := project.New().Load(ctx,
		project.LoadFiles([]string{compose}),
		project.LoadName(strings.ToLower(pn)))
	require.NoError(t, err)

	_, h, err := healthdResolve(p, c)
	require.Nil(t, h)
	require.Error(t, err)
}

func TestNoHealthdWhenNotNeeded(t *testing.T) {
	testlib.SkipLocal(t)
	testlib.SkipE2E(t)
	t.Parallel()

	ctx := t.Context()
	pn := t.Name()
	compose := testlib.Fixture(t, "simple", "compose.yaml")

	testlib.CleanupCompose(t, pn, "-f", compose, "down", "--project")

	_, err := testlib.RunCompose(ctx, t, pn, "", nil, "-f", compose, "up", "--detach")
	require.NoError(t, err)

	gc, err := client.NewTestClient(ctx)
	require.NoError(t, err)

	c, err := gc.EnsureProject(pn)
	require.NoError(t, err)

	p, err := project.New().Load(ctx,
		project.LoadFiles([]string{compose}),
		project.LoadName(strings.ToLower(pn)))
	require.NoError(t, err)

	_, h, err := healthdResolve(p, c)
	require.Nil(t, h)
	require.Error(t, err)
}
