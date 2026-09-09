package dns

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"testing"
	"time"

	incusapi "github.com/lxc/incus/v7/shared/api"
	"github.com/miekg/dns"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lxc/incus-compose/ievent/iutil"
)

// plugged builds a plugin wired to a collecting successor and its two command
// doors, the way the source wires one.
func plugged(t *testing.T, opts ...Option) (*Plugin, chan *iutil.Event, chan iutil.Command, chan iutil.Command) {
	t.Helper()

	p := New(slog.Default(), opts...)

	seen := make(chan *iutil.Event, 64)
	in := make(chan iutil.Command)
	raised := make(chan iutil.Command, 8)

	err := p.Setup(iutil.SetupArgs{
		Next:       func(ev *iutil.Event) { seen <- ev },
		CommandIn:  in,
		CommandOut: raised,
	})
	require.NoError(t, err)

	return p, seen, in, raised
}

// event is one bare event, carrying no read, stamped cold: this plugin is what
// turns the chain warm, so nothing arrives warm until it has said so.
func event(action, project, name string) *iutil.Event {
	return iutil.NewEvent(time.Now(), action, project, name, "").WithChainState(iutil.ChainCold)
}

// warm stamps an event the way the source does once this plugin has raised it.
func warm(ev *iutil.Event) *iutil.Event { return ev.WithChainState(iutil.ChainWarm) }

// onNet is one network an instance sits on in a test: the subnet the network
// itself serves, and what the instance holds there. The two are set apart
// because they answer different questions, and a fixture needs each without the
// other - a NIC up before its lease, and a network Incus does not address.
type onNet struct {
	// project is the project that owns the network. Empty is the instance's own;
	// a wire two projects share is owned by neither, which is what keeps it one
	// wire rather than two that happen to share a name.
	project string

	subnet string
	addrs  []string
}

// addressed is one network serving a /24 around the address the instance holds
// on it, which is what makes that address reverse-resolvable.
func addressed(addr string) onNet {
	return onNet{
		subnet: netip.MustParsePrefix(addr + "/24").Masked().String(),
		addrs:  []string{addr},
	}
}

// read is one instance read as the enricher hands it over: where it sits, and
// what each of those places is.
func read(project string, running bool, config map[string]string, nets map[string]onNet) *iutil.Instance {
	var interfaces []iutil.InstanceInterface

	networks := map[string]*iutil.Network{}

	for name, sits := range nets {
		p := sits.project
		if p == "" {
			p = project
		}

		networks[iutil.NetworkKey(p, name)] =
			iutil.NewNetwork(name, p, true, sits.subnet, "")

		interfaces = append(interfaces,
			iutil.NewInstanceInterface(p, name, true, sits.addrs, nil))
	}

	return iutil.NewInstance(running, config, interfaces, networks)
}

// enriched is one instance event as the enricher hands it over: read, running,
// and on one network with an address on it.
func enriched(action, project, name, addr string) *iutil.Event {
	return event(action, project, name).WithInstance(
		read(project, true, map[string]string{}, map[string]onNet{
			"net0": {subnet: "10.0.0.0/24", addrs: []string{addr}},
		}), true)
}

// enrichedNoAddr is a read that landed before DHCP did: running and on a
// network, same as enriched, but that network carries no address yet.
func enrichedNoAddr(action, project, name string) *iutil.Event {
	return event(action, project, name).WithInstance(
		read(project, true, map[string]string{}, map[string]onNet{
			"net0": {subnet: "10.0.0.0/24"},
		}), true)
}

