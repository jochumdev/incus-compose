package enricher

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	incusapi "github.com/lxc/incus/v7/shared/api"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lxc/incus-compose/ievent/shared"
	"github.com/lxc/incus-compose/testlib"
)

// wanted is the table the source would have built.
var wanted = map[string]shared.Want{
	incusapi.EventLifecycleInstanceUpdated: {
		Action: incusapi.EventLifecycleInstanceUpdated,
		Enrich: shared.EnrichedInstance | shared.EnrichedNetworks | shared.EnrichedProject,
	},
	incusapi.EventLifecycleInstanceDeleted: {Action: incusapi.EventLifecycleInstanceDeleted},

	// Wanted for its networks alone, which is what makes it the case that a
	// name does not imply an instance.
	incusapi.EventLifecycleNetworkUpdated: {
		Action: incusapi.EventLifecycleNetworkUpdated,
		Enrich: shared.EnrichedNetworks,
	},
	incusapi.EventLifecycleNetworkDeleted: {Action: incusapi.EventLifecycleNetworkDeleted},
	incusapi.EventLifecycleNetworkRenamed: {Action: incusapi.EventLifecycleNetworkRenamed},
	incusapi.EventLifecycleProfileUpdated: {Action: incusapi.EventLifecycleProfileUpdated},
}

// harness wires one plugin to a collecting successor and runs it the way main
// would, answering its reads from testlib rather than a daemon.
type harness struct {
	t   *testing.T
	p   *Plugin
	out chan *shared.Event

	cancel context.CancelFunc
	wg     sync.WaitGroup

	// in and raised are the two doors the source gives a plugin: one to ask it
	// something, one for it to say something.
	in     chan shared.Command
	raised chan shared.Command

	// forward stops the goroutine that plays the source, so a drain can read
	// its own answer off raised rather than losing it to that goroutine.
	//
	// forwarded says that goroutine has actually gone. Closing forward is not
	// enough on its own: while it is still sitting in its select, both that
	// case and the drain answer on raised are ready, and select picks between
	// ready cases at random.
	forward     chan struct{}
	forwarded   chan struct{}
	stopForward sync.Once

	mu    sync.Mutex
	reads map[string]int
	err   error

	// gate holds every read until it is closed, so a test can decide when a
	// read lands and in which order.
	gate chan struct{}

	// fleet is what a whole-fleet pass finds, and listErr what it fails with.
	fleet   *testlib.Project
	listErr error
	lists   atomic.Int32
}

func newHarness(t *testing.T) *harness {
	t.Helper()

	// Not t.Context(): the testing package cancels that just before cleanups
	// run, so a harness that drains in a cleanup would find the plugin already
	// aborted and waiting for an answer it can no longer give.
	ctx, cancel := context.WithCancel(context.Background())
	h := &harness{
		t:         t,
		p:         New(Workers(8), ReadTimeout(time.Second)),
		out:       make(chan *shared.Event, 64),
		in:        make(chan shared.Command),
		raised:    make(chan shared.Command, 8),
		forward:   make(chan struct{}),
		forwarded: make(chan struct{}),
		cancel:    cancel,
		reads:     map[string]int{},
	}

	h.p.read = h.answer
	h.p.readNet = h.answerNet
	h.p.list = h.fleetRead

	err := h.p.Setup(shared.SetupArgs{
		Context:    ctx,
		Wanted:     wanted,
		Next:       func(ev *shared.Event) { h.out <- ev },
		CommandIn:  h.in,
		CommandOut: h.raised,
	})
	require.NoError(t, err)

	// What the source does with a raised action: mint it and put it in at the
	// head. There is nothing in front of the enricher here, so the head is its
	// own door.
	h.wg.Go(func() {
		defer close(h.forwarded)

		for {
			select {
			case cmd := <-h.raised:
				h.p.Handle(shared.NewEvent(time.Now(), cmd.Action, "", "", ""))
			case <-h.forward:
				return
			case <-ctx.Done():
				return
			}
		}
	})

	h.wg.Go(func() {
		err := h.p.Run(ctx)
		if err != nil {
			t.Errorf("Run: %s", err)
		}
	})

	t.Cleanup(h.stop)

	return h
}

