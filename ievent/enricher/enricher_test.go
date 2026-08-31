package enricher

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	incusapi "github.com/lxc/incus/v7/shared/api"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lxc/incus-compose/iclient"
	"github.com/lxc/incus-compose/ievent/iutil"
	"github.com/lxc/incus-compose/internal/testlib"
)

// wanted is the table the source would have built.
var wanted = map[string]iutil.Want{
	incusapi.EventLifecycleInstanceUpdated: {
		Action: incusapi.EventLifecycleInstanceUpdated,
		Enrich: iutil.EnrichedInstance | iutil.EnrichedNetwork | iutil.EnrichedProject,
	},
	incusapi.EventLifecycleInstanceDeleted: {Action: incusapi.EventLifecycleInstanceDeleted},

	// Wanted for its networks alone, which is what makes it the case that a
	// name does not imply an instance.
	incusapi.EventLifecycleNetworkUpdated: {
		Action: incusapi.EventLifecycleNetworkUpdated,
		Enrich: iutil.EnrichedNetwork,
	},
	incusapi.EventLifecycleNetworkDeleted: {Action: incusapi.EventLifecycleNetworkDeleted},
	incusapi.EventLifecycleNetworkRenamed: {Action: incusapi.EventLifecycleNetworkRenamed},
	incusapi.EventLifecycleProfileUpdated: {Action: incusapi.EventLifecycleProfileUpdated},
	incusapi.EventLifecycleProfileDeleted: {Action: incusapi.EventLifecycleProfileDeleted},
}

// fixture wires one plugin to a collecting successor and runs it the way main
// would, answering its reads from testlib rather than a daemon.
type fixture struct {
	t   *testing.T
	p   *Plugin
	out chan *iutil.Event

	cancel context.CancelFunc
	wg     sync.WaitGroup

	// in and raised are the two doors the source gives a plugin: one to ask it
	// something, one for it to say something.
	in     chan iutil.Command
	raised chan iutil.Command

	// forward stops the goroutine that plays the source, so a drain can read its
	// own answer off raised. forwarded says it has actually gone - closing
	// forward alone races the drain answer, since select picks at random.
	forward     chan struct{}
	forwarded   chan struct{}
	stopForward sync.Once

	mu    sync.Mutex
	reads map[string]int
	err   error

	// leaseless answers every instance read with a running instance holding no
	// address, which is what one reads between its start and its DHCP lease.
	leaseless bool

	// gate holds every read until it is closed, so a test can decide when a
	// read lands and in which order.
	gate chan struct{}

	// tag goes into every instance read, so a test can make the next read say
	// something new.
	tag string

	// noNIC answers with an instance holding no network device, so a test about
	// something else is not also a test of whether its network has been read.
	noNIC bool

	// nic is the network the answered instance's NIC names. Empty is the fleet's
	// first; a test moves it the way a rename does.
	nic string

	// own is what the instance's own configuration sets, so a test can contest
	// one key from both sides of a read.
	own map[string]string

	// fleet is what a run finds, and listErr what its listings fail with. Nil
	// fleet means every listing refuses, so no run ever reaches the chain.
	fleet   *testlib.Project
	listErr error
	lists   atomic.Int32
}

// errNoFleet is what the listings answer until a test sets one.
var errNoFleet = errors.New("this fixture has no fleet")

// GetProjectNames, Project, NetworkNames and InstanceNames are what the sweeper
// lists instead of a daemon.
func (h *fixture) GetProjectNames(_ context.Context) ([]string, error) {
	h.lists.Add(1)

	fleet, err := h.listing()
	if err != nil {
		return nil, err
	}

	return []string{fleet.Project.Name}, nil
}

func (h *fixture) GetProject(ctx context.Context, name string) (*incusapi.Project, string, error) {
	fleet, err := h.listing()
	if err != nil {
		return nil, "", err
	}

	if name != fleet.Project.Name {
		return nil, "", incusapi.StatusErrorf(http.StatusNotFound, "no such project")
	}

	return &fleet.Project, "", nil
}

func (h *fixture) GetNetworkNames(_ context.Context, project string) ([]string, error) {
	fleet, err := h.listing()
	if err != nil {
		return nil, err
	}

	var out []string

	for _, net := range fleet.Networks {
		if net.Project == project {
			out = append(out, net.Name)
		}
	}

	return out, nil
}

func (h *fixture) GetInstanceNames(_ context.Context, project string, _ *iclient.GetInstancesArgs) ([]string, error) {
	fleet, err := h.listing()
	if err != nil {
		return nil, err
	}

	var out []string

	for _, inst := range fleet.Instances {
		if inst.Project == project {
			out = append(out, inst.Name)
		}
	}

	return out, nil
}

// listing is what every listing above starts with.
func (h *fixture) listing() (*testlib.Project, error) {
	h.mu.Lock()
	defer h.mu.Unlock()

	if h.listErr != nil {
		return nil, h.listErr
	}

	if h.fleet == nil {
		return nil, errNoFleet
	}

	return h.fleet, nil
}

// retag makes every instance read from here on say something new.
func (h *fixture) retag(tag string) {
	h.mu.Lock()
	defer h.mu.Unlock()

	h.tag = tag
}

// setFleet gives the fixture a fleet and starts a run over it, the way a
// reconnect does.
//
// The run Run already started is out there refusing, and it has to have refused
// once before the fleet appears: one that finds a fleet reaches the end of a run
// the reconnect below is about to start again, and the chain sees two ends
// rather than one.
func (h *fixture) setFleet(fleet *testlib.Project) {
	h.t.Helper()

	require.Eventually(h.t, func() bool { return h.lists.Load() > 0 },
		2*time.Second, time.Millisecond, "the run Run started never asked for a fleet")

	h.mu.Lock()
	h.fleet = fleet
	h.mu.Unlock()

	h.send(iutil.ActionConnected, "", "")
}