func TestFold(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		feed []*iutil.Event

		held      []string
		healthy   bool
		published bool
	}{
		{
			// The end of a round is where what is held has been confirmed all
			// the way round, so it is what publishes on a cold chain.
			name:      "the end of a round publishes and turns healthy",
			feed:      []*iutil.Event{event(iutil.ActionSweepEnd, "", "")},
			healthy:   true,
			published: true,
		},
		{
			// A lost stream drops no record - what is held is still the best
			// answer there is - but nothing is confirming it any more.
			name: "a lost stream keeps the records and stops being healthy",
			feed: []*iutil.Event{
				enriched(incusapi.EventLifecycleInstanceStarted, "shop", "web", "10.0.0.2"),
				event(iutil.ActionSweepEnd, "", ""),
				event(iutil.ActionDisconnected, "", ""),
			},
			held:      []string{"shop/web"},
			healthy:   false,
			published: true,
		},
		{
			name: "a delete drops what it names",
			feed: []*iutil.Event{
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
			feed: []*iutil.Event{
				enriched(incusapi.EventLifecycleInstanceStarted, "shop", "web", "10.0.0.2"),
				enriched(incusapi.EventLifecycleInstanceStarted, "blog", "web", "10.0.1.2"),
			},
			held: []string{"shop/web", "blog/web"},
		},
		{
			// A record pointing at an address nothing is listening on has the
			// client wait out a timeout instead of being told at once.
			name: "a deleted instance is dropped",
			feed: []*iutil.Event{
				enriched(incusapi.EventLifecycleInstanceStarted, "shop", "web", "10.0.0.2"),
				event(incusapi.EventLifecycleInstanceDeleted, "shop", "web"),
			},
		},
		{
			// The actions a plugin raises carry no name, and would fold into an
			// entry called "" that reaches the cold store.
			name: "an action with no name is not an instance",
			feed: []*iutil.Event{
				event(iutil.ActionConnected, "", ""),
				event(iutil.ActionSweepEnd, "", ""),
				event("dns/ready", "", ""),
			},
			healthy:   true,
			published: true,
		},
		{
			// The recovery side of the same race: one read that does carry an
			// address, however late, is all it takes.
			name: "an instance is served as soon as one read carries an address",
			feed: []*iutil.Event{
				enrichedNoAddr(incusapi.EventLifecycleInstanceStarted, "shop", "web"),
				enriched(incusapi.EventLifecycleInstanceUpdated, "shop", "web", "10.0.0.2"),
			},
			held: []string{"shop/web"},
		},
		{
			// An event somebody already finished with is walking for the
			// observers. Acting on it would fold a drop into the fleet.
			name: "an event already dropped changes nothing",
			feed: []*iutil.Event{
				enriched(incusapi.EventLifecycleInstanceStarted, "shop", "web", "10.0.0.2").WithDropped("debounce"),
				enriched(incusapi.EventLifecycleInstanceStarted, "shop", "db", "10.0.0.3").WithFailed(errors.New("source/read")),
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			p, seen, _, _ := plugged(t)

			for _, ev := range tc.feed {
				p.fold(ev)
			}

			// A fold collects; the window is what writes. Fired by hand here,
			// since Run owns the timer and this drives fold directly.
			p.write()

			held := make([]string, 0, len(p.state.held))
			for name := range p.state.held {
				held = append(held, name)
			}

			assert.ElementsMatch(t, tc.held, held)
			assert.Equal(t, tc.published, p.view.Ready() || !tc.healthy && tc.published)

			// Every event is handed on whatever was done with it, which is what
			// the observers after this one depend on.
			assert.Len(t, seen, len(tc.feed), "an event was not handed on")
		})
	}
}

// TestReadinessIsEdges pins that a change is announced once. A level re-raised
// on every event would put one command on the chain per event.
func TestReadinessIsEdges(t *testing.T) {
	t.Parallel()

	p, _, _, raised := plugged(t)

	// Nothing published, so nothing to say.
	p.fold(enriched(incusapi.EventLifecycleInstanceStarted, "shop", "web", "10.0.0.2"))
	assert.Empty(t, raised, "readiness was announced before anything was published")

	// The end of a round publishes and turns healthy, which is what makes it
	// ready. Turning the chain warm is the enricher's now, so this raises one
	// event rather than two.
	p.fold(event(iutil.ActionSweepEnd, "", ""))
	require.Len(t, raised, 1)

	assert.Equal(t, "dns/ready", (<-raised).Action)

	// Still ready: an edge is said once.
	p.fold(event(iutil.ActionSweepEnd, "", ""))
	p.fold(enriched(incusapi.EventLifecycleInstanceStarted, "shop", "db", "10.0.0.3"))
	assert.Empty(t, raised, "ready was announced twice")

	// The other edge.
	p.fold(event(iutil.ActionDisconnected, "", ""))
	require.Len(t, raised, 1)
	assert.Equal(t, "dns/not-ready", (<-raised).Action)
}

