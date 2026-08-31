package enricher

import (
	"errors"
	"iter"
	"slices"
	"testing"
	"time"

	incusapi "github.com/lxc/incus/v7/shared/api"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lxc/incus-compose/ievent/iutil"
)

// No skip helper on any test here: the queue talks to nothing, so all of it
// runs in every stage.

// event builds one bare event, named so a test can tell them apart in order.
func event(name string) *iutil.Event {
	return iutil.NewEvent(time.Now(), incusapi.EventLifecycleInstanceUpdated, "p", name, "")
}

// names is what came out, in the order it came out. Nil in, nil out, so a table
// can say "nothing was released" and mean it.
func names(seq iter.Seq[*iutil.Event]) []string {
	evs := slices.Collect(seq)
	if len(evs) == 0 {
		return nil
	}

	out := make([]string, len(evs))

	for i, ev := range evs {
		out[i] = ev.Name()
	}

	return out
}

// TestQueueRelease is the ordering contract: what leaves is the order that
// arrived, whatever order the reads landed in.
func TestQueueRelease(t *testing.T) {
	t.Parallel()

	// settleOrder is the order reads land in, by arrival index. An index left
	// out never lands at all, which is a read still in flight.
	tests := []struct {
		name        string
		arrive      []string
		unsettled   []int
		settleOrder []int
		want        []string
		left        int
	}{
		{
			name:   "nothing to read passes straight through",
			arrive: []string{"a", "b", "c"},
			want:   []string{"a", "b", "c"},
		},
		{
			name:        "reads landing backwards still leave forwards",
			arrive:      []string{"a", "b", "c"},
			unsettled:   []int{0, 1, 2},
			settleOrder: []int{2, 1, 0},
			want:        []string{"a", "b", "c"},
		},
		{
			name:        "one unfinished read holds up the finished ones after it",
			arrive:      []string{"a", "b", "c"},
			unsettled:   []int{0, 1, 2},
			settleOrder: []int{1, 2},
			want:        nil,
			left:        3,
		},
		{
			name:        "the front leaving lets everything settled follow at once",
			arrive:      []string{"a", "b", "c"},
			unsettled:   []int{0, 1, 2},
			settleOrder: []int{2, 1, 0},
			want:        []string{"a", "b", "c"},
		},
		{
			name:        "a settled front leaves without waiting for the back",
			arrive:      []string{"a", "b", "c"},
			unsettled:   []int{1, 2},
			settleOrder: []int{},
			want:        []string{"a"},
			left:        2,
		},
		{
			name:        "a read landing out of order releases only up to the gap",
			arrive:      []string{"a", "b", "c", "d"},
			unsettled:   []int{1, 3},
			settleOrder: []int{3},
			want:        []string{"a"},
			left:        3,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			q := &queue{}
			items := make([]*item, len(tc.arrive))

			pending := map[int]bool{}
			for _, i := range tc.unsettled {
				pending[i] = true
			}

			for i, n := range tc.arrive {
				items[i] = q.push(event(n), !pending[i])
			}

			for _, i := range tc.settleOrder {
				// Settled with the event it arrived as: what a read replaces it
				// with is the read's business, not the queue's.
				q.settle(items[i], items[i].ev)
			}

			assert.Equal(t, tc.want, names(q.release()), "released, in order")
			assert.Equal(t, tc.left, q.count, "still in the line")
		})
	}
}

// TestQueueWrapsAndKeepsOrder pushes and pops out of step so head and tail wrap
// past the end of the array repeatedly, which is the case a ring exists to have
// and the one an index-based queue gets wrong.
func TestQueueWrapsAndKeepsOrder(t *testing.T) {
	t.Parallel()

	q := &queue{}

	var want, got []string

	// Pushed steadily and released every third time, so the line is never empty
	// and head and tail wrap past the end of the array at different moments.
	for i := range 64 {
		name := string(rune('a'+i%26)) + string(rune('0'+i/26))
		q.push(event(name), true)
		want = append(want, name)

		if i%3 == 0 {
			got = append(got, names(q.release())...)
		}
	}

	got = append(got, names(q.release())...)

	assert.Equal(t, want, got, "everything came out, in the order it went in")
	assert.Zero(t, q.count)
}

// TestQueueGrowsAndShrinks: the ring doubles when it fills and halves on the
// way out, so a burst does not leave the line permanently the size of the worst
// moment it ever had.
func TestQueueGrowsAndShrinks(t *testing.T) {
	t.Parallel()

	q := &queue{}

	items := make([]*item, 0, 64)
	for range 64 {
		items = append(items, q.push(event("a"), false))
	}

	grown := len(q.items)
	assert.GreaterOrEqual(t, grown, 64, "the ring holds everything pushed into it")

	for _, it := range items {
		q.settle(it, it.ev)
	}

	require.Len(t, slices.Collect(q.release()), 64)

	assert.Zero(t, q.count)
	assert.Less(t, len(q.items), grown, "and gives the room back")
	assert.GreaterOrEqual(t, len(q.items), minRing, "but never below the floor")
}

// TestQueueResizeWhileWrapped grows the ring at the moment its contents wrap,
// which is the branch of resize that is easy to write backwards.
func TestQueueResizeWhileWrapped(t *testing.T) {
	t.Parallel()

	q := &queue{}

	// Fill, take half out, then push enough to wrap past the end and force a
	// grow while the contents are split across it.
	for _, n := range []string{"a", "b", "c", "d"} {
		q.push(event(n), true)
	}

	assert.Equal(t, []string{"a", "b", "c", "d"}, names(q.release()))

	for _, n := range []string{"e", "f", "g", "h", "i", "j"} {
		q.push(event(n), true)
	}

	assert.Equal(t, []string{"e", "f", "g", "h", "i", "j"}, names(q.release()),
		"a resize across the wrap keeps the order")
}

// TestQueueDrain is the shutdown path: everything left goes, settled or not.
func TestQueueDrain(t *testing.T) {
	t.Parallel()

	q := &queue{}

	q.push(event("a"), true)
	q.push(event("b"), false)
	q.push(event("c"), true)

	assert.Equal(t, []string{"a"}, names(q.release()), "only the settled front leaves normally")
	assert.Equal(t, []string{"b", "c"}, names(q.drain()), "the rest go on the way out, in order")
	assert.Zero(t, q.count)
	assert.Nil(t, names(q.drain()), "and an empty line drains to nothing")
}

// TestQueueSettleReplacesTheEvent is what enrichment does: the event that
// leaves is the derived one, not the one that arrived.
func TestQueueSettleReplacesTheEvent(t *testing.T) {
	t.Parallel()

	q := &queue{}

	it := q.push(event("a"), false)
	err := errors.New("source/read")
	q.settle(it, it.ev.WithFailed(err))

	out := slices.Collect(q.release())
	require.Len(t, out, 1)

	assert.ErrorIs(t, out[0].Err(), iutil.ErrFailed, "what settle was handed is what leaves")
	assert.ErrorIs(t, out[0].Err(), err)
}

// TestQueueReleaseIsRepeatable checks the second call does not hand back what
// the first already did.
func TestQueueReleaseIsRepeatable(t *testing.T) {
	t.Parallel()

	q := &queue{}

	q.push(event("a"), true)
	it := q.push(event("b"), false)

	assert.Equal(t, []string{"a"}, names(q.release()))
	assert.Nil(t, names(q.release()), "nothing new while the front is waiting")

	q.settle(it, it.ev)

	assert.Equal(t, []string{"b"}, names(q.release()))
	assert.Zero(t, q.count)
	assert.Nil(t, names(q.release()), "an empty line releases nothing")
}
