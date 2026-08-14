package debounce

import (
	"context"
	"sync"
	"testing"
	"time"

	incusapi "github.com/lxc/incus/v7/shared/api"

	"github.com/lxc/incus-compose/ievent/shared"
)

// No skip helper on any test here: this plugin talks to nothing, so all of it
// runs in every stage.

// wanted is the table the source would have built. Updates may be collapsed; a
// rename may not, which is the real case for the veto - keeping only the last
// of two loses the middle name. instance-started fires once, so it is not worth
// a window either.
var wanted = map[string]shared.Want{
	incusapi.EventLifecycleInstanceUpdated: {Action: incusapi.EventLifecycleInstanceUpdated, Debounce: true},
	incusapi.EventLifecycleNetworkUpdated:  {Action: incusapi.EventLifecycleNetworkUpdated, Debounce: true},
	incusapi.EventLifecycleInstanceRenamed: {Action: incusapi.EventLifecycleInstanceRenamed},
	incusapi.EventLifecycleInstanceStarted: {Action: incusapi.EventLifecycleInstanceStarted},
}

// window is long enough that "not out yet" is not a race on a loaded machine,
// and short enough that the suite stays quick.
const window = 150 * time.Millisecond

// harness wires one plugin to a collecting successor and runs it the way main
// would.
type harness struct {
	t   *testing.T
	p   *Plugin
	out chan *shared.Event

	// ran carries what Run returned, which is also how a test waits for the
	// shutdown flush - there is no second signal.
	ran chan error
}

func newHarness(t *testing.T) *harness {
	t.Helper()

	ctx, cancel := context.WithCancel(t.Context())
	h := &harness{
		t:   t,
		p:   New(Window(window)),
		out: make(chan *shared.Event, 64),
		ran: make(chan error, 1),
	}

	err := h.p.Setup(shared.SetupArgs{
		Context: ctx,
		Wanted:  wanted,
		Next:    func(ev *shared.Event) { h.out <- ev },
	})
	if err != nil {
		t.Fatalf("Setup: %s", err)
	}

	var wg sync.WaitGroup

	wg.Go(func() { h.ran <- h.p.Run(ctx) })

	// Cancel and wait, so a test that leaves a window open does not leak the
	// goroutine into the next one - and so every test asserts Run came back
	// clean.
	t.Cleanup(func() {
		cancel()
		wg.Wait()

		err := <-h.ran
		if err != nil {
			t.Errorf("Run: %s", err)
		}
	})

	return h
}

// send hands one event to Handle, the way the previous plugin would.
func (h *harness) send(action, project, name string) {
	h.t.Helper()

	h.p.Handle(shared.NewEvent(time.Now(), action, project, name, ""))
}

// next waits for one event to come out the far end.
func (h *harness) next() *shared.Event {
	h.t.Helper()

	select {
	case ev := <-h.out:
		return ev
	case <-time.After(5 * window):
		h.t.Fatal("timed out waiting for an event")

		return nil
	}
}

// nextWithin waits for one event and fails if it takes as long as d.
//
// The point of a leading-edge test: next alone cannot tell "handed straight on"
// from "held for the window and then released", because both arrive eventually.
func (h *harness) nextWithin(d time.Duration) *shared.Event {
	h.t.Helper()

	select {
	case ev := <-h.out:
		return ev
	case <-time.After(d):
		h.t.Fatalf("nothing within %s, so it was held", d)

		return nil
	}
}

// nothingYet asserts that nothing comes out within d.
func (h *harness) nothingYet(d time.Duration) {
	h.t.Helper()

	select {
	case ev := <-h.out:
		h.t.Fatalf("released %s/%s early", ev.Project(), ev.Name())
	case <-time.After(d):
	}
}

func assertEvent(t *testing.T, ev *shared.Event, action, name string, state shared.State) {
	t.Helper()

	if ev.Action() != action || ev.Name() != name || ev.State() != state {
		t.Fatalf("got %s %s %s, want %s %s %s",
			ev.Action(), ev.Name(), ev.State(), action, name, state)
	}
}

func TestLoneEventGoesAtOnce(t *testing.T) {
	h := newHarness(t)

	// The whole reason for the leading edge: one change is not a burst, and
	// waiting out the window for it would be pure latency.
	h.send(incusapi.EventLifecycleInstanceUpdated, "p", "one")

	assertEvent(t, h.nextWithin(window/3), incusapi.EventLifecycleInstanceUpdated, "one", shared.StateOk)
}

func TestLoneEventIsNotReportedTwice(t *testing.T) {
	h := newHarness(t)

	h.send(incusapi.EventLifecycleInstanceUpdated, "p", "one")
	h.next()

	// The window still closes, but on nothing: a burst of one was already
	// carried by its leading edge.
	h.nothingYet(2 * window)
}