// stop shuts the plugin down the way the source does, and is safe to call twice
// so a test can assert on what the shutdown handed on.
//
// Ask, wait for the answer, then cancel. Canceling first is an abort, and
// everything held would go nowhere - which is the difference the drain command
// exists to make.
func (h *harness) stop() {
	h.stopForward.Do(func() {
		// The answer comes back on the same channel the brackets do, so the
		// goroutine playing the source has to stop reading it first - and be
		// gone rather than merely told to go, or the answer below is one it
		// can still take instead of us.
		close(h.forward)

		select {
		case <-h.forwarded:
		case <-time.After(5 * time.Second):
			h.t.Error("the goroutine playing the source never stopped")

			return
		}

		select {
		case h.in <- shared.Command{Action: shared.CommandDrain}:
		case <-time.After(5 * time.Second):
			h.t.Error("the enricher never took the drain")

			return
		}

		for {
			select {
			case cmd := <-h.raised:
				if cmd.Action != shared.CommandDrain {
					continue
				}

			case <-time.After(5 * time.Second):
				h.t.Error("the enricher never answered the drain")
			}

			return
		}
	})

	h.cancel()
	h.wg.Wait()
}

// answer is what a pool worker calls instead of Incus. The instance it builds
// carries the name asked for, so a test can tell one read from another.
func (h *harness) answer(
	ctx context.Context,
	project, name string,
) (*incusapi.Instance, *incusapi.InstanceState, error) {
	h.mu.Lock()
	h.reads[project+"/"+name]++
	gate, failWith := h.gate, h.err
	h.mu.Unlock()

	if gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
			return nil, nil, ctx.Err()
		}
	}

	if failWith != nil {
		return nil, nil, failWith
	}

	inst := testlib.NewInstance(project, 0, 0)
	inst.Name = name
	inst.ExpandedConfig[testlib.LabelPrefix+"service"] = name

	return &inst, testlib.NewInstanceState(0, 0), nil
}

// answerNet is what a pool worker calls instead of reading one network.
func (h *harness) answerNet(_ context.Context, project, name string) (*incusapi.Network, error) {
	h.mu.Lock()
	// Counted apart from the instance reads: the two are keyed the same way, and
	// telling them apart is the whole point of one of the tests below.
	h.reads["net:"+project+"/"+name]++
	failWith := h.err
	h.mu.Unlock()

	if failWith != nil {
		return nil, failWith
	}

	net := testlib.NewNetwork(project, 0)
	net.Name = name

	return &net, nil
}

// fleetRead is what a pool worker calls instead of listing the daemon.
func (h *harness) fleetRead(_ context.Context) (*sweepResult, error) {
	h.lists.Add(1)

	h.mu.Lock()
	fleet, err := h.fleet, h.listErr
	h.mu.Unlock()

	if err != nil {
		return nil, err
	}

	if fleet == nil {
		return &sweepResult{}, nil
	}

	instances := make([]instanceRead, 0, len(fleet.Instances))

	for _, inst := range fleet.Instances {
		instances = append(instances, instanceRead{
			instance: inst,
			state:    fleet.States[inst.Name],
		})
	}

	return &sweepResult{
		networks:  fleet.Networks,
		instances: instances,
		labels:    map[string]map[string]string{fleet.Project.Name: fleet.Project.Config},
	}, nil
}

// readsOf is how many instance reads one key has cost.
func (h *harness) readsOf(project, name string) int {
	h.mu.Lock()
	defer h.mu.Unlock()

	return h.reads[project+"/"+name]
}

// netReadsOf is how many network reads one key has cost.
func (h *harness) netReadsOf(project, name string) int {
	h.mu.Lock()
	defer h.mu.Unlock()

	return h.reads["net:"+project+"/"+name]
}

func (h *harness) send(action, project, name string) {
	h.t.Helper()

	h.p.Handle(shared.NewEvent(time.Now(), action, project, name, ""))
}

