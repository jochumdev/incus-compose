package dns

import (
	"context"
	"fmt"
	"net/netip"
	"path/filepath"
	"testing"
	"time"

	incusapi "github.com/lxc/incus/v7/shared/api"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lxc/incus-compose/ievent/shared"
)

// plugged builds a plugin wired to a collecting successor and its two command
// doors, the way the source wires one.
func plugged(t *testing.T, opts ...Option) (*Plugin, chan *shared.Event, chan shared.Command, chan shared.Command) {
	t.Helper()

	p := New(opts...)

	seen := make(chan *shared.Event, 64)
	in := make(chan shared.Command)
	raised := make(chan shared.Command, 8)

	err := p.Setup(shared.SetupArgs{
		Context:    t.Context(),
		Next:       func(ev *shared.Event) { seen <- ev },
		CommandIn:  in,
		CommandOut: raised,
	})
	require.NoError(t, err)

	return p, seen, in, raised
}

// event is one bare event, carrying no read.
func event(action, project, name string) *shared.Event {
	return shared.NewEvent(time.Now(), action, project, name, "")
}

// enriched is one instance event as the enricher hands it over: read, running,
// and on one network with an address on it.
func enriched(action, project, name, addr string) *shared.Event {
	net := shared.NewNetwork("net0", project, true,
		[]netip.Prefix{netip.MustParsePrefix("10.0.0.0/24")},
		[]netip.Addr{netip.MustParseAddr(addr)}, nil)

	return event(action, project, name).
		WithInstance(true, map[string]string{}, map[string]*shared.Network{project + "/net0": net})
}

func TestFold(t *testing.T) {
	cases := []struct {
		name string
		feed []*shared.Event

		held      []string
		healthy   bool
		published bool
	}{
		{
			// The closing bracket is where everything held is known to be
			// everything there is, so it is the only thing that publishes.
			name:      "a pass publishes and turns healthy",
			feed:      []*shared.Event{event(shared.ActionSweepEnd, "", "")},
			healthy:   true,
			published: true,
		},
		{
			// A lost stream drops no record - what is held is still the best
			// answer there is - but nothing is confirming it any more.
			name: "a lost stream keeps the records and stops being healthy",
			feed: []*shared.Event{
				enriched(incusapi.EventLifecycleInstanceStarted, "shop", "web", "10.0.0.2"),
				event(shared.ActionSweepEnd, "", ""),
				event(shared.ActionDisconnected, "", ""),
			},
			held:      []string{"shop/web"},
			healthy:   false,
			published: true,
		},
		{
			name: "a delete drops what it names",
			feed: []*shared.Event{
				enriched(incusapi.EventLifecycleInstanceStarted, "shop", "web", "10.0.0.2"),
				enriched(incusapi.EventLifecycleInstanceStarted, "shop", "db", "10.0.0.3"),
				event(incusapi.EventLifecycleInstanceDeleted, "shop", "web"),
			},
			held: []string{"shop/db"},
		},
		{
			// Two projects may each have a web, and they are different hosts in
			// different zones. Held by name alone, one would overwrite the other.
			name: "one name in two projects is two instances",
			feed: []*shared.Event{
				enriched(incusapi.EventLifecycleInstanceStarted, "shop", "web", "10.0.0.2"),
				enriched(incusapi.EventLifecycleInstanceStarted, "blog", "web", "10.0.1.2"),
			},
			held: []string{"shop/web", "blog/web"},
		},
		{
			// A record pointing at an address nothing is listening on has the
			// client wait out a timeout instead of being told at once.
			name: "a stopped instance is dropped",
			feed: []*shared.Event{
				enriched(incusapi.EventLifecycleInstanceStarted, "shop", "web", "10.0.0.2"),
				event(incusapi.EventLifecycleInstanceStopped, "shop", "web"),
			},
		},
		{
			// The actions a plugin raises carry no name, and would fold into an
			// entry called "" that reaches the cold store.
			name: "an action with no name is not an instance",
			feed: []*shared.Event{
				event(shared.ActionConnected, "", ""),
				event(shared.ActionSweepStart, "", ""),
				event(shared.ActionReady, "", ""),
			},
		},
		{
			// An event somebody already finished with is walking for the
			// observers. Acting on it would fold a drop into the fleet.
			name: "an event already dropped changes nothing",
			feed: []*shared.Event{
				enriched(incusapi.EventLifecycleInstanceStarted, "shop", "web", "10.0.0.2").WithDropped("debounce"),
				enriched(incusapi.EventLifecycleInstanceStarted, "shop", "db", "10.0.0.3").WithFailed("source/read"),
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, seen, _, _ := plugged(t)

			for _, ev := range tc.feed {
				p.fold(ev)
			}

			held := make([]string, 0, len(p.held))
			for name := range p.held {
				held = append(held, name)
			}

			assert.ElementsMatch(t, tc.held, held)
			assert.Equal(t, tc.published, p.view.Ready() || !tc.healthy && tc.published)

			// Every event is handed on whatever was done with it, which is what
			// the observers behind here depend on.
			assert.Len(t, seen, len(tc.feed), "an event was not handed on")
		})
	}
}

