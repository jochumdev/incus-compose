package log

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lxc/incus-compose/ievent/iutil"
)

// setup wires a log to a successor that keeps what reached it.
func setup(t *testing.T, logger *slog.Logger, at string, opts ...Option) (*Plugin, *[]*iutil.Event) {
	t.Helper()

	var seen []*iutil.Event

	p := New(logger, append([]Option{At(at)}, opts...)...)

	err := p.Setup(iutil.SetupArgs{
		Next: func(ev *iutil.Event) { seen = append(seen, ev) },
	})
	require.NoError(t, err)

	return p, &seen
}

// TestNameIsThePosition pins two of these in one chain being told apart.
func TestNameIsThePosition(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "log/arrival", New(slog.Default(), At("arrival")).Name())
	assert.Equal(t, "log", New(slog.Default()).Name())
}

// TestHandlePassesEverythingOn pins the one thing a log must never do, which is decide.
func TestHandlePassesEverythingOn(t *testing.T) {
	t.Parallel()

	inst := iutil.NewInstance(true, map[string]string{"user.label.a": "1"},
		[]iutil.InstanceInterface{
			iutil.NewInstanceInterface("p", "net0", true, []string{"10.0.0.2"}, nil),
		},
		map[string]*iutil.Network{
			iutil.NetworkKey("p", "net0"): iutil.NewNetwork("net0", "p", true, "10.0.0.1/24", ""),
		})

	cases := []struct {
		name string
		ev   *iutil.Event
	}{
		{
			name: "an ordinary event",
			ev:   iutil.NewEvent(time.Now(), "instance-started", "shop", "web", ""),
		},
		{
			name: "one somebody dropped",
			ev: iutil.NewEvent(time.Now(), "instance-updated", "shop", "web", "").
				WithDropped("debounce"),
		},
		{
			name: "one that failed a read",
			ev: iutil.NewEvent(time.Now(), "instance-started", "shop", "web", "").
				WithFailed(errors.New("source/read")),
		},
		{
			// The source's own actions carry no project and no name.
			name: "one of the source's own",
			ev:   iutil.NewEvent(time.Now(), iutil.ActionConnected, "", "", ""),
		},
		{
			name: "an enriched one",
			ev: iutil.NewEvent(time.Now(), "instance-started", "shop", "web", "").
				WithInstance(inst, true),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			p, seen := setup(t, slog.Default(), "served")

			p.Handle(tc.ev)

			// The same event, not a derived one.
			require.Len(t, *seen, 1)
			assert.Same(t, tc.ev, (*seen)[0])
		})
	}
}

// capture is a handler that keeps the level of every record; Enabled is always true.
type capture struct{ levels []slog.Level }

func (c *capture) Enabled(context.Context, slog.Level) bool { return true }

func (c *capture) Handle(_ context.Context, r slog.Record) error {
	c.levels = append(c.levels, r.Level)

	return nil
}

func (c *capture) WithAttrs([]slog.Attr) slog.Handler { return c }
func (c *capture) WithGroup(string) slog.Handler      { return c }

// TestLevel pins what a position prints at.
func TestLevel(t *testing.T) {
	event := func() *iutil.Event {
		return iutil.NewEvent(time.Now(), "instance-started", "shop", "web", "")
	}

	cases := []struct {
		name string
		opts []Option
		ev   *iutil.Event
		want slog.Level
	}{
		{
			name: "a routine event is Debug when nothing was said",
			ev:   event(),
			want: slog.LevelDebug,
		},
		{
			// Dropped is routine: the chain took it out on purpose.
			name: "and so is one somebody dropped",
			ev:   event().WithDropped("debounce"),
			want: slog.LevelDebug,
		},
		{
			name: "a loud position prints the walk at what it was given",
			opts: []Option{Level(slog.LevelInfo.String())},
			ev:   event(),
			want: slog.LevelInfo,
		},
		{
			name: "a failed event is Warn when nothing was said",
			ev:   event().WithFailed(errors.New("source/read")),
			want: slog.LevelWarn,
		},
		{
			// The one line worth keeping, on the position that was quietened.
			name: "and stays Warn however quiet the position is",
			opts: []Option{Level(slog.LevelDebug.String())},
			ev:   event().WithFailed(errors.New("source/read")),
			want: slog.LevelWarn,
		},
		{
			// Warn is a floor, not the answer: a position asked for louder gets it.
			name: "a position above Warn prints a failure at its own level",
			opts: []Option{Level(slog.LevelError.String())},
			ev:   event().WithFailed(errors.New("source/read")),
			want: slog.LevelError,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := &capture{}
			p, seen := setup(t, slog.New(c), "served", tc.opts...)

			// New logs the position on the same logger; drop it, so only the event is judged.
			c.levels = nil

			p.Handle(tc.ev)

			require.Len(t, c.levels, 1)
			assert.Equal(t, tc.want, c.levels[0])

			// The level decides how it is printed, never whether it walks on.
			assert.Len(t, *seen, 1)
		})
	}
}