func (h *harness) next() *shared.Event {
	h.t.Helper()

	select {
	case ev := <-h.out:
		return ev
	case <-time.After(2 * time.Second):
		h.t.Fatal("timed out waiting for an event")

		return nil
	}
}

// TestOrderIsArrivalOrder is the contract the whole shape rests on, and the one
// worth pinning before any read exists to disturb it.
func TestOrderIsArrivalOrder(t *testing.T) {
	tests := []struct {
		name   string
		arrive []string
	}{
		{name: "one instance", arrive: []string{"a"}},
		{name: "several instances", arrive: []string{"a", "b", "c", "d"}},
		{name: "the same instance repeatedly", arrive: []string{"a", "a", "a"}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)

			for _, n := range tc.arrive {
				h.send(incusapi.EventLifecycleInstanceUpdated, "p", n)
			}

			for i, n := range tc.arrive {
				assert.Equal(t, n, h.next().Name(), "position %d", i)
			}
		})
	}
}

// TestPassesEverythingThrough covers the kinds that need no read at all. They
// still take their place in the line rather than overtaking.
func TestPassesEverythingThrough(t *testing.T) {
	tests := []struct {
		name   string
		action string
		who    string
	}{
		{name: "an action with nothing to enrich", action: incusapi.EventLifecycleInstanceDeleted, who: "one"},
		{name: "an action nobody wanted", action: incusapi.EventLifecycleInstanceMigrated, who: "one"},
		{name: "the source's own, which carries no name", action: shared.ActionConnected},
		{name: "a sweep bracket", action: shared.ActionSweepStart},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)

			h.send(tc.action, "p", tc.who)

			ev := h.next()
			assert.Equal(t, tc.action, ev.Action())
			assert.Equal(t, shared.StateOk, ev.State())
		})
	}
}

// TestAlreadyFinishedEventsAreUntouched: an event a plugin in front is done
// with is walking for the observers, and enriching it would be a read nobody
// asked for.
func TestAlreadyFinishedEventsAreUntouched(t *testing.T) {
	h := newHarness(t)

	ev := shared.NewEvent(time.Now(), incusapi.EventLifecycleInstanceUpdated, "p", "one", "")
	h.p.Handle(ev.WithDropped("debounce"))

	out := h.next()
	assert.Equal(t, shared.StateDropped, out.State())
	assert.Equal(t, "debounce", out.Reason(), "the first reason is the one that stands")
}

// TestShutdownHandsOnWhatItHolds: nothing this plugin accepted may be swallowed
// on the way out, because an event the chain never saw is worse than a late one.
func TestShutdownHandsOnWhatItHolds(t *testing.T) {
	h := newHarness(t)

	for _, n := range []string{"a", "b", "c"} {
		h.send(incusapi.EventLifecycleInstanceUpdated, "p", n)
	}

	h.stop()

	require.Len(t, h.out, 3, "every event taken is handed on")

	for _, n := range []string{"a", "b", "c"} {
		assert.Equal(t, n, (<-h.out).Name(), "and still in order")
	}
}

// TestFullInboxDropsRatherThanBlocks: Handle runs on somebody else's goroutine,
// so it may not wait. A drop rather than a failure - nothing went wrong with a
// read, this plugin is behind.
func TestFullInboxDropsRatherThanBlocks(t *testing.T) {
	// No Run, so nothing drains the inbox: Handle has to cope on its own.
	seen := []*shared.Event{}
	p := New()
	p.next = func(ev *shared.Event) { seen = append(seen, ev) }

	for range defaultInboxSize {
		p.Handle(shared.NewEvent(time.Now(), incusapi.EventLifecycleInstanceUpdated, "p", "one", ""))
	}

	require.Empty(t, seen, "nothing dropped before the inbox was full")

	p.Handle(shared.NewEvent(time.Now(), incusapi.EventLifecycleInstanceUpdated, "p", "two", ""))

	require.Len(t, seen, 1)
	assert.Equal(t, shared.StateDropped, seen[0].State())
	assert.Equal(t, name, seen[0].Reason(), "and says who dropped it")
}