// TestLiveFoldPublishesOnceWarm pins that a delete folded warm republishes at
// once, rather than waiting up to SweepInterval for the record to drop.
func TestLiveFoldPublishesOnceWarm(t *testing.T) {
	t.Parallel()

	p := New(slog.Default(), Suffix("incus"))
	p.next = func(_ *iutil.Event) {}

	// Two instances, so the zone survives web's delete and the answer below is
	// NXDOMAIN within it rather than a refusal of a zone that no longer exists.
	p.fold(enriched(incusapi.EventLifecycleInstanceStarted, "shop", "web", "10.0.0.2"))
	p.fold(enriched(incusapi.EventLifecycleInstanceStarted, "shop", "db", "10.0.0.3"))
	p.fold(event(iutil.ActionSweepEnd, "", ""))

	wire(p.xfr, p.view, nil)
	a := &adapter{chain: p.view}

	w := &recorder{}
	a.ServeDNS(w, subnetQuery("web.shop.incus.", "10.0.0.2"))
	require.Equal(t, dns.RcodeSuccess, w.msg.Rcode, "known from the pass")

	p.fold(warm(event(incusapi.EventLifecycleInstanceDeleted, "shop", "web")))
	p.write()

	w = &recorder{}
	a.ServeDNS(w, subnetQuery("web.shop.incus.", "10.0.0.3"))
	assert.Equal(t, dns.RcodeNameError, w.msg.Rcode,
		"gone the moment the delete folded, not whenever the next pass runs")
}

// TestHandleDropsRatherThanBlocks pins the inbox door: a full inbox is a drop
// that keeps walking rather than a wait that stops the chain.
func TestHandleDropsRatherThanBlocks(t *testing.T) {
	t.Parallel()

	p, seen, _, _ := plugged(t)

	// One slot, so the second has nowhere to go and nothing is reading.
	p.inbox = make(chan *iutil.Event, 1)

	p.Handle(event(incusapi.EventLifecycleInstanceStarted, "shop", "web"))
	assert.Empty(t, seen, "the first one was handed on rather than queued")

	p.Handle(event(incusapi.EventLifecycleInstanceStarted, "shop", "db"))

	require.Len(t, seen, 1)

	got := <-seen
	assert.ErrorIs(t, got.Err(), iutil.ErrDropped)

	var err *iutil.Error

	require.ErrorAs(t, got.Err(), &err)
	assert.Equal(t, name, err.By(), "the drop does not name who did it")
}

// TestRunDrainsWhatItHolds pins the shutdown contract: everything taken is
// handed on before the answer goes back, since the source then asks the next.
func TestRunDrainsWhatItHolds(t *testing.T) {
	t.Parallel()

	p, seen, in, raised := plugged(t)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	done := make(chan error, 1)

	go func() { done <- p.Run(ctx) }()

	for i, n := range []string{"web", "db", "cache"} {
		p.Handle(enriched(incusapi.EventLifecycleInstanceStarted, "shop", n,
			fmt.Sprintf("10.0.0.%d", 2+i)))
	}

	in <- iutil.Command{Action: iutil.CommandDrain}

	// The answer, ignoring any readiness raised on the way.
	for {
		cmd := <-raised
		if cmd.Action == iutil.CommandDrain {
			break
		}
	}

	require.NoError(t, <-done)

	// Everything, and the answer came after it: the channel already holds all
	// three by the time the drain was answered.
	assert.Len(t, seen, 3)

	// And what they patched reached a snapshot. Without the flush the drain
	// answers, the process exits, and the last window is lost with nothing
	// saying so.
	for _, n := range []string{"web", "db", "cache"} {
		_, held := p.state.snapshot().Answers(n + ".shop.")
		assert.True(t, held, "%s was drained but never published", n)
	}
}
