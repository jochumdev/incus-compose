package main

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	incusApi "github.com/lxc/incus/v7/shared/api"
	"github.com/stretchr/testify/require"
	"github.com/urfave/cli/v3"

	"github.com/lxc/incus-compose/ievent/iutil"
)

func TestParseMarker(t *testing.T) {
	t.Parallel()

	key, value := parseMarker("user.healthcheck.scope=global")
	require.Equal(t, "user.healthcheck.scope", key)
	require.Equal(t, "global", value)

	// A bare key means "true".
	key, value = parseMarker("user.mine")
	require.Equal(t, "user.mine", key)
	require.Equal(t, "true", value)

	// An empty marker keys on nothing.
	key, value = parseMarker("")
	require.Equal(t, "", key)
	require.Equal(t, "true", value)
}

// parse drives the real flag set with an argv, so this tests the command line
// rather than a struct literal.
func parse(t *testing.T, argv ...string) *config {
	t.Helper()

	got := newConfig()

	cmd := runCommand(got)
	cmd.Action = func(_ context.Context, c *cli.Command) error {
		return nil
	}

	err := cmd.Run(t.Context(), append([]string{"run"}, argv...))
	require.NoError(t, err)

	return got
}

func TestConfigFromCommand(t *testing.T) {
	t.Parallel()

	cfg := parse(t,
		"--incus", "https://10.0.0.1:8443",
		"--token", "secret",
		"--project", "one", "--project", "two",
		"--project-marker", "user.mine=yes",
		"--own-project", "self",
		"--own-name", "ic-healthd",
		"--workers", "7",
		"--restart-workers", "3",
		"--http-address", ":9000",
		"--metrics=false",
		"--debug",
	)

	require.Equal(t, "https://10.0.0.1:8443", cfg.IncusURL)
	require.Equal(t, "secret", cfg.Token)
	require.Equal(t, []string{"one", "two"}, cfg.Projects)
	require.Equal(t, "user.mine", cfg.ProjectMarker)
	require.Equal(t, "yes", cfg.ProjectMarkerValue)
	require.Equal(t, "self", cfg.OwnProject)
	require.Equal(t, "ic-healthd", cfg.OwnName)
	require.Equal(t, 7, cfg.Workers)
	require.Equal(t, 3, cfg.RestartWorkers)
	require.Equal(t, ":9000", cfg.HTTPAddr)
	require.False(t, cfg.Metrics)
	require.True(t, cfg.Debug)
	require.False(t, cfg.Trace)

	cfg = parse(t, "--project-marker", "user.mine")
	require.Equal(t, "user.mine", cfg.ProjectMarker)
	require.Equal(t, "true", cfg.ProjectMarkerValue)

	cfg = parse(t, "--project-marker", "")
	require.Equal(t, "", cfg.ProjectMarker)
	require.Equal(t, "true", cfg.ProjectMarkerValue)
}

func TestConfigFromEnvironment(t *testing.T) {
	t.Setenv("INCUS_COMPOSE_HEALTHD_INCUS", "https://env:8443")
	t.Setenv("INCUS_COMPOSE_HEALTHD_HTTP_ADDRESS", ":9153")
	t.Setenv("INCUS_COMPOSE_HEALTHD_WORKERS", "16")
	t.Setenv("INCUS_COMPOSE_HEALTHD_DEBUG", "true")

	cfg := parse(t)

	require.Equal(t, "https://env:8443", cfg.IncusURL)
	require.Equal(t, ":9153", cfg.HTTPAddr)
	require.Equal(t, 16, cfg.Workers)
	require.True(t, cfg.Debug)

	// The flag still wins over the environment.
	cfg = parse(t, "--http-address", ":9000")
	require.Equal(t, ":9000", cfg.HTTPAddr)

	t.Setenv("INCUS_COMPOSE_HEALTHD_PROJECT_MARKER", "user.custom=val")
	cfg = parse(t)
	require.Equal(t, "user.custom", cfg.ProjectMarker)
	require.Equal(t, "val", cfg.ProjectMarkerValue)

	t.Setenv("INCUS_COMPOSE_HEALTHD_PROJECT_MARKER", "user.mine")
	cfg = parse(t)
	require.Equal(t, "user.mine", cfg.ProjectMarker)
	require.Equal(t, "true", cfg.ProjectMarkerValue)

	t.Setenv("INCUS_COMPOSE_HEALTHD_PROJECT_MARKER", "")
	cfg = parse(t)
	require.Equal(t, "", cfg.ProjectMarker)
	require.Equal(t, "true", cfg.ProjectMarkerValue)

	t.Setenv("INCUS_COMPOSE_HEALTHD_PROJECT_MARKER", "user.env=fromenv")
	cfg = parse(t, "--project-marker", "user.cli=fromcli")
	require.Equal(t, "user.cli", cfg.ProjectMarker)
	require.Equal(t, "fromcli", cfg.ProjectMarkerValue)
}