// TestEnrichesFromTheRead is the point of the plugin: what leaves carries what
// the read found, and says so.
func TestEnrichesFromTheRead(t *testing.T) {
	h := newHarness(t)

	h.send(incusapi.EventLifecycleInstanceUpdated, "p", "one")

	ev := h.next()

	assert.Equal(t, shared.StateOk, ev.State())
	assert.True(t, ev.Enriched(shared.EnrichedInstance), "the instance read landed")
	assert.True(t, ev.Running())

	service, ok := ev.Metadata(testlib.LabelPrefix + "service")
	assert.True(t, ok)
	assert.Equal(t, "one", service, "the labels came off the instance that was read")
}

// TestFailedReadFails is rule 7: what asked for something and did not get it
// says so, rather than arriving looking complete.
func TestFailedReadFails(t *testing.T) {
	h := newHarness(t)

	h.mu.Lock()
	h.err = errors.New("incusd said no")
	h.mu.Unlock()

	h.send(incusapi.EventLifecycleInstanceUpdated, "p", "one")

	ev := h.next()

	assert.Equal(t, shared.StateFailed, ev.State())
	assert.Equal(t, "source/read", ev.Reason(), "an actor, not a bare cause")
	assert.False(t, ev.Enriched(shared.EnrichedInstance), "and nothing pretends to have landed")
}

// TestCoalescesReadsPerKey: coalescing saves the read, not the event. Two
// events on one key cost one read and both still walk, carrying what it found.
func TestCoalescesReadsPerKey(t *testing.T) {
	h := newHarness(t)

	// Held, so the second event arrives while the first read is still out.
	h.mu.Lock()
	h.gate = make(chan struct{})
	gate := h.gate
	h.mu.Unlock()

	h.send(incusapi.EventLifecycleInstanceUpdated, "p", "one")

	// Wait for the read to be running before the second event arrives, so this
	// is coalescing rather than a race that happened to pass.
	require.Eventually(t, func() bool { return h.readsOf("p", "one") == 1 },
		time.Second, time.Millisecond, "the first read started")

	h.send(incusapi.EventLifecycleInstanceUpdated, "p", "one")
	close(gate)

	first, second := h.next(), h.next()

	assert.True(t, first.Enriched(shared.EnrichedInstance))
	assert.True(t, second.Enriched(shared.EnrichedInstance), "both carry what the one read found")
	assert.Equal(t, 1, h.readsOf("p", "one"), "and it was one read")
}

// TestSlowReadHoldsTheLine is the ordering cost, stated: reads run
// concurrently, delivery does not. A read still out at the front keeps
// everything behind it waiting, however long ago those finished.
func TestSlowReadHoldsTheLine(t *testing.T) {
	h := newHarness(t)

	h.mu.Lock()
	h.gate = make(chan struct{})
	gate := h.gate
	h.mu.Unlock()

	h.send(incusapi.EventLifecycleInstanceUpdated, "p", "slow")
	require.Eventually(t, func() bool { return h.readsOf("p", "slow") == 1 },
		time.Second, time.Millisecond, "the slow read started")

	for _, n := range []string{"a", "b"} {
		h.send(incusapi.EventLifecycleInstanceUpdated, "p", n)
	}

	// Their reads run and finish; nothing leaves, because the front has not.
	require.Eventually(t, func() bool { return h.readsOf("p", "b") == 1 },
		time.Second, time.Millisecond, "the reads behind it ran anyway")

	assert.Empty(t, h.out, "and none of them left")

	close(gate)

	for _, n := range []string{"slow", "a", "b"} {
		assert.Equal(t, n, h.next().Name(), "then all of it, in arrival order")
	}
}

// TestDeleteCostsNoRead is rule 3: a delete says the subject is gone, and
// reading to confirm it would be a read whose answer we already have.
func TestDeleteCostsNoRead(t *testing.T) {
	h := newHarness(t)

	h.send(incusapi.EventLifecycleInstanceUpdated, "p", "one")
	require.True(t, h.next().Enriched(shared.EnrichedInstance))

	h.send(incusapi.EventLifecycleInstanceDeleted, "p", "one")

	ev := h.next()
	assert.Equal(t, incusapi.EventLifecycleInstanceDeleted, ev.Action())
	assert.Equal(t, 1, h.readsOf("p", "one"), "the delete added no read")
}

