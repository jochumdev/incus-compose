package debounce

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	incusapi "github.com/lxc/incus/v7/shared/api"

	"github.com/lxc/incus-compose/ievent/iutil"
)

// No skip helper on any test here: this plugin talks to nothing, so all of it
// runs in every stage.

// wanted is the table the source would have built: updates collapse, renames
// and instance-started do not.
var wanted = map[string]iutil.Want{
	incusapi.EventLifecycleInstanceUpdated: {Action: incusapi.EventLifecycleInstanceUpdated, Debounce: true},
	incusapi.EventLifecycleNetworkUpdated:  {Action: incusapi.EventLifecycleNetworkUpdated, Debounce: true},
	incusapi.EventLifecycleInstanceRenamed: {Action: incusapi.EventLifecycleInstanceRenamed},
	incusapi.EventLifecycleInstanceStarted: {Action: incusapi.EventLifecycleInstanceStarted},
}

// window is long enough that "not out yet" is not a race on a loaded machine,
// and short enough that the suite stays quick.
const window = 150 * time.Millisecond

// fixture wires one plugin to a collecting successor and runs it the way main would.
type fixture struct {
	t   *testing.T
	p   *Plugin
	out chan *iutil.Event

	// ran carries what Run returned, which is also how a test waits for the
	// shutdown flush - there is no second signal.
	ran chan error

	// chain is what the source would be stamping; warm is where collapsing
	// happens, so that is where a test starts.
	chain iutil.ChainState
}

