package main

import (
	"context"
	"log/slog"
	"slices"
	"testing"

	incusapi "github.com/lxc/incus/v7/shared/api"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lxc/incus-compose/ievent/iutil"
	"github.com/lxc/incus-compose/ievent/log"
)

// positions is a chain shaped like the real one: two observers and something
// between them that may not go.
func positions() []position {
	return []position{
		{plugin: log.New(slog.Default(), log.At("arrival")), optional: true},
		{plugin: log.New(slog.Default(), log.At("enricher")), optional: false},
		{plugin: log.New(slog.Default(), log.At("served")), optional: true},
	}
}

func names(plugins []iutil.Plugin) []string {
	out := make([]string, 0, len(plugins))

	for _, p := range plugins {
		out = append(out, p.Name())
	}

	return out
}

// TestChainLogPositionsNeedALevel pins what --log decides: empty leaves every
// log position out of the chain rather than quieting it, because a position that
// is there costs a line's worth of work per event whatever the level.
func TestChainLogPositionsNeedALevel(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name  string
		cfg   config
		wants []string
	}{
		{name: "empty leaves them all out", cfg: config{}},
		{
			name:  "a level puts the unconditional ones in",
			cfg:   config{Log: "DEBUG"},
			wants: []string{"log/enriched", "log/served"},
		},
		{
			name:  "trace adds the pair around debounce",
			cfg:   config{Log: "TRACE"},
			wants: []string{"log/arrival", "log/received", "log/enriched", "log/served"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			composed := chain(slog.Default(), tc.cfg)

			plugins := make([]iutil.Plugin, 0, len(composed))
			for _, p := range composed {
				plugins = append(plugins, p.plugin)
			}

			got := names(plugins)

			var logs []string

			for _, at := range []string{"log/arrival", "log/received", "log/enriched", "log/served"} {
				if slices.Contains(got, at) {
					logs = append(logs, at)
				}
			}

			assert.Equal(t, tc.wants, logs)
		})
	}
}

func TestServes(t *testing.T) {
	t.Parallel()

	logger := slog.Default()

	pGlobal := &incusapi.Project{
		Name: "p-global",
		ProjectPut: incusapi.ProjectPut{
			Config: map[string]string{
				"user.label.dns.scope": "global",
			},
		},
	}
	pCustom := &incusapi.Project{
		Name: "p-custom",
		ProjectPut: incusapi.ProjectPut{
			Config: map[string]string{
				"user.label.dns.scope": "project",
			},
		},
	}
	pBare := &incusapi.Project{
		Name: "p-bare",
		ProjectPut: incusapi.ProjectPut{
			Config: map[string]string{
				"user.dns": "true",
			},
		},
	}
	pUnmarked := &incusapi.Project{
		Name: "p-unmarked",
	}

	t.Run("defaults serve marked projects", func(t *testing.T) {
		cfg := *newConfig()
		filter := serves(logger, cfg)
		require.NotNil(t, filter)

		assert.True(t, filter(pGlobal))
		assert.False(t, filter(pCustom))
		assert.False(t, filter(pUnmarked))
	})

	t.Run("explicit projects list wins over marker", func(t *testing.T) {
		cfg := *newConfig()
		cfg.Projects = []string{"p-unmarked", "p-custom"}
		filter := serves(logger, cfg)
		require.NotNil(t, filter)

		assert.True(t, filter(pUnmarked))
		assert.True(t, filter(pCustom))
		assert.False(t, filter(pGlobal))
	})

	t.Run("custom marker", func(t *testing.T) {
		cfg := *newConfig()
		cfg.ProjectMarker = "user.label.dns.scope"
		cfg.ProjectMarkerValue = "project"
		filter := serves(logger, cfg)
		require.NotNil(t, filter)

		assert.True(t, filter(pCustom))
		assert.False(t, filter(pGlobal))
		assert.False(t, filter(pUnmarked))
	})

	t.Run("bare marker key", func(t *testing.T) {
		cfg := *newConfig()
		cfg.ProjectMarker = "user.dns"
		cfg.ProjectMarkerValue = "true"
		filter := serves(logger, cfg)
		require.NotNil(t, filter)

		assert.True(t, filter(pBare))
		assert.False(t, filter(pGlobal))
		assert.False(t, filter(pUnmarked))
	})

	t.Run("empty marker serves all projects", func(t *testing.T) {
		cfg := *newConfig()
		cfg.ProjectMarker = ""
		filter := serves(logger, cfg)
		assert.Nil(t, filter)
	})
}

func TestAssemble(t *testing.T) {
	cases := []struct {
		name    string
		exclude []string

		want    []string
		wantErr string
	}{
		{
			name: "nothing excluded is the whole chain, in order",
			want: []string{"log/arrival", "log/enricher", "log/served"},
		},
		{
			name:    "an optional position goes",
			exclude: []string{"log/arrival"},
			want:    []string{"log/enricher", "log/served"},
		},
		{
			name:    "and so do all of them",
			exclude: []string{"log/arrival", "log/served"},
			want:    []string{"log/enricher"},
		},
		{
			name:    "a position that may not go is refused",
			exclude: []string{"log/enricher"},
			wantErr: `cannot exclude "log/enricher"; this binary allows log/arrival, log/served`,
		},
		{
			// A typo is otherwise a position that silently stayed in.
			name:    "so is a position this binary has never heard of",
			exclude: []string{"log/nowhere"},
			wantErr: `cannot exclude "log/nowhere"; this binary allows log/arrival, log/served`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			plugins, runners, err := assemble(positions(), tc.exclude)

			if tc.wantErr != "" {
				require.Error(t, err)
				assert.Equal(t, tc.wantErr, err.Error())

				// Nothing half-built comes back with the error.
				assert.Nil(t, plugins)
				assert.Nil(t, runners)

				return
			}

			require.NoError(t, err)
			assert.Equal(t, tc.want, names(plugins))

			// log has no Run, so nothing here is main's to start.
			assert.Empty(t, runners)
		})
	}
}

// TestAssembleFindsRunners pins that excluding a plugin that owns a goroutine
// takes it out of both lists at once. A runner left behind is a Wait on nothing.
func TestAssembleFindsRunners(t *testing.T) {
	ps := []position{
		{plugin: log.New(slog.Default(), log.At("arrival")), optional: true},
		{plugin: &runnerPlugin{Plugin: log.New(slog.Default(), log.At("worker"))}, optional: true},
	}

	_, runners, err := assemble(ps, nil)
	require.NoError(t, err)
	require.Len(t, runners, 1)
	assert.Equal(t, "log/worker", runners[0].Name())

	plugins, runners, err := assemble(ps, []string{"log/worker"})
	require.NoError(t, err)
	assert.Equal(t, []string{"log/arrival"}, names(plugins))
	assert.Empty(t, runners)
}

// runnerPlugin is a plugin that owns a goroutine, which log does not.
type runnerPlugin struct{ *log.Plugin }

func (r *runnerPlugin) Run(ctx context.Context) error {
	<-ctx.Done()

	return nil
}