// TestOnlyInstanceActionsAreRead: a name is not enough to make something an
// instance. A network action carries one too, and reading an instance called
// net0 is the mistake this guards.
func TestOnlyInstanceActionsAreRead(t *testing.T) {
	h := newHarness(t)

	h.send(incusapi.EventLifecycleNetworkUpdated, "p", "net0")

	ev := h.next()

	assert.Equal(t, incusapi.EventLifecycleNetworkUpdated, ev.Action())
	assert.Equal(t, shared.StateOk, ev.State())
	assert.Zero(t, h.readsOf("p", "net0"), "no instance read for a network's name")

	// It is read, just as the thing it actually is.
	require.Eventually(t, func() bool { return h.netReadsOf("p", "net0") == 1 },
		2*time.Second, time.Millisecond, "read as a network instead")
}

// collect takes n events off the far end.
func (h *harness) collect(n int) []*shared.Event {
	h.t.Helper()

	out := make([]*shared.Event, 0, n)
	for range n {
		out = append(out, h.next())
	}

	return out
}

// TestConnectedSweeps: a stream coming up makes everything held suspect at
// once, because whatever happened while it was down was announced to nobody.
func TestConnectedSweeps(t *testing.T) {
	h := newHarness(t)

	h.mu.Lock()
	h.fleet = testlib.NewProject("p", 2, 1)
	h.mu.Unlock()

	h.send(shared.ActionConnected, "", "")

	// The bracket, then what the pass found, then the closing bracket. The
	// brackets are what let a plugin behind here tell absence from silence.
	got := h.collect(5)

	assert.Equal(t, shared.ActionConnected, got[0].Action())
	assert.Equal(t, shared.ActionSweepStart, got[1].Action())

	names := []string{got[2].Name(), got[3].Name()}
	assert.ElementsMatch(t, []string{testlib.InstanceName(0), testlib.InstanceName(1)}, names)

	assert.Equal(t, shared.ActionSweepEnd, got[4].Action())

	for _, ev := range got[2:4] {
		assert.True(t, ev.Enriched(shared.EnrichedInstance|shared.EnrichedNetworks),
			"a pass reads the networks too, so what it announces is complete")
		assert.Equal(t, incusapi.EventLifecycleInstanceUpdated, ev.Action())
	}
}

// TestSweepFillsTheWires is what makes EnrichedNetworks mean anything: the pass
// reads the networks, so an instance read after it lands under a known wire.
func TestSweepFillsTheWires(t *testing.T) {
	h := newHarness(t)

	h.mu.Lock()
	h.fleet = testlib.NewProject("p", 1, 1)
	h.mu.Unlock()

	// connected, the bracket, the one instance, the closing bracket.
	h.send(shared.ActionConnected, "", "")
	h.collect(4)

	// Now a single instance read finds the wire the pass put there.
	h.send(incusapi.EventLifecycleInstanceUpdated, "p", testlib.InstanceName(0))

	out := h.next()
	require.True(t, out.Enriched(shared.EnrichedNetworks))

	found := false
	for _, net := range out.Networks() {
		found = true

		assert.Equal(t, testlib.NetworkName(0), net.Name())
		assert.NotEmpty(t, net.Prefixes(), "the wire carries the subnet the pass read")
	}

	assert.True(t, found, "the instance sits on the network the pass knows about")
}