// TestReadinessIsEdges pins that a change is announced once. A level re-raised
// on every event would put one command on the chain per event.
func TestReadinessIsEdges(t *testing.T) {
	p, _, _, raised := plugged(t)

	// Nothing published, so nothing to say.
	p.fold(enriched(incusapi.EventLifecycleInstanceStarted, "shop", "web", "10.0.0.2"))
	assert.Empty(t, raised, "readiness was announced before anything was published")

	p.fold(event(shared.ActionSweepEnd, "", ""))
	require.Len(t, raised, 1)
	assert.Equal(t, shared.ActionReady, (<-raised).Action)

	// Still ready, and said once.
	p.fold(event(shared.ActionSweepEnd, "", ""))
	p.fold(enriched(incusapi.EventLifecycleInstanceStarted, "shop", "db", "10.0.0.3"))
	assert.Empty(t, raised, "ready was announced twice")

	// The other edge.
	p.fold(event(shared.ActionDisconnected, "", ""))
	require.Len(t, raised, 1)
	assert.Equal(t, shared.ActionNotReady, (<-raised).Action)
}

// TestHandleDropsRatherThanBlocks pins the inbox door: a full inbox is a drop
// that keeps walking rather than a wait that stops the chain.
func TestHandleDropsRatherThanBlocks(t *testing.T) {
	p, seen, _, _ := plugged(t)

	// One slot, so the second has nowhere to go and nothing is reading.
	p.inbox = make(chan *shared.Event, 1)

	p.Handle(event(incusapi.EventLifecycleInstanceStarted, "shop", "web"))
	assert.Empty(t, seen, "the first one was handed on rather than queued")

	p.Handle(event(incusapi.EventLifecycleInstanceStarted, "shop", "db"))

	require.Len(t, seen, 1)

	got := <-seen
	assert.Equal(t, shared.StateDropped, got.State())
	assert.Equal(t, name, got.Reason(), "the drop does not name who did it")
}

// TestRunDrainsWhatItHolds pins the shutdown contract: everything taken is
// handed on before the answer goes back, since the source then asks the next.
func TestRunDrainsWhatItHolds(t *testing.T) {
	p, seen, in, raised := plugged(t)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	done := make(chan error, 1)

	go func() { done <- p.Run(ctx) }()

	for i, n := range []string{"web", "db", "cache"} {
		p.Handle(enriched(incusapi.EventLifecycleInstanceStarted, "shop", n,
			fmt.Sprintf("10.0.0.%d", 2+i)))
	}

	in <- shared.Command{Action: shared.CommandDrain}

	// The answer, ignoring any readiness raised on the way.
	for {
		cmd := <-raised
		if cmd.Action == shared.CommandDrain {
			break
		}
	}

	require.NoError(t, <-done)

	// Everything, and the answer came after it: the channel already holds all
	// three by the time the drain was answered.
	assert.Len(t, seen, 3)
}