func newFixture(t *testing.T) *fixture {
	t.Helper()

	ctx, cancel := context.WithCancel(t.Context())
	h := &fixture{
		t:     t,
		p:     New(Window(window)),
		out:   make(chan *iutil.Event, 64),
		ran:   make(chan error, 1),
		chain: iutil.ChainWarm,
	}

	err := h.p.Setup(iutil.SetupArgs{
		Context: ctx,
		Wanted:  wanted,
		Next:    func(ev *iutil.Event) { h.out <- ev },
	})
	if err != nil {
		t.Fatalf("Setup: %s", err)
	}

	var wg sync.WaitGroup

	wg.Go(func() { h.ran <- h.p.Run(ctx) })

	// Cancel and wait so a leftover open window doesn't leak the goroutine, and
	// every test asserts Run returned clean.
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

// send hands one event to Handle, stamped the way the source would: the end of
// a round turns the chain warm, so the events after it follow.
func (h *fixture) send(action, project, name string) {
	h.t.Helper()

	if action == iutil.ActionSweepEnd {
		h.chain = iutil.ChainWarm
	}

	h.p.Handle(iutil.NewEvent(time.Now(), action, project, name, "").WithChainState(h.chain))
}

// next waits for one event to come out the far end.
func (h *fixture) next() *iutil.Event {
	h.t.Helper()

	select {
	case ev := <-h.out:
		return ev
	case <-time.After(5 * window):
		h.t.Fatal("timed out waiting for an event")

		return nil
	}
}

// nextWithin waits for one event and fails if it takes as long as d - unlike
// next, it can tell "handed straight on" from "held then released".
func (h *fixture) nextWithin(d time.Duration) *iutil.Event {
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
func (h *fixture) nothingYet(d time.Duration) {
	h.t.Helper()

	select {
	case ev := <-h.out:
		h.t.Fatalf("released %s/%s early", ev.ProjectName(), ev.Name())
	case <-time.After(d):
	}
}

// assertEvent checks one event is the one expected, and that it ended the way
// it should have: nil for one nothing happened to, or the sentinel it carries.
func assertEvent(t *testing.T, ev *iutil.Event, action, name string, want error) {
	t.Helper()

	if ev.Action() != action || ev.Name() != name || !errors.Is(ev.Err(), want) {
		t.Fatalf("got %s %s %v, want %s %s %v",
			ev.Action(), ev.Name(), ev.Err(), action, name, want)
	}
}

// droppedBy is who finished one event with, off the sentinel it carries.
func droppedBy(t *testing.T, ev *iutil.Event) string {
	t.Helper()

	var err *iutil.Error

	if !errors.As(ev.Err(), &err) {
		t.Fatalf("event carries %v, want one of ours", ev.Err())
	}

	return err.By()
}

func TestLoneEventGoesAtOnce(t *testing.T) {
	t.Parallel()

	h := newFixture(t)

	// One change is not a burst; waiting out the window for it would be pure latency.
	h.send(incusapi.EventLifecycleInstanceUpdated, "p", "one")

	assertEvent(t, h.nextWithin(window/3), incusapi.EventLifecycleInstanceUpdated, "one", nil)
}

func TestLoneEventIsNotReportedTwice(t *testing.T) {
	t.Parallel()

	h := newFixture(t)

	h.send(incusapi.EventLifecycleInstanceUpdated, "p", "one")
	h.next()

	// The window closes on nothing: a burst of one was already carried by its leading edge.
	h.nothingYet(2 * window)
}

func TestTwoEventsGiveTheFirstAndTheLast(t *testing.T) {
	t.Parallel()

	h := newFixture(t)

	h.send(incusapi.EventLifecycleInstanceUpdated, "p", "one")
	assertEvent(t, h.nextWithin(window/3), incusapi.EventLifecycleInstanceUpdated, "one", nil)

	h.send(incusapi.EventLifecycleInstanceUpdated, "p", "one")

	// The second closes the window rather than riding the leading edge, so it
	// arrives once the key is quiet; nothing sat between them to drop.
	h.nothingYet(window / 3)
	assertEvent(t, h.next(), incusapi.EventLifecycleInstanceUpdated, "one", nil)
}

func TestStormGivesTheFirstAndTheLastAndDropsTheMiddle(t *testing.T) {
	t.Parallel()

	h := newFixture(t)

	for range 20 {
		h.send(incusapi.EventLifecycleInstanceUpdated, "p", "one")
	}

	// First out is the leading edge, at once and un-dropped.
	assertEvent(t, h.nextWithin(window/3), incusapi.EventLifecycleInstanceUpdated, "one", nil)

	// Then the eighteen the last one superseded - still walking, marked dropped.
	for range 18 {
		ev := h.next()
		if !errors.Is(ev.Err(), iutil.ErrDropped) {
			t.Fatalf("event ended %v, want dropped", ev.Err())
		}

		if by := droppedBy(t, ev); by != name {
			t.Fatalf("dropped by %q, want %q", by, name)
		}
	}

	assertEvent(t, h.next(), incusapi.EventLifecycleInstanceUpdated, "one", nil)
}

func TestWindowReopensAfterItCloses(t *testing.T) {
	t.Parallel()

	h := newFixture(t)

	h.send(incusapi.EventLifecycleInstanceUpdated, "p", "one")
	h.next()
	h.nothingYet(2 * window)

	// The window closed on nothing, so the next event is a leading edge again.
	h.send(incusapi.EventLifecycleInstanceUpdated, "p", "one")

	assertEvent(t, h.nextWithin(window/3), incusapi.EventLifecycleInstanceUpdated, "one", nil)
}

func TestKeysDoNotCollapseIntoEachOther(t *testing.T) {
	t.Parallel()

	h := newFixture(t)

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
	t.Parallel()

	h := newFixture(t)

	h.send(incusapi.EventLifecycleInstanceUpdated, "a", "one")
	h.send(incusapi.EventLifecycleInstanceUpdated, "b", "one")

	first, second := h.nextWithin(window/3), h.nextWithin(window/3)
	if first.ProjectName() == second.ProjectName() {
		t.Fatalf("both came from %s, want one from each project", first.ProjectName())
	}
}

func TestNamelessActionsAreNotHeld(t *testing.T) {
	t.Parallel()

	h := newFixture(t)

	// The source's own actions carry no project or name, so there's nothing to key on.
	h.send(iutil.ActionConnected, "", "")

	assertEvent(t, h.nextWithin(window/3), iutil.ActionConnected, "", nil)
}

func TestAlreadyFinishedEventsAreNotHeld(t *testing.T) {
	t.Parallel()

	h := newFixture(t)

	ev := iutil.NewEvent(time.Now(), incusapi.EventLifecycleInstanceUpdated, "p", "one", "")
	h.p.Handle(ev.WithDropped("somebody"))

	// Delaying it would delay a report of something that already happened.
	out := h.nextWithin(window / 3)
	assertEvent(t, out, incusapi.EventLifecycleInstanceUpdated, "one", iutil.ErrDropped)

	if by := droppedBy(t, out); by != "somebody" {
		t.Fatalf("dropped by %q, want it left alone", by)
	}
}

func TestVetoedActionIsNeverCollapsed(t *testing.T) {
	t.Parallel()

	h := newFixture(t)

	// dns asks for every rename, so all of them arrive whole and none waits.
	for range 3 {
		h.send(incusapi.EventLifecycleInstanceRenamed, "p", "one")
	}

	for range 3 {
		assertEvent(t, h.nextWithin(window/3), incusapi.EventLifecycleInstanceRenamed, "one", nil)
	}
}

func TestVetoIsPerAction(t *testing.T) {
	t.Parallel()

	h := newFixture(t)

	// instance-started isn't worth a window, but that says nothing about updates on the same instance.
	h.send(incusapi.EventLifecycleInstanceStarted, "p", "one")
	assertEvent(t, h.nextWithin(window/3), incusapi.EventLifecycleInstanceStarted, "one", nil)

	h.send(incusapi.EventLifecycleInstanceUpdated, "p", "one")
	h.send(incusapi.EventLifecycleInstanceUpdated, "p", "one")

	assertEvent(t, h.nextWithin(window/3), incusapi.EventLifecycleInstanceUpdated, "one", nil)
	assertEvent(t, h.next(), incusapi.EventLifecycleInstanceUpdated, "one", nil)
}

func TestUnknownActionIsNotCollapsed(t *testing.T) {
	t.Parallel()

	h := newFixture(t)

	// Nothing wanted it, so the zero Want vetoes and it passes through anyway.
	h.send(incusapi.EventLifecycleInstanceMigrated, "p", "one")

	assertEvent(t, h.nextWithin(window/3), incusapi.EventLifecycleInstanceMigrated, "one", nil)
}

func TestPassThroughDoesNotOvertakeAWaitingEvent(t *testing.T) {
	t.Parallel()

	h := newFixture(t)

	// Actions worth collapsing sit next to ones that aren't, on the same instance.
	h.send(incusapi.EventLifecycleInstanceUpdated, "p", "one")
	h.next()

	h.send(incusapi.EventLifecycleInstanceUpdated, "p", "one")
	h.nothingYet(window / 4)

	h.send(incusapi.EventLifecycleInstanceRenamed, "p", "one")

	// The waiting update arrived first, so it leaves first.
	assertEvent(t, h.nextWithin(window/3), incusapi.EventLifecycleInstanceUpdated, "one", nil)
	assertEvent(t, h.nextWithin(window/3), incusapi.EventLifecycleInstanceRenamed, "one", nil)
}

func TestPassThroughOnlyClosesItsOwnKey(t *testing.T) {
	t.Parallel()

	h := newFixture(t)

	for _, n := range []string{"one", "two"} {
		h.send(incusapi.EventLifecycleInstanceUpdated, "p", n)
		h.next()
		h.send(incusapi.EventLifecycleInstanceUpdated, "p", n)
	}

	h.nothingYet(window / 4)

	// A vetoed event on "one" says nothing about "two", which keeps waiting.
	h.send(incusapi.EventLifecycleInstanceRenamed, "p", "one")

	assertEvent(t, h.nextWithin(window/3), incusapi.EventLifecycleInstanceUpdated, "one", nil)
	assertEvent(t, h.nextWithin(window/3), incusapi.EventLifecycleInstanceRenamed, "one", nil)

	assertEvent(t, h.next(), incusapi.EventLifecycleInstanceUpdated, "two", nil)
}

func TestDrainClosesOpenWindows(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())
	out := make(chan *iutil.Event, 8)

	in := make(chan iutil.Command)
	raised := make(chan iutil.Command, 1)

	// An hour, so the window is one only the shutdown can close.
	p := New(Window(time.Hour))

	err := p.Setup(iutil.SetupArgs{
		Context:    ctx,
		Wanted:     wanted,
		Next:       func(ev *iutil.Event) { out <- ev },
		CommandIn:  in,
		CommandOut: raised,
	})
	if err != nil {
		t.Fatalf("Setup: %s", err)
	}

	warm := iutil.NewEvent(time.Now(), incusapi.EventLifecycleInstanceUpdated, "p", "one", "").
		WithChainState(iutil.ChainWarm)

	p.Handle(warm)
	p.Handle(warm)

	var wg sync.WaitGroup

	wg.Go(func() {
		err := p.Run(ctx)
		if err != nil {
			t.Errorf("Run: %s", err)
		}
	})

	// Asked to finish rather than canceled: canceling is an abort, and what's held goes nowhere.
	in <- iutil.Command{Action: iutil.CommandDrain}

	got := <-raised
	if got.Action != iutil.CommandDrain {
		t.Fatalf("answer was %q, want the question back", got.Action)
	}

	wg.Wait()
	cancel()

	if len(out) != 2 {
		t.Fatalf("got %d events, want the leading one and the trailing one", len(out))
	}

	assertEvent(t, <-out, incusapi.EventLifecycleInstanceUpdated, "one", nil)
	assertEvent(t, <-out, incusapi.EventLifecycleInstanceUpdated, "one", nil)
}

func TestRunReturnsAfterCancel(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())

	p := New(Window(window))

	err := p.Setup(iutil.SetupArgs{Context: ctx, Wanted: wanted, Next: func(_ *iutil.Event) {}})
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
	t.Parallel()

	// No Setup, so nothing drains the inbox: Handle has to cope on its own.
	seen := []*iutil.Event{}
	p := New(Window(window))
	p.next = func(ev *iutil.Event) { seen = append(seen, ev) }

	for range defaultInboxSize {
		p.Handle(iutil.NewEvent(time.Now(), incusapi.EventLifecycleInstanceUpdated, "p", "one", ""))
	}

	if len(seen) != 0 {
		t.Fatalf("dropped %d before the inbox was full", len(seen))
	}

	p.Handle(iutil.NewEvent(time.Now(), incusapi.EventLifecycleInstanceUpdated, "p", "two", ""))

	if len(seen) != 1 {
		t.Fatalf("handed on %d past a full inbox, want 1", len(seen))
	}

	// Marked and traveling, not swallowed: a silent drop is worse than a visible one.
	assertEvent(t, seen[0], incusapi.EventLifecycleInstanceUpdated, "two", iutil.ErrDropped)

	if by := droppedBy(t, seen[0]); by != name {
		t.Fatalf("dropped by %q, want %q", by, name)
	}
}