// TestSweepFillsTheProjectLabels is the same thing for the other half of what a
// name is built from. The pass reads each project's default profile, so an
// instance action carries its project's own settings without any read of them.
//
// Both doors, because they are two: the pass pushes what it found straight into
// the queue, and an event arriving afterwards goes round through accept.
func TestSweepFillsTheProjectLabels(t *testing.T) {
	h := newHarness(t)

	fleet := testlib.NewProject("p", 1, 1)
	fleet.Project.Config["user.label.coredns.zone"] = "example.test"

	h.mu.Lock()
	h.fleet = fleet
	h.mu.Unlock()

	h.send(shared.ActionConnected, "", "")

	// connected, the bracket, the one instance, the closing bracket. The
	// instance is the one the pass pushed itself.
	got := h.collect(4)

	swept := got[2]
	require.True(t, swept.Enriched(shared.EnrichedProject),
		"the pass read the project and handed its instance over without the labels")
	assert.Equal(t, map[string]string{"user.label.coredns.zone": "example.test"},
		swept.ProjectMetadatas())

	// And an event arriving after it, which takes the other door.
	h.send(incusapi.EventLifecycleInstanceUpdated, "p", testlib.InstanceName(0))

	out := h.next()
	require.True(t, out.Enriched(shared.EnrichedProject))
	assert.Equal(t, "example.test", must(out.ProjectMetadata("user.label.coredns.zone")))
}

// TestProjectLabelsWaitForTheProjectToBeRead pins the difference between "this
// project sets none" and "this project has not been read". Enriching with an
// empty map would make them one, and a consumer acting on the first would prune
// a name the second only has not heard about yet.
func TestProjectLabelsWaitForTheProjectToBeRead(t *testing.T) {
	h := newHarness(t)

	// No pass has run, so the model knows no project at all.
	h.send(incusapi.EventLifecycleInstanceUpdated, "p", testlib.InstanceName(0))

	out := h.next()

	assert.False(t, out.Enriched(shared.EnrichedProject),
		"a project nothing has read was reported as read")
	assert.Empty(t, out.ProjectMetadatas())
}

// must is the value of a two-value read the test has already required.
func must(value string, _ bool) string { return value }

// TestFailedReadPullsASweepIn is the retry policy, and the whole of it: the
// event fails fast so the line keeps moving, the key is noted, and a pass
// repairs it.
func TestFailedReadPullsASweepIn(t *testing.T) {
	h := newHarness(t)

	h.mu.Lock()
	h.err = errors.New("incusd said no")
	h.fleet = testlib.NewProject("p", 1, 1)
	h.mu.Unlock()

	h.send(incusapi.EventLifecycleInstanceUpdated, "p", testlib.InstanceName(0))

	failed := h.next()
	require.Equal(t, shared.StateFailed, failed.State(), "the event does not wait on a retry")

	// dirtyDelay later, a pass runs on its own - nothing asked it to.
	require.Eventually(t, func() bool { return h.lists.Load() > 0 },
		10*time.Second, 10*time.Millisecond, "a failed read pulls a pass in")

	got := h.collect(3)
	assert.Equal(t, shared.ActionSweepStart, got[0].Action())
	assert.True(t, got[1].Enriched(shared.EnrichedInstance), "and the pass repairs what failed")
	assert.Equal(t, shared.ActionSweepEnd, got[2].Action())
}

// TestOneSweepAtATime: a second pass would read the same fleet for the same
// answer, and its events would interleave with the first's, so absence would
// stop meaning anything.
func TestOneSweepAtATime(t *testing.T) {
	h := newHarness(t)

	h.mu.Lock()
	h.fleet = testlib.NewProject("p", 1, 1)
	h.gate = make(chan struct{})
	gate := h.gate
	h.mu.Unlock()

	// The pass shares the gate with the instance reads, so it is held out.
	h.send(shared.ActionConnected, "", "")
	h.send(shared.ActionConnected, "", "")

	require.Equal(t, shared.ActionConnected, h.next().Action())
	require.Equal(t, shared.ActionConnected, h.next().Action())

	close(gate)

	got := h.collect(3)
	assert.Equal(t, shared.ActionSweepStart, got[0].Action())
	assert.Equal(t, shared.ActionSweepEnd, got[2].Action())

	assert.Equal(t, int32(1), h.lists.Load(), "two reasons, one pass")
}

