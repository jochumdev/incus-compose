package log

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lxc/incus-compose/ievent/iutil"
)

// setup wires a log to a successor that keeps what reached it.
func setup(t *testing.T, at string, opts ...Option) (*Plugin, *[]*iutil.Event) {
	t.Helper()

	var seen []*iutil.Event

	p := New(append([]Option{At(at)}, opts...)...)

	err := p.Setup(iutil.SetupArgs{
		Context: t.Context(),
		Next:    func(ev *iutil.Event) { seen = append(seen, ev) },
	})
	require.NoError(t, err)

	return p, &seen
}

// TestNameIsThePosition pins two of these in one chain being told apart. The
// same event walks every position, so a line that cannot say where it came from
// says half of what it is there for.
func TestNameIsThePosition(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "log/arrival", New(At("arrival")).Name())

	// The right answer for a chain with only one of them in it.
	assert.Equal(t, "log", New().Name())
}

// TestHandlePassesEverythingOn pins the one thing a log must never do, which is
// decide. Drops and failures travel the chain so the observers behind can see
// them, and an observer that swallowed one would be the reason they stopped.
func TestHandlePassesEverythingOn(t *testing.T) {
	t.Parallel()

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
				WithFailed("source/read"),
		},
		{
			// The source's own actions carry no project and no name, which is
			// the case the attribute list leaves fields out for.
			name: "one of the source's own",
			ev:   iutil.NewEvent(time.Now(), iutil.ActionSweepStart, "", "", ""),
		},
		{
			name: "an enriched one",
			ev: iutil.NewEvent(time.Now(), "instance-started", "shop", "web", "").
				WithInstance(true, map[string]string{}, map[string]*iutil.Network{}),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			p, seen := setup(t, "served")

			p.Handle(tc.ev)

			// The same event, not a derived one: an observer that rewrote what
			// it saw would change what everything behind it acts on.
			require.Len(t, *seen, 1)
			assert.Same(t, tc.ev, (*seen)[0])
		})
	}
}

// capture is a handler that keeps the level of every record. Enabled is always
// true, so what this test reads is the position's own choice rather than a
// threshold the handler applied on top of it.
type capture struct{ levels []slog.Level }

func (c *capture) Enabled(context.Context, slog.Level) bool { return true }

func (c *capture) Handle(_ context.Context, r slog.Record) error {
	c.levels = append(c.levels, r.Level)

	return nil
}

func (c *capture) WithAttrs([]slog.Attr) slog.Handler { return c }
func (c *capture) WithGroup(string) slog.Handler      { return c }

// TestLevel pins what a position prints at, which is how the same plugin is a
// line per event in front of debounce and nothing at all in a production chain.
// Not t.Parallel(): Handle logs through the process-wide slog default, which
// every case here swaps out - it must not race a sibling test's own Handle
// call using that same default.
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
			ev:   event().WithFailed("source/read"),
			want: slog.LevelWarn,
		},
		{
			// The one line worth keeping, on the position that was quietened.
			name: "and stays Warn however quiet the position is",
			opts: []Option{Level(slog.LevelDebug.String())},
			ev:   event().WithFailed("source/read"),
			want: slog.LevelWarn,
		},
		{
			// Warn is a floor, not the answer: a position asked for louder gets it.
			name: "a position above Warn prints a failure at its own level",
			opts: []Option{Level(slog.LevelError.String())},
			ev:   event().WithFailed("source/read"),
			want: slog.LevelError,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Not t.Parallel(): every case swaps the process-wide slog default,
			// which two cases running at once would step on.
			p, seen := setup(t, "served", tc.opts...)

			// Swapped after New, which announces the position on the default
			// logger: this handler is for what Handle prints and nothing else.
			c := &capture{}

			restore := slog.Default()
			slog.SetDefault(slog.New(c))

			t.Cleanup(func() { slog.SetDefault(restore) })

			p.Handle(tc.ev)

			require.Len(t, c.levels, 1)
			assert.Equal(t, tc.want, c.levels[0])

			// The level decides how it is printed, never whether it walks on.
			assert.Len(t, *seen, 1)
		})
	}
}