func newFixture(t *testing.T, opts ...Option) *fixture {
	t.Helper()

	// Not t.Context(): the testing package cancels that just before cleanups
	// run, so a fixture that drains in a cleanup would find the plugin already
	// aborted and waiting for an answer it can no longer give.
	ctx, cancel := context.WithCancel(context.Background())
	h := &fixture{
		t:         t,
		p:         New(append([]Option{Workers(8), ReadTimeout(time.Second)}, opts...)...),
		out:       make(chan *iutil.Event, 64),
		in:        make(chan iutil.Command),
		raised:    make(chan iutil.Command, 8),
		forward:   make(chan struct{}),
		forwarded: make(chan struct{}),
		cancel:    cancel,
		reads:     map[string]int{},
		own:       map[string]string{},
	}

	h.p.reads.read = h.answer
	h.p.reads.readNet = h.answerNet
	h.p.reads.readProject = h.GetProject
	h.p.reads.fleet = h

	err := h.p.Setup(iutil.SetupArgs{
		Context:    ctx,
		Wanted:     wanted,
		Next:       func(ev *iutil.Event) { h.out <- ev },
		CommandIn:  h.in,
		CommandOut: h.raised,
	})
	require.NoError(t, err)

	// What the source does with a raised action: creates it and put it in at the
	// head, which is the enricher's own door with nothing before it here.
	h.wg.Go(func() {
		defer close(h.forwarded)

		for {
			select {
			case cmd := <-h.raised:
				h.p.Handle(iutil.NewEvent(time.Now(), cmd.Action, "", "", ""))
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
// so a test can assert on what the shutdown handed on. Ask, wait for the
// answer, then cancel - canceling first is an abort, and everything held would
// go nowhere.
func (h *fixture) stop() {
	h.stopForward.Do(func() {
		// The goroutine playing the source has to stop reading raised first,
		// and be gone rather than merely told to go, or it can still take the
		// answer below instead of us.
		close(h.forward)

		select {
		case <-h.forwarded:
		case <-time.After(5 * time.Second):
			h.t.Error("the goroutine playing the source never stopped")

			return
		}

		select {
		case h.in <- iutil.Command{Action: iutil.CommandDrain}:
		case <-time.After(5 * time.Second):
			h.t.Error("the enricher never took the drain")

			return
		}

		for {
			select {
			case cmd := <-h.raised:
				if cmd.Action != iutil.CommandDrain {
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
func (h *fixture) answer(
	ctx context.Context,
	project, name string,
) (*incusapi.Instance, *incusapi.InstanceState, error) {
	h.mu.Lock()
	h.reads[project+"/"+name]++
	gate, failWith, leaseless, tag, noNIC, nic := h.gate, h.err, h.leaseless, h.tag, h.noNIC, h.nic
	own := maps.Clone(h.own)
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
	inst.ExpandedConfig["user.label."+testlib.LabelPrefix+"service"] = name

	for key, value := range own {
		inst.ExpandedConfig["user.label."+key] = value
	}

	if tag != "" {
		inst.ExpandedConfig["user.label."+testlib.LabelPrefix+"tag"] = tag
	}

	if nic != "" {
		inst.ExpandedDevices["eth0"]["network"] = nic
	}

	if noNIC {
		inst.ExpandedDevices = map[string]map[string]string{}
	}

	state := testlib.NewInstanceState(0, 0)
	if leaseless {
		state.Network["eth0"] = incusapi.InstanceStateNetwork{Type: "broadcast"}
	}

	return &inst, state, nil
}

// answerNet is what a pool worker calls instead of reading one network.
//
// Out of the fleet rather than made up on the spot: a name the fleet does not
// have answers the way Incus does, or a read of a network that has just been
// renamed away would put it back.
func (h *fixture) answerNet(_ context.Context, project, name string) (*incusapi.Network, string, error) {
	h.mu.Lock()
	// Counted apart from the instance reads: the two are keyed the same way, and
	// telling them apart is the whole point of one of the tests below.
	h.reads["net:"+project+"/"+name]++
	failWith := h.err
	h.mu.Unlock()

	if failWith != nil {
		return nil, "", failWith
	}

	fleet, err := h.listing()
	if err != nil {
		return nil, "", err
	}

	for i := range fleet.Networks {
		if fleet.Networks[i].Name == name {
			net := fleet.Networks[i]

			return &net, "", nil
		}
	}

	return nil, "", incusapi.StatusErrorf(http.StatusNotFound, "no such network")
}

// readsOf is how many instance reads one key has cost.
func (h *fixture) readsOf(project, name string) int {
	h.mu.Lock()
	defer h.mu.Unlock()

	return h.reads[project+"/"+name]
}

// netReadsOf is how many network reads one key has cost.
func (h *fixture) netReadsOf(project, name string) int {
	h.mu.Lock()
	defer h.mu.Unlock()

	return h.reads["net:"+project+"/"+name]
}

func (h *fixture) send(action, project, name string) {
	h.t.Helper()

	h.p.Handle(iutil.NewEvent(time.Now(), action, project, name, ""))
}

func (h *fixture) next() *iutil.Event {
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
	t.Parallel()

	tests := []struct {
		name   string
		arrive []string
	}{
		{name: "one instance", arrive: []string{"a"}},
		{name: "several instances", arrive: []string{"a", "b", "c", "d"}},
		// One key repeatedly is not an order case: reads after the first find
		// what it found, and only it walks. See TestAnUnchangedInstanceEventGoesNoFurther.
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			h := seeded(t, 0, 0)

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
	t.Parallel()

	tests := []struct {
		name   string
		action string
		who    string
	}{
		{name: "an action with nothing to enrich", action: incusapi.EventLifecycleInstanceDeleted, who: "one"},
		{name: "an action nobody wanted", action: incusapi.EventLifecycleInstanceMigrated, who: "one"},
		{name: "the source's own, which carries no name", action: iutil.ActionConnected},
		{name: "the end of a run", action: iutil.ActionSweepEnd},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			h := newFixture(t)

			h.send(tc.action, "p", tc.who)

			ev := h.next()
			assert.Equal(t, tc.action, ev.Action())
			assert.NoError(t, ev.Err())
		})
	}
}

// TestAlreadyFinishedEventsAreUntouched: an event a plugin before this one is
// done with is walking for the observers, and enriching it would be a read
// nobody asked for.
func TestAlreadyFinishedEventsAreUntouched(t *testing.T) {
	t.Parallel()

	h := newFixture(t)

	ev := iutil.NewEvent(time.Now(), incusapi.EventLifecycleInstanceUpdated, "p", "one", "")
	h.p.Handle(ev.WithDropped("debounce"))

	out := h.next()
	assert.ErrorIs(t, out.Err(), iutil.ErrDropped)
	assert.Equal(t, "debounce", droppedBy(t, out), "the first reason is the one that stands")
}

// TestShutdownHandsOnWhatItHolds: nothing this plugin accepted may be swallowed
// on the way out, because an event the chain never saw is worse than a late one.
func TestShutdownHandsOnWhatItHolds(t *testing.T) {
	t.Parallel()

	h := newFixture(t)

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
	t.Parallel()

	// No Run, so nothing drains the inbox: Handle has to cope on its own.
	seen := []*iutil.Event{}
	p := New()
	p.args.Next = func(ev *iutil.Event) { seen = append(seen, ev) }

	for range defaultInboxSize {
		p.Handle(iutil.NewEvent(time.Now(), incusapi.EventLifecycleInstanceUpdated, "p", "one", ""))
	}

	require.Empty(t, seen, "nothing dropped before the inbox was full")

	p.Handle(iutil.NewEvent(time.Now(), incusapi.EventLifecycleInstanceUpdated, "p", "two", ""))

	require.Len(t, seen, 1)
	assert.ErrorIs(t, seen[0].Err(), iutil.ErrDropped)
	assert.Equal(t, name, droppedBy(t, seen[0]), "and says who dropped it")
}

// TestEnrichesFromTheRead is the point of the plugin: what leaves carries what
// the read found, and says so.
func TestEnrichesFromTheRead(t *testing.T) {
	t.Parallel()

	h := seeded(t, 0, 0)

	h.send(incusapi.EventLifecycleInstanceUpdated, "p", "one")

	ev := h.next()

	assert.NoError(t, ev.Err())
	assert.True(t, ev.Enriched(iutil.EnrichedInstance), "the instance read landed")

	inst := ev.Instance()
	assert.True(t, inst.Running())

	service, ok := inst.ConfigValue("user.label." + testlib.LabelPrefix + "service")
	assert.True(t, ok)
	assert.Equal(t, "one", service, "the configuration came off the instance that was read")
}

// TestFailedReadFails is rule 7: what asked for something and did not get it
// says so, rather than arriving looking complete.
func TestFailedReadFails(t *testing.T) {
	t.Parallel()

	h := seeded(t, 0, 0)

	h.mu.Lock()
	h.err = errors.New("incusd said no")
	h.mu.Unlock()

	h.send(incusapi.EventLifecycleInstanceUpdated, "p", "one")

	ev := h.next()

	assert.ErrorIs(t, ev.Err(), iutil.ErrFailed)
	assert.ErrorIs(t, ev.Err(), errRead, "an actor, not a bare cause")
	assert.False(t, ev.Enriched(iutil.EnrichedInstance), "and nothing pretends to have landed")
}

// TestCoalescesReadsPerKey: coalescing saves the read, not the event. Two
// events on one key cost one read and both still walk, carrying what it found.
func TestCoalescesReadsPerKey(t *testing.T) {
	t.Parallel()

	h := seeded(t, 0, 0)

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

	// After both of them, so it is what arrives second only if the coalesced
	// one did not walk.
	h.send(incusapi.EventLifecycleInstanceUpdated, "p", "two")

	close(gate)

	first := h.next()

	assert.True(t, first.Enriched(iutil.EnrichedInstance))
	assert.Equal(t, "one", first.Name())
	assert.Equal(t, 1, h.readsOf("p", "one"), "it was one read")

	// Both settled from that read, and the second says what the first said.
	assert.Equal(t, "two", h.next().Name(), "the coalesced event walked with nothing new on it")
}

// TestSlowReadHoldsTheLine is the ordering cost, stated: reads run
// concurrently, delivery does not, so a slow read at the front holds up
// everything after it.
func TestSlowReadHoldsTheLine(t *testing.T) {
	t.Parallel()

	h := seeded(t, 0, 0)

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
		time.Second, time.Millisecond, "the reads after it ran anyway")

	assert.Empty(t, h.out, "and none of them left")

	close(gate)

	for _, n := range []string{"slow", "a", "b"} {
		assert.Equal(t, n, h.next().Name(), "then all of it, in arrival order")
	}
}

// TestDeleteCostsNoRead is rule 3: a delete says the subject is gone, and
// reading to confirm it would be a read whose answer we already have.
func TestDeleteCostsNoRead(t *testing.T) {
	t.Parallel()

	h := seeded(t, 0, 0)

	h.send(incusapi.EventLifecycleInstanceUpdated, "p", "one")
	require.True(t, h.next().Enriched(iutil.EnrichedInstance))

	h.send(incusapi.EventLifecycleInstanceDeleted, "p", "one")

	ev := h.next()
	assert.Equal(t, incusapi.EventLifecycleInstanceDeleted, ev.Action())
	assert.Equal(t, 1, h.readsOf("p", "one"), "the delete added no read")
}

// TestOnlyInstanceActionsAreRead: a name is not enough to make something an
// instance. A network action carries one too, and reading an instance called
// net0 is the mistake this guards.
func TestOnlyInstanceActionsAreRead(t *testing.T) {
	t.Parallel()

	h := newFixture(t)

	h.send(incusapi.EventLifecycleNetworkUpdated, "p", "net0")

	ev := h.next()

	assert.Equal(t, incusapi.EventLifecycleNetworkUpdated, ev.Action())
	assert.NoError(t, ev.Err())
	assert.Zero(t, h.readsOf("p", "net0"), "no instance read for a network's name")

	// It is read, just as the thing it actually is.
	require.Eventually(t, func() bool { return h.netReadsOf("p", "net0") == 1 },
		2*time.Second, time.Millisecond, "read as a network instead")
}

// must is the value of a two-value read the test has already required.
func must(value string, _ bool) string { return value }

// bare is a plugin with its read pool open and nothing else running, for a test
// that drives one fold step itself rather than through Run.
func bare(t *testing.T) *Plugin {
	t.Helper()

	p := New()
	p.args.Next = func(*iutil.Event) {}

	require.NoError(t, p.reads.start(1))
	t.Cleanup(p.reads.stop)

	p.reads.readProject = func(_ context.Context, name string) (*incusapi.Project, string, error) {
		return &incusapi.Project{Name: name}, "", nil
	}

	return p
}

// droppedBy is who finished one event with, off the sentinel it carries.
func droppedBy(t *testing.T, ev *iutil.Event) string {
	t.Helper()

	var err *iutil.Error

	require.ErrorAs(t, ev.Err(), &err)

	return err.By()
}

// collect takes n events off the far end.
func (h *fixture) collect(n int) []*iutil.Event {
	h.t.Helper()

	out := make([]*iutil.Event, 0, n)
	for range n {
		out = append(out, h.next())
	}

	return out
}

// TestConnectedSweeps: a stream coming up makes everything held suspect at
// once, because whatever happened while it was down was announced to nobody.
func TestConnectedSweeps(t *testing.T) {
	t.Parallel()

	h := newFixture(t)

	fleet := testlib.NewProject("p", 2, 1)
	fleet.Project.Config[iutil.FeaturesNetworks] = "true"

	h.setFleet(fleet)

	// Every name the run trickled, then the end of it. No opening bracket:
	// there is no duration to bracket, only an instant at the end.
	got := h.collect(4)

	assert.Equal(t, iutil.ActionConnected, got[0].Action())

	names := []string{got[1].Name(), got[2].Name()}
	assert.ElementsMatch(t, []string{testlib.InstanceName(0), testlib.InstanceName(1)}, names)

	assert.Equal(t, iutil.ActionSweepEnd, got[3].Action())

	for _, ev := range got[1:3] {
		assert.True(t, ev.Enriched(iutil.EnrichedInstance|iutil.EnrichedInstanceWithInterfaces),
			"a run reads the networks before the instances, so what it hands over is complete")
		assert.Equal(t, incusapi.EventLifecycleInstanceUpdated, ev.Action())
	}
}

// TestSweepFillsTheNetworks is what makes EnrichedNetworks mean anything: the
// run reads the networks first, so an instance read after it lands under a
// known network.
func TestSweepFillsTheNetworks(t *testing.T) {
	t.Parallel()

	h := seeded(t, 1, 1)

	// Now a single instance read finds the network the run put there. Tagged, or
	// the run has already filed what this read finds and it goes nowhere.
	h.retag("moved")
	h.send(incusapi.EventLifecycleInstanceUpdated, "p", testlib.InstanceName(0))

	out := h.next()
	require.True(t, out.Enriched(iutil.EnrichedInstanceWithInterfaces))

	on := slices.Collect(out.Instance().Interfaces())
	require.Len(t, on, 1, "the instance sits on the network the run knows about")

	assert.Equal(t, testlib.NetworkName(0), on[0].Network())
	assert.Equal(t, "p", on[0].Project(), "keyed by the project that owns the bridge")
	assert.True(t, on[0].Managed(), "the run read the network, not just its name")
	assert.Equal(t, []string{testlib.Address(0, 0)}, on[0].IPv4())
}

// TestSweepFillsTheProject is the same thing for the other half of what an
// event is built from. A run reads each project, so an instance action carries
// its project's own configuration without any read of it.
func TestSweepFillsTheProject(t *testing.T) {
	t.Parallel()

	h := newFixture(t)

	fleet := testlib.NewProject("p", 1, 1)
	fleet.Project.Config[iutil.FeaturesNetworks] = "true"
	fleet.Project.Config["dns.zone"] = "example.test"

	h.setFleet(fleet)

	// connected, the one instance the run trickled, the end of the run.
	got := h.collect(3)

	require.True(t, got[1].Enriched(iutil.EnrichedProject),
		"the run read the project and handed its instance over without it")
	assert.Equal(t, "example.test", must(got[1].Project().ConfigValue("dns.zone")))

	// And an event arriving after it, which takes the other door.
	h.retag("moved")
	h.send(incusapi.EventLifecycleInstanceUpdated, "p", testlib.InstanceName(0))

	out := h.next()
	require.True(t, out.Enriched(iutil.EnrichedProject))
	assert.Equal(t, "example.test", must(out.Project().ConfigValue("dns.zone")))
}

// TestProjectWaitsForTheProjectToBeRead pins the difference between "this
// project sets none" and "this project has not been read" - an empty map would
// make them one, and a consumer acting on the first would prune the second.
func TestProjectWaitsForTheProjectToBeRead(t *testing.T) {
	t.Parallel()

	h := seeded(t, 0, 0)

	// No NIC, so this stays a test about the project rather than about whether
	// the network its NIC would name has been read yet.
	h.mu.Lock()
	h.noNIC = true
	h.mu.Unlock()

	// The run read project "p", never "other" - so an instance event naming
	// it finds no project the same way one would before any run had happened.
	h.send(incusapi.EventLifecycleInstanceUpdated, "other", testlib.InstanceName(0))

	out := h.next()

	assert.False(t, out.Enriched(iutil.EnrichedProject),
		"a project nothing has read was reported as read")
	assert.Nil(t, out.Project(), "and it carried a project all the same")
	assert.True(t, out.Enriched(iutil.EnrichedInstance),
		"the instance read landed either way")
}

// TestFailedReadIsReadAgain is the first half of the retry policy: the event
// fails fast so the line keeps moving, and only the failed key is read again.
func TestFailedReadIsReadAgain(t *testing.T) {
	t.Parallel()

	h := seeded(t, 1, 1)

	h.mu.Lock()
	h.err = errors.New("incusd said no")
	h.mu.Unlock()

	before := h.lists.Load()

	h.send(incusapi.EventLifecycleInstanceUpdated, "p", testlib.InstanceName(0))

	failed := h.next()
	require.ErrorIs(t, failed.Err(), iutil.ErrFailed, "the event does not wait on a retry")

	require.Eventually(t, func() bool { return h.readsOf("p", testlib.InstanceName(0)) > 1 },
		5*time.Second, 10*time.Millisecond, "the key that failed was never read again")

	assert.Equal(t, before, h.lists.Load(), "one key that failed pulled a run in")
}

// TestUnleasedInstanceIsReadAgain: a lease lands after the start event, and a
// network-updated fan-out skips an instance holding no address on that network -
// so nothing else would ever ask again without rereadSoon.
func TestUnleasedInstanceIsReadAgain(t *testing.T) {
	t.Parallel()

	h := seeded(t, 1, 1)

	h.mu.Lock()
	h.leaseless = true
	h.mu.Unlock()

	before := h.lists.Load()

	h.send(incusapi.EventLifecycleInstanceUpdated, "p", testlib.InstanceName(0))

	served := h.next()
	require.NoError(t, served.Err(), "the read landed, it just found no address")

	require.Eventually(t, func() bool { return h.readsOf("p", testlib.InstanceName(0)) > 1 },
		5*time.Second, 10*time.Millisecond, "an instance with no address was never read again")

	assert.Equal(t, before, h.lists.Load(), "one address-less instance cost a whole-fleet pass")
}

// TestFanOutOverAnUnchangedFleetEmitsNothing is what the archive is for: a network
// that moved is one event, not one per instance sitting on it, when what those
// instances look like has not changed.
func TestFanOutOverAnUnchangedFleetEmitsNothing(t *testing.T) {
	t.Parallel()

	h := seeded(t, 2, 1)

	// The run that seeded this filed both instances as they read now, so the
	// fan-out below reads the same thing about each of them.
	h.send(incusapi.EventLifecycleNetworkUpdated, "p", testlib.NetworkName(0))

	require.Equal(t, incusapi.EventLifecycleNetworkUpdated, h.next().Action())

	require.Eventually(t, func() bool {
		return h.readsOf("p", testlib.InstanceName(0)) == 2 &&
			h.readsOf("p", testlib.InstanceName(1)) == 2
	}, 5*time.Second, 10*time.Millisecond, "the fan-out never read them")

	assert.Empty(t, h.out, "a fleet that did not move emitted an event per instance")
}

// TestAnUnchangedInstanceEventGoesNoFurther pins the other half: volatile keys
// are stripped before the comparison, so a lease renewal is an instance-updated
// whose distilled value did not move.
func TestAnUnchangedInstanceEventGoesNoFurther(t *testing.T) {
	t.Parallel()

	h := seeded(t, 1, 1)

	h.retag("moved")

	h.send(incusapi.EventLifecycleInstanceUpdated, "p", testlib.InstanceName(0))
	require.NoError(t, h.next().Err())

	h.send(incusapi.EventLifecycleInstanceUpdated, "p", testlib.InstanceName(0))

	// After it, so this is what arrives next only if the one before it did not.
	// Asserting on an empty channel would pass before it could have arrived.
	h.send(incusapi.EventLifecycleInstanceUpdated, "p", "other")

	assert.Equal(t, "other", h.next().Name(),
		"an event saying what the last one said walked anyway")
}

// TestADeleteAfterAReadKeepsItsOrder pins the worst case for overtaking: a
// delete needs no read, so it settles at once while the update before it
// is still out. The delete also drops what the archive held, so the update is
// news again rather than compared against a value the delete has retired.
func TestADeleteAfterAReadKeepsItsOrder(t *testing.T) {
	t.Parallel()

	h := seeded(t, 1, 1)

	// The first read is what the second one below compares equal to.
	h.retag("moved")
	h.send(incusapi.EventLifecycleInstanceUpdated, "p", testlib.InstanceName(0))
	require.NoError(t, h.next().Err())

	h.mu.Lock()
	h.gate = make(chan struct{})
	gate := h.gate
	h.mu.Unlock()

	// Held on its read, and the delete after it needs none.
	h.send(incusapi.EventLifecycleInstanceUpdated, "p", testlib.InstanceName(0))
	h.send(incusapi.EventLifecycleInstanceDeleted, "p", testlib.InstanceName(0))

	assert.Empty(t, h.out, "the delete went out while the event before it was still reading")

	close(gate)

	assert.Equal(t, incusapi.EventLifecycleInstanceUpdated, h.next().Action())
	assert.Equal(t, incusapi.EventLifecycleInstanceDeleted, h.next().Action())
}

// TestALiveEventAndAFanOutOnOneKeyAreOneEvent: both settle from the same read,
// so the real one walks and the invented one has nothing left to say.
func TestALiveEventAndAFanOutOnOneKeyAreOneEvent(t *testing.T) {
	t.Parallel()

	h := seeded(t, 1, 1)

	h.mu.Lock()
	h.gate = make(chan struct{})
	h.tag = "moved"
	gate := h.gate
	h.mu.Unlock()

	// A profile fans out without a read of its own, so the instance read it
	// issues is still in flight for the live event below to join.
	h.send(incusapi.EventLifecycleProfileUpdated, "p", "default")
	require.Equal(t, incusapi.EventLifecycleProfileUpdated, h.next().Action())

	h.send(incusapi.EventLifecycleInstanceUpdated, "p", testlib.InstanceName(0))

	close(gate)

	got := h.next()
	assert.Equal(t, incusapi.EventLifecycleInstanceUpdated, got.Action())
	assert.NoError(t, got.Err(), "the event somebody sent was the one dropped")

	require.Eventually(t, func() bool { return h.readsOf("p", testlib.InstanceName(0)) == 2 },
		5*time.Second, 10*time.Millisecond, "the two did not share one read")

	assert.Empty(t, h.out, "the fan-out emitted an event of its own beside the real one")
}

// TestFailedNetworkReadIsNotAnInstanceRead pins what a re-read cannot reread:
// the fan-out creates instance-updated, so a network handed to it would have the
// daemon asked for an instance called net0. A failed network read waits for the
// run instead.
func TestFailedNetworkReadIsNotAnInstanceRead(t *testing.T) {
	t.Parallel()

	h := seeded(t, 1, 1)

	h.mu.Lock()
	h.err = errors.New("incusd said no")
	h.mu.Unlock()

	// It walks settled and ahead of the read, so what the read did is not on it.
	h.send(incusapi.EventLifecycleNetworkUpdated, "p", testlib.NetworkName(0))

	require.Equal(t, incusapi.EventLifecycleNetworkUpdated, h.next().Action())

	assert.Never(t, func() bool { return h.readsOf("p", testlib.NetworkName(0)) > 0 },
		time.Second, 20*time.Millisecond, "the network's own name was read as an instance")
}

// TestARunMeetsAnInstanceMidBoot pins the one absorb point: a run reaches
// the same conclusion about a read as the event path does, so a running
// instance holding no address gets rereaded either way it was found.
func TestARunMeetsAnInstanceMidBoot(t *testing.T) {
	t.Parallel()

	h := newFixture(t)

	fleet := testlib.NewProject("p", 1, 1)
	fleet.Project.Config[iutil.FeaturesNetworks] = "true"

	// A running instance holding no address, which is what a state read that
	// failed leaves behind and what a lease that has not landed looks like.
	h.mu.Lock()
	h.leaseless = true
	h.mu.Unlock()

	h.setFleet(fleet)

	// connected, the instance the run trickled, the end of the run.
	h.collect(3)

	before := h.readsOf("p", testlib.InstanceName(0))

	require.Eventually(t, func() bool { return h.readsOf("p", testlib.InstanceName(0)) > before },
		5*time.Second, 10*time.Millisecond, "a run met an instance mid-boot and never read it again")
}

// TestRereadSlowsDownButNeverStops is the other half, and the one the e2e
// caught: a key that spends its fast attempts drops to the slow rate rather
// than stopping and waiting for the run, which is minutes away by design.
func TestRereadSlowsDownButNeverStops(t *testing.T) {
	t.Parallel()

	h := seeded(t, 1, 1)

	before := h.readsOf("p", testlib.InstanceName(0))

	h.mu.Lock()
	h.err = errors.New("incusd said no")
	h.mu.Unlock()

	h.send(incusapi.EventLifecycleInstanceUpdated, "p", testlib.InstanceName(0))

	require.ErrorIs(t, h.next().Err(), iutil.ErrFailed)

	// The read the event asked for, and no more re-reads than it was given.
	want := before + 1 + retryTries

	require.Eventually(t, func() bool {
		return h.readsOf("p", testlib.InstanceName(0)) == want
	}, 5*time.Second, 10*time.Millisecond, "the key spent a different number of attempts")

	assert.Never(t, func() bool { return h.readsOf("p", testlib.InstanceName(0)) > want },
		2*retryDelay, 20*time.Millisecond, "the fast attempts did not slow down")

	require.Eventually(t, func() bool { return h.readsOf("p", testlib.InstanceName(0)) > want },
		3*slowRetryDelay, 50*time.Millisecond, "a key that spent its attempts was never read again")
}

// TestAReconnectRestartsTheRun: everything held is as old as the stream was
// down, so the run in flight is abandoned wherever it had got to and a fast
// one starts.
func TestAReconnectRestartsTheRun(t *testing.T) {
	t.Parallel()

	h := seeded(t, 1, 1)

	before := h.lists.Load()

	h.send(iutil.ActionConnected, "", "")

	require.Equal(t, iutil.ActionConnected, h.next().Action())

	require.Eventually(t, func() bool { return h.lists.Load() > before },
		5*time.Second, 10*time.Millisecond, "a reconnect started no run")

	// The instance is unchanged, so it emits nothing; the end of the run is
	// what says a whole one ran.
	assert.Equal(t, iutil.ActionSweepEnd, h.next().Action())
}

// TestGoneIsNotAFailure: an instance read after it went is the ordinary race
// between an event and the delete that overtook it, not something to reread.
func TestGoneIsNotAFailure(t *testing.T) {
	t.Parallel()

	h := seeded(t, 0, 0)

	h.mu.Lock()
	h.err = incusapi.StatusErrorf(http.StatusNotFound, "instance not found")
	h.mu.Unlock()

	h.send(incusapi.EventLifecycleInstanceUpdated, "p", "one")

	ev := h.next()

	assert.NoError(t, ev.Err(), "nothing failed; it is simply not there")
	assert.False(t, ev.Enriched(iutil.EnrichedInstance), "and nothing pretends to have landed")

	// Nothing is owed: re-reading a gone instance rereads nothing.
	assert.Never(t, func() bool { return h.readsOf("p", "one") > 1 },
		2*retryDelay, 20*time.Millisecond, "a gone instance was read again")
}

// TestBothSidesArriveUnmerged: the enricher attaches what it read and does not
// decide between the two. A key both sides set arrives twice, and which one
// wins is the consumer's rule rather than this plugin's.
func TestBothSidesArriveUnmerged(t *testing.T) {
	t.Parallel()

	const key = testlib.LabelPrefix + "zone"

	h := newFixture(t)
	h.own[key] = "mine"

	fleet := testlib.NewProject("p", 1, 1)
	fleet.Project.Config[key] = "theirs"
	fleet.Project.Config[iutil.FeaturesNetworks] = "true"

	h.setFleet(fleet)

	// connected, the one instance the run trickled, the end of the run.
	got := h.collect(3)

	swept := got[1]
	require.True(t, swept.Enriched(iutil.EnrichedInstance|iutil.EnrichedProject))

	assert.Equal(t, "mine", must(swept.Instance().ConfigValue("user.label."+key)),
		"the instance's own side of it was decided here")
	assert.Equal(t, "theirs", must(swept.Project().ConfigValue(key)),
		"and the project's side was contested away")
}

// TestSweepReadsProjectLabels: the project's own settings come off the project,
// which is what `incus project set` writes.
func TestSweepReadsProjectLabels(t *testing.T) {
	t.Parallel()

	h := newFixture(t)

	fleet := testlib.NewProject("p", 1, 1)
	testlib.Label(fleet.Project.Config, "zone", "internal")

	fleet.Project.Config[iutil.FeaturesNetworks] = "true"

	h.setFleet(fleet)
	h.collect(3)

	h.stop()

	config := h.p.state.projectConfig("p")
	require.NotNil(t, config, "the run patched the project in")
	assert.Equal(t, "internal", config[testlib.LabelPrefix+"zone"])
}

// TestARunTakesTheProjectNotItsProfile: the default profile is the wrong
// place to look even though a project always has one - its keys are already in
// every instance's expanded configuration, so reading them as the project's
// leaves `incus project set user.label.dns.zone=...` with nowhere to land.
func TestARunTakesTheProjectNotItsProfile(t *testing.T) {
	t.Parallel()

	h := newFixture(t)

	fleet := testlib.NewProject("shop", 0, 0)
	fleet.Project.Config[testlib.LabelPrefix+"zone"] = "my.zone.com"
	fleet.Project.Config[iutil.FeaturesNetworks] = "true"

	h.setFleet(fleet)

	// connected, then the end of the run: no instances to trickle.
	h.collect(2)

	h.stop()

	got := h.p.state.projectConfig("shop")

	assert.Equal(t, "my.zone.com", got[testlib.LabelPrefix+"zone"])

	// Handed over whole, ours and everybody else's. Picking out what a key means
	// is the consumer's, which is what lets one read answer ic-dns and operator.
	assert.Equal(t, "true", got[iutil.FeaturesNetworks])
}

// seeded runs one run so the state holds a fleet, and returns the fixture.
// The project owns its networks, or a run would only read the default
// project's and the fleet's own networks would be invisible.
func seeded(t *testing.T, instances, networks int) *fixture {
	t.Helper()

	h := newFixture(t)

	fleet := testlib.NewProject("p", instances, max(networks, 1))
	fleet.Project.Config[iutil.FeaturesNetworks] = "true"

	h.setFleet(fleet)

	// connected, then one event per instance, then the end of the run.
	h.collect(2 + instances)

	return h
}

// TestNetworkUpdatePatchesAndFansOut: a network is iutil, so a subnet moving
// changes every record on it. The event says so and the re-reads follow.
func TestNetworkUpdatePatchesAndFansOut(t *testing.T) {
	t.Parallel()

	h := seeded(t, 2, 1)

	before := h.readsOf("p", testlib.InstanceName(0))

	// Tagged, so what the re-reads find is worth an event of its own.
	h.retag("moved")
	h.send(incusapi.EventLifecycleNetworkUpdated, "p", testlib.NetworkName(0))

	// The change first, then what it caused.
	assert.Equal(t, incusapi.EventLifecycleNetworkUpdated, h.next().Action())

	fanned := []string{h.next().Name(), h.next().Name()}
	assert.ElementsMatch(t,
		[]string{testlib.InstanceName(0), testlib.InstanceName(1)}, fanned,
		"everything on the network is re-read")

	assert.Greater(t, h.readsOf("p", testlib.InstanceName(0)), before,
		"and re-read means read, not guessed")
}

// TestNetworkDeleteForgetsTheNetwork: the network goes, so nothing is left resolving
// against a key that describes nothing. Nothing is on it, because Incus
// refuses to delete a managed network anything is attached to.
func TestNetworkDeleteForgetsTheNetwork(t *testing.T) {
	t.Parallel()

	h := seeded(t, 0, 1)

	h.send(incusapi.EventLifecycleNetworkDeleted, "p", testlib.NetworkName(0))

	assert.Equal(t, incusapi.EventLifecycleNetworkDeleted, h.next().Action())

	// The state belongs to Run's goroutine, so it is only safe to look at once
	// Run has returned.
	h.stop()

	_, _, known := h.p.state.network("p", testlib.NetworkName(0))
	assert.False(t, known, "the network is gone")
}

// TestNetworkRenameDropsTheOldKey: a rename leaves the old key behind whatever
// else happens - the network is still there, but not under the name its addresses
// were filed under.
func TestNetworkRenameDropsTheOldKey(t *testing.T) {
	t.Parallel()

	h := seeded(t, 1, 1)

	// Renamed in the fleet too, or the instances this fans out over still name
	// the old network - and reading it would put it back, correctly, because
	// Incus would still have it under that name.
	h.mu.Lock()
	h.fleet.Networks[0].Name = "renamed"
	h.nic = "renamed"
	h.mu.Unlock()

	ev := iutil.NewEvent(time.Now(), incusapi.EventLifecycleNetworkRenamed,
		"p", "renamed", testlib.NetworkName(0))
	h.p.Handle(ev)

	assert.Equal(t, incusapi.EventLifecycleNetworkRenamed, h.next().Action())

	require.Eventually(t, func() bool { return h.netReadsOf("p", "renamed") > 0 },
		2*time.Second, time.Millisecond, "the new name is read")

	h.stop()

	_, _, known := h.p.state.network("p", testlib.NetworkName(0))
	assert.False(t, known, "and the old key does not survive it")
}

// TestProfileUpdateFansOut: a profile re-expands the configuration of every
// instance using it, and the event names none of them.
func TestProfileUpdateFansOut(t *testing.T) {
	t.Parallel()

	h := seeded(t, 3, 1)

	before := h.lists.Load()

	// Tagged, so what the re-reads find is worth an event of its own.
	h.retag("moved")
	h.send(incusapi.EventLifecycleProfileUpdated, "p", "default")

	assert.Equal(t, incusapi.EventLifecycleProfileUpdated, h.next().Action())

	fanned := []string{h.next().Name(), h.next().Name(), h.next().Name()}
	assert.ElementsMatch(t, []string{
		testlib.InstanceName(0), testlib.InstanceName(1), testlib.InstanceName(2),
	}, fanned, "every instance in the project, and the project was not read to find them")

	assert.Equal(t, before, h.lists.Load(), "no run was needed for that")
}

// TestProfileDeleteCostsNoRead: Incus refuses to delete a profile anything is
// expanded from, so the event names one no instance used. Fanning out over the
// project would be a read per instance to learn nothing.
func TestProfileDeleteCostsNoRead(t *testing.T) {
	t.Parallel()

	h := seeded(t, 2, 1)

	before := h.readsOf("p", testlib.InstanceName(0))

	h.retag("moved")
	h.send(incusapi.EventLifecycleProfileDeleted, "p", "gone")

	assert.Equal(t, incusapi.EventLifecycleProfileDeleted, h.next().Action())

	assert.Never(t, func() bool { return h.readsOf("p", testlib.InstanceName(0)) > before },
		time.Second, 20*time.Millisecond, "a profile nothing used re-read the project")
}

// TestProfileUpdateInAnEmptyProjectCostsNothing: nothing to re-expand, nothing
// to read.
func TestProfileUpdateInAnEmptyProjectCostsNothing(t *testing.T) {
	t.Parallel()

	h := seeded(t, 1, 1)

	h.send(incusapi.EventLifecycleProfileUpdated, "elsewhere", "default")

	assert.Equal(t, incusapi.EventLifecycleProfileUpdated, h.next().Action())
	assert.Zero(t, h.readsOf("elsewhere", testlib.InstanceName(0)))
}

// TestARunPrunesWhatTheListingLeftOut: a listing is the whole of one scope, so
// what it does not have has gone.
func TestARunPrunesWhatTheListingLeftOut(t *testing.T) {
	t.Parallel()

	p := bare(t)

	var gone []string

	p.args.Next = func(ev *iutil.Event) {
		if ev.Action() == incusapi.EventLifecycleInstanceDeleted {
			gone = append(gone, ev.Name())
		}
	}

	p.state.setNetwork(testlib.NewNetwork("p", 0))

	hold := func(name string) {
		inst := testlib.NewInstance("p", 0, 0)
		inst.Name = name

		held, _ := p.state.setInstance(&inst, testlib.NewInstanceState(0, 0))
		require.NotNil(t, held)
	}

	hold("keep")
	hold("gone")

	ctx := t.Context()

	p.acceptSweep(ctx, sweepMsg{
		action:  sweepActionInstances,
		project: "p",
		names:   []string{"keep"},
	})

	assert.NotNil(t, p.state.instance("p", "keep"), "a listed name was pruned")
	assert.Nil(t, p.state.instance("p", "gone"), "a name the listing no longer has stayed in the state")

	// Nothing else reaches a name Incus lost while this was down, so a prune
	// that dropped it silently would leave it answering for ever.
	assert.Equal(t, []string{"gone"}, gone, "the prune was not announced")
}

// TestARunPrunesAProjectItNoLongerServes: a project leaving the served set
// takes its instances with it, or a fan-out keeps re-reading names in a project
// this binary has been told to ignore.
func TestARunPrunesAProjectItNoLongerServes(t *testing.T) {
	t.Parallel()

	p := bare(t)

	p.state.setProject("shop", map[string]string{})
	p.state.setProject("blog", map[string]string{})
	p.state.setNetwork(testlib.NewNetwork("blog", 0))

	inst := testlib.NewInstance("blog", 0, 0)
	held, _ := p.state.setInstance(&inst, testlib.NewInstanceState(0, 0))
	require.NotNil(t, held)

	ctx := t.Context()

	p.acceptSweep(ctx, sweepMsg{action: sweepActionProjects, names: []string{"shop"}})

	assert.NotNil(t, p.state.projectConfig("shop"))
	assert.Nil(t, p.state.projectConfig("blog"))
	assert.Nil(t, p.state.instance("blog", testlib.InstanceName(0)),
		"the project went and its instances stayed behind")
}

// TestARunLeavesTheStateAloneWhenTheListingFailed: an empty answer and a
// daemon that would not answer arrive the same way, and one of them means every
// name in the project.
func TestARunLeavesTheStateAloneWhenTheListingFailed(t *testing.T) {
	t.Parallel()

	h := seeded(t, 1, 1)

	h.mu.Lock()
	h.listErr = errors.New("incusd said no")
	h.mu.Unlock()

	h.send(iutil.ActionConnected, "", "")
	require.Equal(t, iutil.ActionConnected, h.next().Action())

	assert.Never(t, func() bool { return len(h.out) > 0 },
		time.Second, 20*time.Millisecond, "a run that read nothing announced something")

	h.stop()

	assert.NotNil(t, h.p.state.instance("p", testlib.InstanceName(0)),
		"a listing that failed pruned the fleet it could not read")
}

// TestRunsAreNotBackToBack: every run after the first is paced, or a fleet
// small enough to read in no time would be read again immediately, for ever.
func TestRunsAreNotBackToBack(t *testing.T) {
	t.Parallel()

	h := seeded(t, 1, 1)

	before := h.lists.Load()

	assert.Never(t, func() bool { return h.lists.Load() > before },
		time.Second, 20*time.Millisecond, "a second run started with no delay at all")
}

// TestWantsWhatItActsOnItself: it asks for no enrichment of its own, since it
// is the plugin that performs everybody else's.
func TestWantsWhatItActsOnItself(t *testing.T) {
	t.Parallel()

	got := New().Wants()

	require.Len(t, got, 2)

	for _, want := range got {
		assert.Zero(t, want.Enrich, "the enricher asked to be enriched")
	}

	assert.Equal(t, incusapi.EventLifecycleProfileUpdated, got[0].Action)
	assert.True(t, got[0].Debounce, "every update in a burst costs a read per instance")

	assert.Equal(t, incusapi.EventLifecycleProfileDeleted, got[1].Action)
	assert.False(t, got[1].Debounce, "a delete collapsed into a burst is a delete lost")
}

// TestTheFleetIsWrittenOnTheWayDown: what the enricher held when it stopped is
// what the next start finds, so a shutdown owes the store one last clone -
// whatever the timer was doing.
func TestTheFleetIsWrittenOnTheWayDown(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "fleet.json")

	// An hour, so nothing is written on the timer and what lands is the clone
	// the way down offered.
	h := newFixture(t, StoreFile(path), StoreInterval(time.Hour))

	fleet := testlib.NewProject("p", 1, 1)
	fleet.Project.Config[iutil.FeaturesNetworks] = "true"
	fleet.Project.Config["user.label.zone"] = "internal"

	h.setFleet(fleet)

	// connected, the one instance the run trickled, the end of the run.
	h.collect(3)

	require.NoFileExists(t, path, "the fleet was written before anything asked for it")

	h.stop()

	b, err := os.ReadFile(path)
	require.NoError(t, err, "nothing was written on the way down")

	var got stateJSON

	require.NoError(t, json.Unmarshal(b, &got))

	require.Contains(t, got.Projects, "p")
	assert.Equal(t, "internal", got.Projects["p"].Config["user.label.zone"])

	held := got.Projects["p"].Instances[testlib.InstanceName(0)]
	require.NotNil(t, held, "the instance the run read did not reach the file")
	assert.True(t, held.Running())

	require.Contains(t, got.Projects["p"].Networks, testlib.NetworkName(0),
		"the network the run read did not reach the file")
}

// TestNothingIsWrittenWithoutAStoreFile: the cold store is off by default, and
// off means no goroutine and no clone rather than a write nobody reads.
func TestNothingIsWrittenWithoutAStoreFile(t *testing.T) {
	t.Parallel()

	h := seeded(t, 1, 1)

	assert.Nil(t, h.p.storeIn, "a store was started for a plugin told to write nowhere")

	h.p.storeClone()

	assert.True(t, h.p.state.dirty, "a clone nobody wanted cleared the state's mark")
}

// TestWhatAsksForAnInstanceRead: there are no interfaces without the instance
// they belong to, so a plugin naming only the second bit still asks for a read.
// One that asked for enrichment and silently got none would serve an empty
// fleet and never say why - which is exactly what it did.
func TestWhatAsksForAnInstanceRead(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		want iutil.Enrichment
		read bool
	}{
		{name: "the instance alone", want: iutil.EnrichedInstance, read: true},
		{name: "its interfaces alone", want: iutil.EnrichedInstanceWithInterfaces, read: true},
		{
			name: "both, which is what a plugin means either way",
			want: iutil.EnrichedInstance | iutil.EnrichedInstanceWithInterfaces,
			read: true,
		},
		{
			// An observer takes what walks past and asks for nothing.
			name: "neither",
			want: iutil.EnrichedProject,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			p := bare(t)
			p.args.Wanted = map[string]iutil.Want{
				incusapi.EventLifecycleInstanceUpdated: {
					Action: incusapi.EventLifecycleInstanceUpdated,
					Enrich: tc.want,
				},
			}

			p.accept(t.Context(), iutil.NewEvent(time.Now(),
				incusapi.EventLifecycleInstanceUpdated, "p", "one", ""))

			_, asked := p.reads.calls[resourceKey(kindInstance, "p", "one")]
			assert.Equal(t, tc.read, asked, "what was asked of the daemon")
		})
	}
}

// TestEveryWantOfDNSIsReadFor pins the table that cost an e2e run: what the dns
// plugin asks for has to be what the enricher acts on, and the two live apart.
func TestEveryWantOfDNSIsReadFor(t *testing.T) {
	t.Parallel()

	for _, want := range wanted {
		if want.Enrich&iutil.EnrichedInstanceWithInterfaces == 0 {
			continue
		}

		assert.NotZero(t, want.Enrich&wantsInstanceRead,
			"%s asks for interfaces without asking for a read", want.Action)
	}
}
