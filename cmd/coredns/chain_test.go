package main

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lxc/incus-compose/ievent/iutil"
	"github.com/lxc/incus-compose/ievent/log"
)

// positions is a chain shaped like the real one: two observers and something
// between them that may not go.
func positions() []position {
	return []position{
		{plugin: log.New(log.At("arrival")), optional: true},
		{plugin: log.New(log.At("enricher")), optional: false},
		{plugin: log.New(log.At("served")), optional: true},
	}
}

func names(plugins []iutil.Plugin) []string {
	out := make([]string, 0, len(plugins))

	for _, p := range plugins {
		out = append(out, p.Name())
	}

	return out
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
		{plugin: log.New(log.At("arrival")), optional: true},
		{plugin: &runnerPlugin{Plugin: log.New(log.At("worker"))}, optional: true},
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