// TestGoneIsNotAFailure: an instance read after it went is the ordinary race
// between an event and the delete that overtook it, not something to repair.
func TestGoneIsNotAFailure(t *testing.T) {
	h := newHarness(t)

	h.mu.Lock()
	h.err = incusapi.StatusErrorf(http.StatusNotFound, "instance not found")
	h.mu.Unlock()

	h.send(incusapi.EventLifecycleInstanceUpdated, "p", "one")

	ev := h.next()

	assert.Equal(t, shared.StateOk, ev.State(), "nothing failed; it is simply not there")
	assert.False(t, ev.Enriched(shared.EnrichedInstance), "and nothing pretends to have landed")

	// No pass is owed: re-reading something that no longer exists repairs
	// nothing, so the fleet is left alone.
	assert.Never(t, func() bool { return h.lists.Load() > 0 },
		2*dirtyDelay, 50*time.Millisecond, "a gone instance pulls in no pass")
}

// TestSweepReadsProjectLabels: the project's own settings come off the project,
// which is what `incus project set` writes.
func TestSweepReadsProjectLabels(t *testing.T) {
	h := newHarness(t)

	fleet := testlib.NewProject("p", 1, 1)
	testlib.Label(fleet.Project.Config, "zone", "internal")

	h.mu.Lock()
	h.fleet = fleet
	h.mu.Unlock()

	h.send(shared.ActionConnected, "", "")
	h.collect(4)

	h.stop()

	zone, ok := h.p.m.projects["p"][testlib.LabelPrefix+"zone"]
	require.True(t, ok, "the pass patched the project in")
	assert.Equal(t, "internal", zone)
}

// TestFleetLabelsTakesTheProjectNotItsProfile is the whole of what a per-project
// setting rests on.
//
// The default profile is the wrong place to look even though a project always
// has one: its keys are already in every instance's expanded configuration, so
// reading them as the project's says nothing the instance did not, and leaves
// `incus project set user.label.coredns.zone=...` with nowhere to land.
func TestFleetLabelsTakesTheProjectNotItsProfile(t *testing.T) {
	projects := []incusapi.Project{
		{
			Name: "shop",
			ProjectPut: incusapi.ProjectPut{Config: map[string]string{
				testlib.LabelPrefix + "zone": "my.zone.com",
				"features.networks":          "true",
			}},
		},
		{
			Name:       "blog",
			ProjectPut: incusapi.ProjectPut{Config: map[string]string{}},
		},
	}

	got := fleetLabels(projects)

	assert.Equal(t, "my.zone.com", got["shop"][testlib.LabelPrefix+"zone"])

	// Handed over whole, ours and everybody else's. Picking out what a key means
	// is the consumer's, which is what lets one read answer coredns and operator.
	assert.Equal(t, "true", got["shop"]["features.networks"])

	// A project setting nothing is still a project that has been read, which is
	// what tells "sets none" from "not read yet" downstream.
	require.Contains(t, got, "blog")
	assert.Empty(t, got["blog"])
}

// seeded runs one pass so the model holds a fleet, and returns the harness.
func seeded(t *testing.T, instances, networks int) *harness {
	t.Helper()

	h := newHarness(t)

	h.mu.Lock()
	h.fleet = testlib.NewProject("p", instances, networks)
	h.mu.Unlock()

	h.send(shared.ActionConnected, "", "")

	// connected, the bracket, one event per instance, the closing bracket.
	h.collect(3 + instances)

	return h
}

// TestNetworkUpdatePatchesAndFansOut: a wire is shared, so a subnet moving
// changes every record on it. The event says so and the re-reads follow.
func TestNetworkUpdatePatchesAndFansOut(t *testing.T) {
	h := seeded(t, 2, 1)

	before := h.readsOf("p", testlib.InstanceName(0))

	h.send(incusapi.EventLifecycleNetworkUpdated, "p", testlib.NetworkName(0))

	// The change first, then what it caused.
	assert.Equal(t, incusapi.EventLifecycleNetworkUpdated, h.next().Action())

	fanned := []string{h.next().Name(), h.next().Name()}
	assert.ElementsMatch(t,
		[]string{testlib.InstanceName(0), testlib.InstanceName(1)}, fanned,
		"everything on the wire is re-read")

	assert.Greater(t, h.readsOf("p", testlib.InstanceName(0)), before,
		"and re-read means read, not guessed")
}

