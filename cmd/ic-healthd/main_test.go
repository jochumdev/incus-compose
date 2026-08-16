package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/urfave/cli/v3"

	"github.com/lxc/incus-compose/internal/testlib"
	"github.com/lxc/incus-compose/shared"
)

// runFlags parses args through the real run command and hands back the config
// the daemon would have started with, without starting it.
func runFlags(t *testing.T, args ...string) config {
	t.Helper()

	var got config

	run := newRunCommand()
	run.Action = func(_ context.Context, cmd *cli.Command) error {
		got = configFromCommand(cmd)

		return nil
	}

	app := &cli.Command{Name: "ic-healthd", Commands: []*cli.Command{run}}
	app.Writer = t.Output()
	app.ErrWriter = t.Output()

	require.NoError(t, app.Run(t.Context(), append([]string{"ic-healthd", "run"}, args...)))

	return got
}

// TestConfigFromFlags pins every flag to the field it fills.
func TestConfigFromFlags(t *testing.T) {
	t.Parallel()

	got := runFlags(t,
		"--incus", "https://incus.example:8443",
		"--token", "a-token",
		"--project", "one",
		"--project", "two",
		"--own-project", "sidecars",
		"--own-name", "my-ic-healthd",
		"--data-dir", "/data",
		"--secrets-dir", "/secrets",
		"--workers", "9",
		"--restart-workers", "3",
	)

	require.Equal(t, "https://incus.example:8443", got.IncusURL)
	require.Equal(t, "a-token", got.Token)
	require.Equal(t, []string{"one", "two"}, got.Projects)
	require.Equal(t, "sidecars", got.OwnProject)
	require.Equal(t, "my-ic-healthd", got.OwnName)
	require.Equal(t, "/data", got.DataDir)
	require.Equal(t, "/secrets", got.SecretsDir)
	require.Equal(t, 9, got.Workers)
	require.Equal(t, 3, got.RestartWorkers)
}

// TestConfigDefaults pins the two flags that have one.
func TestConfigDefaults(t *testing.T) {
	t.Parallel()

	got := runFlags(t)

	require.Equal(t, defaultDataDir, got.DataDir)
	require.Equal(t, defaultSecretsDir, got.SecretsDir)
	require.Equal(t, defaultWorkers, got.Workers)
	require.Equal(t, defaultRestartWorkers, got.RestartWorkers)
	require.Equal(t, shared.HealthScopeKey, got.ProjectMarker)
	require.Equal(t, shared.HealthScopeGlobal, got.ProjectMarkerValue)
	require.Empty(t, got.Projects)
}

// TestProjectMarker pins the KEY=VALUE form the shared daemon is created with.
func TestProjectMarker(t *testing.T) {
	t.Parallel()

	tests := []struct {
		marker string
		key    string
		value  string
	}{
		{marker: "user.healthcheck.scope=global", key: "user.healthcheck.scope", value: "global"},
		{marker: "user.healthcheck.enabled", key: "user.healthcheck.enabled", value: "true"},
		{marker: "user.mine=", key: "user.mine", value: ""},
		{marker: "user.mine=a=b", key: "user.mine", value: "a=b"},
		{marker: "", key: "", value: "true"},
	}

	for _, tt := range tests {
		t.Run(tt.marker, func(t *testing.T) {
			t.Parallel()

			key, value := parseProjectMarker(tt.marker)
			require.Equal(t, tt.key, key)
			require.Equal(t, tt.value, value)
		})
	}

	got := runFlags(t, "--project-marker", "user.mine=yes")
	require.Equal(t, "user.mine", got.ProjectMarker)
	require.Equal(t, "yes", got.ProjectMarkerValue)
}

// TestConfigFromEnvironment pins the names incus-compose sets on the sidecar.
// A typo here is invisible until a deployment silently ignores its config.
func TestConfigFromEnvironment(t *testing.T) {
	env := map[string]string{
		"INCUS_COMPOSE_HEALTHD_INCUS":           "https://from-env:8443",
		"INCUS_COMPOSE_HEALTHD_TOKEN":           "env-token",
		"INCUS_COMPOSE_HEALTHD_PROJECTS":        "alpha",
		"INCUS_COMPOSE_HEALTHD_OWN_PROJECT":     "env-sidecars",
		"INCUS_COMPOSE_HEALTHD_OWN_NAME":        "env-healthd",
		"INCUS_COMPOSE_HEALTHD_DATA_DIR":        "/env/data",
		"INCUS_COMPOSE_HEALTHD_SECRETS_DIR":     "/env/secrets",
		"INCUS_COMPOSE_HEALTHD_WORKERS":         "11",
		"INCUS_COMPOSE_HEALTHD_RESTART_WORKERS": "4",
	}

	for k, v := range env {
		t.Setenv(k, v)
	}

	got := runFlags(t)

	require.Equal(t, "https://from-env:8443", got.IncusURL)
	require.Equal(t, "env-token", got.Token)
	require.Equal(t, []string{"alpha"}, got.Projects)
	require.Equal(t, "env-sidecars", got.OwnProject)
	require.Equal(t, "env-healthd", got.OwnName)
	require.Equal(t, "/env/data", got.DataDir)
	require.Equal(t, "/env/secrets", got.SecretsDir)
	require.Equal(t, 11, got.Workers)
	require.Equal(t, 4, got.RestartWorkers)
}