func TestConfigDefaults(t *testing.T) {
	t.Parallel()

	cfg := parse(t)

	require.Equal(t, defaultDataDir, cfg.DataDir)
	require.Equal(t, defaultSecretsDir, cfg.SecretsDir)
	require.Equal(t, defaultHTTPAddr, cfg.HTTPAddr)
	require.True(t, cfg.Metrics)
	require.Equal(t, defaultWorkers, cfg.Workers)
	require.Equal(t, defaultRestartWorkers, cfg.RestartWorkers)
	require.Equal(t, "user.healthcheck.scope", cfg.ProjectMarker)
	require.Equal(t, "global", cfg.ProjectMarkerValue)
}

// TestServes pins the enricher's half of the scope decision.
func TestServes(t *testing.T) {
	t.Parallel()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	project := &incusApi.Project{Name: "blog"}
	project.Config = incusApi.ConfigMap{"user.healthcheck.scope": "global"}

	// An explicit list wins over any marker.
	cfg := &config{Projects: []string{"blog"}, ProjectMarker: "user.mine", ProjectMarkerValue: "true"}
	require.True(t, serves(logger, cfg)(project))
	require.False(t, serves(logger, cfg)(&incusApi.Project{Name: "shop"}))

	// No marker means every project the certificate can see.
	cfg = &config{}
	require.Nil(t, serves(logger, cfg))

	// The marker, key and value together.
	cfg = &config{ProjectMarker: "user.healthcheck.scope", ProjectMarkerValue: "global"}
	require.True(t, serves(logger, cfg)(project))

	other := &incusApi.Project{Name: "shop"}
	other.Config = incusApi.ConfigMap{"user.healthcheck.scope": "project"}
	require.False(t, serves(logger, cfg)(other))

	// Default config serves marked projects.
	require.True(t, serves(logger, newConfig())(project))
	require.False(t, serves(logger, newConfig())(other))
	require.False(t, serves(logger, newConfig())(&incusApi.Project{Name: "unmarked"}))

	// Bare marker key.
	bare := &incusApi.Project{Name: "bare"}
	bare.Config = incusApi.ConfigMap{"user.mine": "true"}
	cfg = newConfig()
	cfg.ProjectMarker = "user.mine"
	cfg.ProjectMarkerValue = "true"
	require.True(t, serves(logger, cfg)(bare))
	require.False(t, serves(logger, cfg)(project))

	// Empty marker watches every project the certificate can see.
	cfg = newConfig()
	cfg.ProjectMarker = ""
	require.Nil(t, serves(logger, cfg))
}

// TestServeable pins the checker's half of the scope decision: the same policy,
// answered per event.
func TestServeable(t *testing.T) {
	t.Parallel()

	ev := func(project string, config map[string]string) *iutil.Event {
		out := iutil.NewEvent(time.Now(), incusApi.EventLifecycleInstanceUpdated, project, "web", "")
		if config != nil {
			out = out.WithProject(iutil.NewProject(config))
		}

		return out
	}

	cfg := newConfig()
	cfg.Projects = []string{"blog"}
	// An explicit list checks the event's project, nothing else.
	fn := serveable(cfg)
	require.True(t, fn(ev("blog", nil)))
	require.False(t, fn(ev("shop", nil)))

	// No marker means every project the certificate can see.
	require.Nil(t, serveable(&config{}))

	// The marker reads what the enricher put on the event.
	fn = serveable(newConfig())
	require.True(t, fn(ev("blog", map[string]string{"user.healthcheck.scope": "global"})))
	require.False(t, fn(ev("blog", map[string]string{"user.healthcheck.scope": "project"})))

	// A project the enricher holds nothing for is not watched: believing a
	// missing read would watch what was never opted in.
	require.False(t, fn(ev("blog", nil)))

	// Bare marker key.
	cfg = newConfig()
	cfg.ProjectMarker = "user.mine"
	cfg.ProjectMarkerValue = "true"
	fn = serveable(cfg)
	require.True(t, fn(ev("blog", map[string]string{"user.mine": "true"})))
	require.False(t, fn(ev("blog", map[string]string{"user.mine": "false"})))

	// Empty marker watches every project.
	cfg = newConfig()
	cfg.ProjectMarker = ""
	require.Nil(t, serveable(cfg))
}