func TestTwoEventsGiveTheFirstAndTheLast(t *testing.T) {
	h := newHarness(t)

	h.send(incusapi.EventLifecycleInstanceUpdated, "p", "one")
	assertEvent(t, h.nextWithin(window/3), incusapi.EventLifecycleInstanceUpdated, "one", shared.StateOk)

	h.send(incusapi.EventLifecycleInstanceUpdated, "p", "one")

	// The second closes the window rather than riding the leading edge, so it
	// arrives once the key is quiet - and nothing was dropped, because there
	// was nothing between the two.
	h.nothingYet(window / 3)
	assertEvent(t, h.next(), incusapi.EventLifecycleInstanceUpdated, "one", shared.StateOk)
}

func TestStormGivesTheFirstAndTheLastAndDropsTheMiddle(t *testing.T) {
	h := newHarness(t)

	for range 20 {
		h.send(incusapi.EventLifecycleInstanceUpdated, "p", "one")
	}

	// First out is the leading edge, at once and un-dropped.
	assertEvent(t, h.nextWithin(window/3), incusapi.EventLifecycleInstanceUpdated, "one", shared.StateOk)

	// Then everything the last one superseded. Two of the twenty are acted on;
	// the other eighteen still walk, so nothing is invisible.
	for range 18 {
		ev := h.next()
		if ev.State() != shared.StateDropped {
			t.Fatalf("state %s, want dropped", ev.State())
		}

		if ev.Reason() != name {
			t.Fatalf("reason %q, want %q", ev.Reason(), name)
		}
	}

	assertEvent(t, h.next(), incusapi.EventLifecycleInstanceUpdated, "one", shared.StateOk)
}

func TestWindowReopensAfterItCloses(t *testing.T) {
	h := newHarness(t)

	h.send(incusapi.EventLifecycleInstanceUpdated, "p", "one")
	h.next()
	h.nothingYet(2 * window)

	// The window closed on nothing, so the next event is a leading edge again
	// rather than a trailing one.
	h.send(incusapi.EventLifecycleInstanceUpdated, "p", "one")

	assertEvent(t, h.nextWithin(window/3), incusapi.EventLifecycleInstanceUpdated, "one", shared.StateOk)
}

func TestKeysDoNotCollapseIntoEachOther(t *testing.T) {
	h := newHarness(t)

	h.send(incusapi.EventLifecycleInstanceUpdated, "p", "one")
	h.send(incusapi.EventLifecycleInstanceUpdated, "p", "two")

	// Two different instances, so both are leading edges of their own.
	seen := map[string]bool{}
	seen[h.nextWithin(window/3).Name()] = true
	seen[h.nextWithin(window/3).Name()] = true

	if !seen["one"] || !seen["two"] {
		t.Fatalf("got %v, want both one and two", seen)
	}
}

func TestSameNameInTwoProjectsAreTwoKeys(t *testing.T) {
	h := newHarness(t)

	h.send(incusapi.EventLifecycleInstanceUpdated, "a", "one")
	h.send(incusapi.EventLifecycleInstanceUpdated, "b", "one")

	first, second := h.nextWithin(window/3), h.nextWithin(window/3)
	if first.Project() == second.Project() {
		t.Fatalf("both came from %s, want one from each project", first.Project())
	}
}

func TestSweepStartClosesWindowsBeforeTheBracket(t *testing.T) {
	h := newHarness(t)

	h.send(incusapi.EventLifecycleInstanceUpdated, "p", "one")
	h.next()

	// A second one is waiting on the window rather than out already.
	h.send(incusapi.EventLifecycleInstanceUpdated, "p", "one")
	h.nothingYet(window / 4)

	h.send(shared.ActionSweepStart, "", "")

	// The trailing one goes first, so the pass contains it rather than finding
	// it missing and pruning a live record.
	assertEvent(t, h.next(), incusapi.EventLifecycleInstanceUpdated, "one", shared.StateOk)
	assertEvent(t, h.next(), shared.ActionSweepStart, "", shared.StateOk)
}

func TestSweepPassesEverythingThrough(t *testing.T) {
	h := newHarness(t)

	h.send(shared.ActionSweepStart, "", "")
	assertEvent(t, h.next(), shared.ActionSweepStart, "", shared.StateOk)

	// Nothing is collapsed while a pass is running, so a burst arrives whole
	// and nothing in it reads as absent.
	h.send(incusapi.EventLifecycleInstanceUpdated, "p", "one")
	h.send(incusapi.EventLifecycleInstanceUpdated, "p", "one")

	assertEvent(t, h.nextWithin(window/3), incusapi.EventLifecycleInstanceUpdated, "one", shared.StateOk)
	assertEvent(t, h.nextWithin(window/3), incusapi.EventLifecycleInstanceUpdated, "one", shared.StateOk)
}