// TestVersionCommand walks the real command tree, which is the only thing that
// proves the flags and subcommands are wired together at all.
func TestVersionCommand(t *testing.T) {
	t.Parallel()

	app := newRootCommand()
	app.Writer = t.Output()
	app.ErrWriter = t.Output()

	require.NoError(t, app.Run(t.Context(), []string{"ic-healthd", "version"}))
}

// TestConnectRefusesWithoutCredentials covers the paths that fail before any
// network call: a daemon that cannot authenticate must say so at once.
func TestConnectRefusesWithoutCredentials(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		setup func(t *testing.T, cfg *config)
		want  string
	}{
		{
			name:  "no token and nothing persisted",
			setup: func(_ *testing.T, _ *config) {},
			want:  "no token and no registration happened before",
		},
		{
			name: "a cert without its key",
			setup: func(t *testing.T, cfg *config) {
				t.Helper()
				require.NoError(t, os.WriteFile(filepath.Join(cfg.DataDir, certFile), []byte("cert"), 0o600))
			},
			want: "no token and no registration happened before",
		},
		{
			name: "an empty token file",
			setup: func(t *testing.T, cfg *config) {
				t.Helper()
				require.NoError(t, os.WriteFile(filepath.Join(cfg.SecretsDir, tokenFile), []byte("  \n"), 0o600))
			},
			want: "token file is empty",
		},
		{
			name: "an unreadable token file",
			setup: func(t *testing.T, cfg *config) {
				t.Helper()
				require.NoError(t, os.Mkdir(filepath.Join(cfg.SecretsDir, tokenFile), 0o700))
			},
			want: "reading token",
		},
		{
			name: "an unreadable cert",
			setup: func(t *testing.T, cfg *config) {
				t.Helper()
				require.NoError(t, os.Mkdir(filepath.Join(cfg.DataDir, certFile), 0o700))
				require.NoError(t, os.WriteFile(filepath.Join(cfg.DataDir, keyFile), []byte("key"), 0o600))
			},
			want: "reading cert",
		},
		{
			name: "an unreadable key",
			setup: func(t *testing.T, cfg *config) {
				t.Helper()
				require.NoError(t, os.WriteFile(filepath.Join(cfg.DataDir, certFile), []byte("cert"), 0o600))
				require.NoError(t, os.Mkdir(filepath.Join(cfg.DataDir, keyFile), 0o700))
			},
			want: "reading key",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			cfg := config{DataDir: t.TempDir(), SecretsDir: t.TempDir()}
			tt.setup(t, &cfg)

			_, err := connect(t.Context(), cfg)

			require.ErrorContains(t, err, tt.want)
		})
	}
}

// TestProjectSchedulerSurvivesADeadServer pins that an unreachable Incus makes
// discovery retry rather than take the scheduler down, and that ctx still stops it.
func TestProjectSchedulerSurvivesADeadServer(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())

	done := make(chan struct{})

	go func() {
		defer close(done)

		pool, err := newPools(defaultWorkers, defaultRestartWorkers)
		require.NoError(t, err)
		defer releasePools(pool)

		projectScheduler(ctx, refusedConn(t), pool, "p", make(chan instanceEvent))
	}()

	// Long enough for the first discovery to fail and a retry to elapse.
	time.Sleep(1200 * time.Millisecond)

	select {
	case <-done:
		t.Fatal("the scheduler must keep running while discovery fails")
	default:
	}

	cancel()

	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("projectScheduler did not return after its context was canceled")
	}
}

// TestDiscoverProjectSelectsWatchableInstances pins the startup sweep, which is
// the only thing that finds instances that were already up.
func TestDiscoverProjectSelectsWatchableInstances(t *testing.T) {
	t.Parallel()
	testlib.SkipLocal(t)

	c := testProject(t, "healthd-discover-")

	watched := testContainer(t, c, "watched", healthKeys(map[string]string{
		"test": `["CMD","/bin/true"]`,
	}), false)
	restartOnly := testContainer(t, c, "restart-only", healthKeys(map[string]string{
		"restart": "always",
	}), false)
	testContainer(t, c, "ignored", healthKeys(map[string]string{
		"test":   `["CMD","/bin/true"]`,
		"ignore": "true",
	}), false)
	testContainer(t, c, "plain", nil, false)

	results := make(chan instanceResult, 32)
	discoverProject(t.Context(), testConn(t, c), results)

	got := map[string]error{}

	var roster instanceResult

	// The roster closes the pass, so it is also how the test knows it is over.
	for roster.kind == "" {
		select {
		case res := <-results:
			if res.kind == instanceResultRoster {
				roster = res
				continue
			}

			require.Equal(t, instanceResultDiscovered, res.kind)
			got[res.name] = res.err
		case <-time.After(30 * time.Second):
			t.Fatalf("only %d of 4 instances were reported", len(got))
		}
	}

	require.NoError(t, roster.err)

	require.NoError(t, got[watched])
	require.NoError(t, got[restartOnly], "a restart policy alone is worth watching")
	require.ErrorIs(t, got["ignored"], ErrInstanceIgnored)
	require.ErrorIs(t, got["plain"], ErrInstanceNoHealthcheck)

	require.ElementsMatch(t, []string{watched, restartOnly, "ignored", "plain"}, roster.names,
		"the roster must name every instance, watchable or not: it is what prunes the map")
}
