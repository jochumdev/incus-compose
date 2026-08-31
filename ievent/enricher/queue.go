package enricher

import (
	"iter"

	"github.com/lxc/incus-compose/ievent/iutil"
)

// minRing is the smallest the ring is allocated or shrunk to. Two, so growing
// is always a doubling and shrinking always a halving.
const minRing = 2

// queue keeps arrival order across reads that finish in any order: an event goes
// when its own read has landed and everything ahead of it has already gone. A
// ring rather than a slice reslicing forward, so the array is reused.
type queue struct {
	items []*item

	// head is the next event to leave, tail the next free slot. Both wrap, so
	// count is what tells a full ring from an empty one.
	head  int
	tail  int
	count int
}

// item is one event's place in the line. A caller holds the pointer across the
// read, so a resize moving entries between arrays cannot invalidate it.
type item struct {
	ev *iutil.Event

	// settled says this one may go. An event that needed no read is settled on
	// arrival, so it keeps its place instead of overtaking.
	settled bool

	// skip drops this one on the way out, keeping its place until then.
	skip bool
}

// push puts one event at the back of the line, settled when it needs no read.
func (q *queue) push(ev *iutil.Event, settled bool) *item {
	if q.count == len(q.items) {
		q.resize(max(len(q.items)*2, minRing))
	}

	it := &item{ev: ev, settled: settled}

	q.items[q.tail] = it
	q.tail = (q.tail + 1) % len(q.items)
	q.count++

	return it
}

// settle marks one item finished. The event is replaced rather than merged,
// because enrichment and failure both derive a new one.
func (q *queue) settle(it *item, ev *iutil.Event) {
	it.ev = ev
	it.settled = true
}

// trash finishes one item with nothing to hand on. It keeps its place, or an
// event after it that needs no read would overtake the ones still waiting.
func (q *queue) trash(it *item) {
	it.ev = nil
	it.settled = true
	it.skip = true
}

// release takes every event at the front that is ready, and stops at the first
// that is not - so a line full of finished reads may hand back nothing.
func (q *queue) release() iter.Seq[*iutil.Event] { return q.take(false) }

// drain takes everything left, settled or not: what shutdown hands on. One
// still waiting on a read goes as it stands rather than being swallowed.
func (q *queue) drain() iter.Seq[*iutil.Event] { return q.take(true) }

// take walks the front of the line, stopping at the first event still waiting
// on a read unless everything is being taken.
func (q *queue) take(all bool) iter.Seq[*iutil.Event] {
	return func(yield func(*iutil.Event) bool) {
		for q.count > 0 {
			it := q.items[q.head]
			if !all && !it.settled {
				return
			}

			ev := it.ev
			skip := it.skip

			// The slot is cleared, so an event that has gone is not kept alive
			// by a ring that has not been reused yet.
			q.items[q.head] = nil
			q.head = (q.head + 1) % len(q.items)
			q.count--

			// A quarter full is the trigger and a half the target, so a ring
			// that has just shrunk is half full and not about to grow straight
			// back.
			half := len(q.items) / 2
			if half >= minRing && q.count <= half/2 {
				q.resize(half)
			}

			if skip {
				continue
			}

			if !yield(ev) {
				return
			}
		}
	}
}

// resize moves the line into an array of n, starting at index 0. The entries
// are either contiguous from head to tail, or they wrap past the end.
func (q *queue) resize(n int) {
	items := make([]*item, n)

	switch {
	case q.count == 0:

	case q.head < q.tail:
		copy(items, q.items[q.head:q.tail])

	default:
		// Wrapped: the end of the array first, then what continued at its start.
		copy(items, q.items[q.head:])
		copy(items[len(q.items)-q.head:], q.items[:q.tail])
	}

	q.items = items
	q.head = 0
	q.tail = q.count % n
}