// TestNetworkDeleteForgetsTheWire: the wire goes, and what was on it is re-read
// so nothing is left holding addresses under a key that describes nothing.
func TestNetworkDeleteForgetsTheWire(t *testing.T) {
	h := seeded(t, 1, 1)

	h.send(incusapi.EventLifecycleNetworkDeleted, "p", testlib.NetworkName(0))

	assert.Equal(t, incusapi.EventLifecycleNetworkDeleted, h.next().Action())
	assert.Equal(t, testlib.InstanceName(0), h.next().Name())

	// The model belongs to Run's goroutine, so it is only safe to look at once
	// Run has returned. That single owner is the reason nothing here is locked.
	h.stop()

	assert.NotContains(t, h.p.m.wires, key("p", testlib.NetworkName(0)), "the wire is gone")
}

// TestNetworkRenameDropsTheOldKey: a rename leaves the old key behind whatever
// else happens - the wire is still there, but not under the name its addresses
// were filed under.
func TestNetworkRenameDropsTheOldKey(t *testing.T) {
	h := seeded(t, 1, 1)

	ev := shared.NewEvent(time.Now(), incusapi.EventLifecycleNetworkRenamed,
		"p", "renamed", testlib.NetworkName(0))
	h.p.Handle(ev)

	assert.Equal(t, incusapi.EventLifecycleNetworkRenamed, h.next().Action())

	require.Eventually(t, func() bool { return h.netReadsOf("p", "renamed") > 0 },
		2*time.Second, time.Millisecond, "the new name is read")

	h.stop()

	assert.NotContains(t, h.p.m.wires, key("p", testlib.NetworkName(0)),
		"and the old key does not survive it")
}

// TestProfileUpdateFansOut: a profile re-expands the configuration of every
// instance using it, and the event names none of them.
func TestProfileUpdateFansOut(t *testing.T) {
	h := seeded(t, 3, 1)

	h.send(incusapi.EventLifecycleProfileUpdated, "p", "default")

	assert.Equal(t, incusapi.EventLifecycleProfileUpdated, h.next().Action())

	fanned := []string{h.next().Name(), h.next().Name(), h.next().Name()}
	assert.ElementsMatch(t, []string{
		testlib.InstanceName(0), testlib.InstanceName(1), testlib.InstanceName(2),
	}, fanned, "every instance in the project, and the project was not read to find them")

	assert.Equal(t, int32(1), h.lists.Load(), "no pass was needed for that")
}

// TestProfileUpdateInAnEmptyProjectCostsNothing: nothing to re-expand, nothing
// to read.
func TestProfileUpdateInAnEmptyProjectCostsNothing(t *testing.T) {
	h := seeded(t, 1, 1)

	h.send(incusapi.EventLifecycleProfileUpdated, "elsewhere", "default")

	assert.Equal(t, incusapi.EventLifecycleProfileUpdated, h.next().Action())
	assert.Zero(t, h.readsOf("elsewhere", testlib.InstanceName(0)))
}

// TestFailingReadsDoNotStarveThePass: pulling a pass in may never push it out.
// A read failing every second would otherwise keep the repair permanently five
// seconds away, and the one thing that repairs a failed read would never run.
func TestFailingReadsDoNotStarveThePass(t *testing.T) {
	h := newHarness(t)

	h.mu.Lock()
	h.err = errors.New("incusd said no")
	h.fleet = testlib.NewProject("p", 1, 1)
	h.mu.Unlock()

	// Steadily, and faster than the pass is ever due.
	stop := make(chan struct{})
	defer close(stop)

	go func() {
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			case <-time.After(dirtyDelay / 5):
				h.send(incusapi.EventLifecycleInstanceUpdated, "p", testlib.InstanceName(0))
			}
		}
	}()

	require.Eventually(t, func() bool { return h.lists.Load() > 0 },
		4*dirtyDelay, 20*time.Millisecond,
		"the pass runs despite failures arriving faster than its delay")
}