func TestCollapsingResumesAfterTheSweep(t *testing.T) {
	h := newHarness(t)

	h.send(shared.ActionSweepStart, "", "")
	assertEvent(t, h.next(), shared.ActionSweepStart, "", shared.StateOk)

	h.send(shared.ActionSweepEnd, "", "")
	assertEvent(t, h.next(), shared.ActionSweepEnd, "", shared.StateOk)

	h.send(incusapi.EventLifecycleInstanceUpdated, "p", "one")
	h.send(incusapi.EventLifecycleInstanceUpdated, "p", "one")
	h.send(incusapi.EventLifecycleInstanceUpdated, "p", "one")

	assertEvent(t, h.nextWithin(window/3), incusapi.EventLifecycleInstanceUpdated, "one", shared.StateOk)
	assertEvent(t, h.next(), incusapi.EventLifecycleInstanceUpdated, "one", shared.StateDropped)
	assertEvent(t, h.next(), incusapi.EventLifecycleInstanceUpdated, "one", shared.StateOk)
}

func TestNamelessActionsAreNotHeld(t *testing.T) {
	h := newHarness(t)

	// The source's own carry no project and no name, so there is nothing to
	// collapse and no reason to delay them.
	h.send(shared.ActionConnected, "", "")

	assertEvent(t, h.nextWithin(window/3), shared.ActionConnected, "", shared.StateOk)
}

func TestAlreadyFinishedEventsAreNotHeld(t *testing.T) {
	h := newHarness(t)

	ev := shared.NewEvent(time.Now(), incusapi.EventLifecycleInstanceUpdated, "p", "one", "")
	h.p.Handle(ev.WithDropped("somebody"))

	// It is walking for the observers. Delaying it would delay a report of
	// something that has already happened.
	out := h.nextWithin(window / 3)
	assertEvent(t, out, incusapi.EventLifecycleInstanceUpdated, "one", shared.StateDropped)

	if out.Reason() != "somebody" {
		t.Fatalf("reason %q, want it left alone", out.Reason())
	}
}

func TestVetoedActionIsNeverCollapsed(t *testing.T) {
	h := newHarness(t)

	// dns asks for every rename, so all of them arrive whole and none waits.
	for range 3 {
		h.send(incusapi.EventLifecycleInstanceRenamed, "p", "one")
	}

	for range 3 {
		assertEvent(t, h.nextWithin(window/3), incusapi.EventLifecycleInstanceRenamed, "one", shared.StateOk)
	}
}

func TestVetoIsPerAction(t *testing.T) {
	h := newHarness(t)

	// instance-started fires once, so it is not worth a window either - but
	// that says nothing about an update on the same instance.
	h.send(incusapi.EventLifecycleInstanceStarted, "p", "one")
	assertEvent(t, h.nextWithin(window/3), incusapi.EventLifecycleInstanceStarted, "one", shared.StateOk)

	h.send(incusapi.EventLifecycleInstanceUpdated, "p", "one")
	h.send(incusapi.EventLifecycleInstanceUpdated, "p", "one")

	assertEvent(t, h.nextWithin(window/3), incusapi.EventLifecycleInstanceUpdated, "one", shared.StateOk)
	assertEvent(t, h.next(), incusapi.EventLifecycleInstanceUpdated, "one", shared.StateOk)
}

func TestUnknownActionIsNotCollapsed(t *testing.T) {
	h := newHarness(t)

	// Nothing wanted it, so the source would never have walked it at all. If
	// one arrives anyway, the zero Want vetoes and it passes through.
	h.send(incusapi.EventLifecycleInstanceMigrated, "p", "one")

	assertEvent(t, h.nextWithin(window/3), incusapi.EventLifecycleInstanceMigrated, "one", shared.StateOk)
}

func TestPassThroughDoesNotOvertakeAWaitingEvent(t *testing.T) {
	h := newHarness(t)

	// The case this is really about: the actions worth collapsing sit next to
	// ones that are not, on the same instance.
	h.send(incusapi.EventLifecycleInstanceUpdated, "p", "one")
	h.next()

	h.send(incusapi.EventLifecycleInstanceUpdated, "p", "one")
	h.nothingYet(window / 4)

	h.send(incusapi.EventLifecycleInstanceRenamed, "p", "one")

	// The waiting update arrived first, so it leaves first.
	assertEvent(t, h.nextWithin(window/3), incusapi.EventLifecycleInstanceUpdated, "one", shared.StateOk)
	assertEvent(t, h.nextWithin(window/3), incusapi.EventLifecycleInstanceRenamed, "one", shared.StateOk)
}