// TestRunRestoresTheColdStore pins the whole point of the file: a restart
// answers from what the last run served, and carries its serials.
func TestRunRestoresTheColdStore(t *testing.T) {
	dir := t.TempDir()

	// What a previous run left behind.
	b, err := encodeCold(
		map[string]*instance{
			"shop/web": oneInstance("shop.incus.", "shop/net0", "10.0.0.2"),
			"shop/db":  oneInstance("shop.incus.", "shop/net0", "10.0.0.3"),
		},
		snapshotWithSerials(map[string]uint32{"shop.incus.": 9}),
	)
	require.NoError(t, err)

	newColdStore(dir).write(b)

	p, _, in, raised := plugged(t, ColdDir(dir))

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	done := make(chan error, 1)

	go func() { done <- p.Run(ctx) }()

	in <- shared.Command{Action: shared.CommandDrain}

	for {
		cmd := <-raised
		if cmd.Action == shared.CommandDrain {
			break
		}
	}

	require.NoError(t, <-done)

	require.Len(t, p.held, 2, "a restart did not answer from what the last run served")
	assert.Equal(t, "shop.incus.", p.held["shop/web"].zone)
	assert.Equal(t, map[string]uint32{"shop.incus.": 9}, p.serials,
		"the serials did not survive, so every secondary re-transfers")

	// And it wrote on the way out, so the next restart has one too.
	assert.FileExists(t, filepath.Join(dir, coldFile))
}

// TestFoldKeepsProjectLabelsAnEventDidNotCarry pins the enricher's contract at
// the consumer: it fills a project's labels only on the actions somebody asked
// for them on, so an event arriving without them has not learned that the
// project sets none.
//
// A rename is where it bites. Wants asks for the instance and its networks
// alone, so reading the labels off it anyway re-files the instance under the
// default zone and closes the transfer gate its project opened.
func TestFoldKeepsProjectLabelsAnEventDidNotCarry(t *testing.T) {
	own := map[string]string{
		labelPrefix + metaZone:     "shop.example.",
		labelPrefix + metaTransfer: "true",
	}

	// A rename as the enricher hands one over: instance and networks read, the
	// project not, and the old name alongside the new one.
	renamed := func() *shared.Event {
		net := shared.NewNetwork("net0", "shop", true,
			[]netip.Prefix{netip.MustParsePrefix("10.0.0.0/24")},
			[]netip.Addr{netip.MustParseAddr("10.0.0.2")}, nil)

		return shared.NewEvent(time.Now(), incusapi.EventLifecycleInstanceRenamed, "shop", "www", "web").
			WithInstance(true, map[string]string{}, map[string]*shared.Network{"shop/net0": net})
	}

	t.Run("a rename keeps what the project last said", func(t *testing.T) {
		p, _, _, _ := plugged(t, Suffix("incus"))

		p.fold(enriched(incusapi.EventLifecycleInstanceStarted, "shop", "web", "10.0.0.2").
			WithProject(own))

		was := p.held["shop/web"]
		require.NotNil(t, was)
		require.Equal(t, "shop.example.", was.zone)
		require.True(t, was.transfer)

		p.fold(renamed())

		got := p.held["shop/www"]
		require.NotNil(t, got)

		assert.Equal(t, "shop.example.", got.zone, "the project's zone survived a rename that never read it")
		assert.True(t, got.transfer, "and so did its transfer opt-in")
		assert.NotContains(t, p.held, "shop/web", "the old name is gone either way")
	})

	t.Run("a project read as setting nothing does clear it", func(t *testing.T) {
		p, _, _, _ := plugged(t, Suffix("incus"))

		p.fold(enriched(incusapi.EventLifecycleInstanceStarted, "shop", "web", "10.0.0.2").
			WithProject(own))
		require.True(t, p.held["shop/web"].transfer)

		// Enriched, and empty: the project really was read and really unset it.
		p.fold(enriched(incusapi.EventLifecycleInstanceUpdated, "shop", "web", "10.0.0.2").
			WithProject(map[string]string{}))

		got := p.held["shop/web"]
		require.NotNil(t, got)

		assert.Equal(t, "shop.incus.", got.zone, "unsetting the label falls back to the suffix")
		assert.False(t, got.transfer, "and closes the gate again")
	})
}

// TestDistillIgnoresAnInstanceClaimingTransfer pins the one label a project
// keeps to itself. A zone belongs to its project, so an instance opting one in
// would expose every sibling sharing it.
func TestDistillIgnoresAnInstanceClaimingTransfer(t *testing.T) {
	inst := distill(labeled("shop", "web",
		map[string]string{labelPrefix + metaTransfer: "true"},
		nil), nil, "incus")
	require.NotNil(t, inst)

	assert.False(t, inst.transfer, "an instance cannot open its project's zone")
	assert.NotContains(t, inst.meta, metaTransfer, "and the key never reaches meta to be read later")
}