func TestPassThroughOnlyClosesItsOwnKey(t *testing.T) {
	h := newHarness(t)

	for _, n := range []string{"one", "two"} {
		h.send(incusapi.EventLifecycleInstanceUpdated, "p", n)
		h.next()
		h.send(incusapi.EventLifecycleInstanceUpdated, "p", n)
	}

	h.nothingYet(window / 4)

	// A vetoed event on "one" says nothing about "two", which keeps waiting.
	h.send(incusapi.EventLifecycleInstanceRenamed, "p", "one")

	assertEvent(t, h.nextWithin(window/3), incusapi.EventLifecycleInstanceUpdated, "one", shared.StateOk)
	assertEvent(t, h.nextWithin(window/3), incusapi.EventLifecycleInstanceRenamed, "one", shared.StateOk)

	assertEvent(t, h.next(), incusapi.EventLifecycleInstanceUpdated, "two", shared.StateOk)
}

func TestDrainClosesOpenWindows(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	out := make(chan *shared.Event, 8)

	in := make(chan shared.Command)
	raised := make(chan shared.Command, 1)

	// An hour, so the window is one only the shutdown can close.
	p := New(Window(time.Hour))

	err := p.Setup(shared.SetupArgs{
		Context:    ctx,
		Wanted:     wanted,
		Next:       func(ev *shared.Event) { out <- ev },
		CommandIn:  in,
		CommandOut: raised,
	})
	if err != nil {
		t.Fatalf("Setup: %s", err)
	}

	p.Handle(shared.NewEvent(time.Now(), incusapi.EventLifecycleInstanceUpdated, "p", "one", ""))
	p.Handle(shared.NewEvent(time.Now(), incusapi.EventLifecycleInstanceUpdated, "p", "one", ""))

	var wg sync.WaitGroup

	wg.Go(func() {
		err := p.Run(ctx)
		if err != nil {
			t.Errorf("Run: %s", err)
		}
	})

	// Asked to finish rather than canceled. Canceling is an abort now, and what
	// is held goes nowhere - which is the whole difference the drain makes.
	in <- shared.Command{Action: shared.CommandDrain}

	got := <-raised
	if got.Action != shared.CommandDrain {
		t.Fatalf("answer was %q, want the question back", got.Action)
	}

	wg.Wait()
	cancel()

	if len(out) != 2 {
		t.Fatalf("got %d events, want the leading one and the trailing one", len(out))
	}

	assertEvent(t, <-out, incusapi.EventLifecycleInstanceUpdated, "one", shared.StateOk)
	assertEvent(t, <-out, incusapi.EventLifecycleInstanceUpdated, "one", shared.StateOk)
}

func TestRunReturnsAfterCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())

	p := New(Window(window))

	err := p.Setup(shared.SetupArgs{Context: ctx, Wanted: wanted, Next: func(_ *shared.Event) {}})
	if err != nil {
		t.Fatalf("Setup: %s", err)
	}

	cancel()

	ran := make(chan error, 1)

	go func() { ran <- p.Run(ctx) }()

	select {
	case err := <-ran:
		if err != nil {
			t.Fatalf("Run: %s", err)
		}
	case <-time.After(5 * window):
		t.Fatal("Run did not return after the context was canceled")
	}
}

func TestFullInboxDropsRatherThanBlocks(t *testing.T) {
	// No Setup, so nothing drains the inbox: Handle has to cope on its own.
	seen := []*shared.Event{}
	p := New(Window(window))
	p.next = func(ev *shared.Event) { seen = append(seen, ev) }

	for range defaultInboxSize {
		p.Handle(shared.NewEvent(time.Now(), incusapi.EventLifecycleInstanceUpdated, "p", "one", ""))
	}

	if len(seen) != 0 {
		t.Fatalf("dropped %d before the inbox was full", len(seen))
	}

	p.Handle(shared.NewEvent(time.Now(), incusapi.EventLifecycleInstanceUpdated, "p", "two", ""))

	if len(seen) != 1 {
		t.Fatalf("handed on %d past a full inbox, want 1", len(seen))
	}

	// Marked and traveling, not swallowed: a drop nobody can see is worse than
	// a drop.
	assertEvent(t, seen[0], incusapi.EventLifecycleInstanceUpdated, "two", shared.StateDropped)

	if seen[0].Reason() != name {
		t.Fatalf("reason %q, want %q", seen[0].Reason(), name)
	}
}
