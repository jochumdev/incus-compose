package dns

import (
	"context"
	"log/slog"
	"testing"
	"time"

	incusapi "github.com/lxc/incus/v7/shared/api"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lxc/incus-compose/ievent/iutil"
)

// resolves is one query and what the fleet the events describe should answer
// it with. Nil want is a name nothing answers to.
type resolves struct {
	qname string
	from  string
	want  []string
}

// fed drives the plugin the way the chain does - events in, then the window -
// and hands back what it holds.
func fed(t *testing.T, feed ...*iutil.Event) *Plugin {
	t.Helper()

	p := New(slog.Default(), Suffix("example"))
	p.next = func(_ *iutil.Event) {}

	for _, ev := range feed {
		p.fold(ev)
	}

	// Run owns the timer, so the window is fired by hand here.
	p.write()

	return p
}

func TestPluginServesWhatItWasFed(t *testing.T) {
	const started = incusapi.EventLifecycleInstanceStarted

	tests := []struct {
		name string
		feed []*iutil.Event
		want []resolves
	}{
		{
			name: "one instance answers to its own name",
			feed: []*iutil.Event{
				eventOn(started, "shop", "web", nil, map[string]string{"net-a": "10.0.0.5"}),
			},
			want: []resolves{
				{qname: "web.shop.example.", from: "10.0.0.5", want: []string{"10.0.0.5"}},
				{qname: "db.shop.example.", from: "10.0.0.5"},
			},
		},
		{
			name: "a service name answers with every replica",
			feed: []*iutil.Event{
				eventOn(started, "shop", "web", map[string]string{metaService: "frontend"}, map[string]string{"net-a": "10.0.0.5"}),
				eventOn(started, "shop", "api", map[string]string{metaService: "frontend"}, map[string]string{"net-a": "10.0.0.6"}),
			},
			want: []resolves{
				{qname: "frontend.shop.example.", from: "10.0.0.5", want: []string{"10.0.0.5", "10.0.0.6"}},
				{qname: "web.shop.example.", from: "10.0.0.5", want: []string{"10.0.0.5"}},
			},
		},
		{
			name: "the last read of an instance is the one that counts",
			feed: []*iutil.Event{
				eventOn(started, "shop", "web", nil, map[string]string{"net-a": "10.0.0.5"}),
				eventOn(incusapi.EventLifecycleInstanceUpdated, "shop", "web", nil, map[string]string{"net-a": "10.0.0.9"}),
			},
			want: []resolves{
				{qname: "web.shop.example.", from: "10.0.0.9", want: []string{"10.0.0.9"}},
			},
		},
		{
			name: "a delete takes the name with it",
			feed: []*iutil.Event{
				eventOn(started, "shop", "web", nil, map[string]string{"net-a": "10.0.0.5"}),
				eventOn(started, "shop", "db", nil, map[string]string{"net-a": "10.0.0.6"}),
				event(incusapi.EventLifecycleInstanceDeleted, "shop", "web"),
			},
			want: []resolves{
				{qname: "web.shop.example.", from: "10.0.0.6"},
				{qname: "db.shop.example.", from: "10.0.0.6", want: []string{"10.0.0.6"}},
			},
		},
		{
			name: "an instance that stopped and lost its addresses stops answering",
			feed: []*iutil.Event{
				eventOn(started, "shop", "web", nil, map[string]string{"net-a": "10.0.0.5"}),
				eventOn(started, "shop", "db", nil, map[string]string{"net-a": "10.0.0.6"}),
				event(incusapi.EventLifecycleInstanceStopped, "shop", "web").
					WithInstance(read("shop", false, nil, nil), true),
			},
			want: []resolves{
				{qname: "web.shop.example.", from: "10.0.0.6"},
				{qname: "db.shop.example.", from: "10.0.0.6", want: []string{"10.0.0.6"}},
			},
		},
		{
			name: "a rename answers to the new name and not the old",
			feed: []*iutil.Event{
				eventOn(started, "shop", "web", nil, map[string]string{"net-a": "10.0.0.5"}),
				eventOn(started, "shop", "db", nil, map[string]string{"net-a": "10.0.0.6"}),
				iutil.NewEvent(time.Now(), incusapi.EventLifecycleInstanceRenamed, "shop", "www", "web").
					WithChainState(iutil.ChainCold).
					WithInstance(read("shop", true, nil, netsOf(map[string]string{"net-a": "10.0.0.5"})), true),
			},
			want: []resolves{
				{qname: "www.shop.example.", from: "10.0.0.5", want: []string{"10.0.0.5"}},
				{qname: "web.shop.example.", from: "10.0.0.5"},
			},
		},
		{
			name: "two projects on one wire see each other",
			feed: []*iutil.Event{
				eventOn(started, "shop", "web", nil, map[string]string{"net-a": "10.0.0.5"}),
				eventOn(started, "blog", "api", nil, map[string]string{"net-a": "10.0.0.6"}),
			},
			want: []resolves{
				{qname: "api.blog.example.", from: "10.0.0.5", want: []string{"10.0.0.6"}},
				{qname: "web.shop.example.", from: "10.0.0.6", want: []string{"10.0.0.5"}},
			},
		},
		{
			name: "wires that do not reach each other answer for nobody else",
			feed: []*iutil.Event{
				eventOn(started, "shop", "web", nil, map[string]string{"net-a": "10.0.0.5"}),
				eventOn(started, "blog", "api", nil, map[string]string{"net-b": "10.1.0.6"}),
			},
			want: []resolves{
				{qname: "web.shop.example.", from: "10.0.0.5", want: []string{"10.0.0.5"}},
				{qname: "api.blog.example.", from: "10.0.0.5"},
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			snap := fed(t, test.feed...).state.snapshot()

			for _, want := range test.want {
				assert.Equal(t, want.want, answered(t, snap, want.qname, want.from),
					"%s asked from %s", want.qname, want.from)
			}
		})
	}
}

// running is the plugin started the way the source starts it, with the doors a
// test drives it through.
type running struct {
	p      *Plugin
	seen   chan *iutil.Event
	in     chan iutil.Command
	raised chan iutil.Command
	done   chan error
}

// start wires and runs the plugin. Run owns everything below the inbox, so a
// test reads that only after stop returns.
func start(t *testing.T, opts ...Option) *running {
	t.Helper()

	r := &running{
		p:      New(slog.Default(), opts...),
		seen:   make(chan *iutil.Event, 64),
		in:     make(chan iutil.Command),
		raised: make(chan iutil.Command, 16),
		done:   make(chan error, 1),
	}

	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)

	require.NoError(t, r.p.Setup(iutil.SetupArgs{
		Next:       func(ev *iutil.Event) { r.seen <- ev },
		CommandIn:  r.in,
		CommandOut: r.raised,
	}))

	go func() { r.done <- r.p.Run(ctx) }()

	return r
}

// feed hands events in and waits for the last to come out the other side, which
// is what makes the fold before it finished.
func (r *running) feed(t *testing.T, feed ...*iutil.Event) {
	t.Helper()

	for _, ev := range feed {
		r.p.Handle(ev)
	}

	last := feed[len(feed)-1]

	for {
		select {
		case got := <-r.seen:
			if got.Action() == last.Action() {
				return
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("%s never reached the far side", last.Action())
		}
	}
}

// stop drains the plugin the way the source does, so what it holds can be read.
func (r *running) stop(t *testing.T) {
	t.Helper()

	r.in <- iutil.Command{Action: iutil.CommandDrain}

	for cmd := range r.raised {
		if cmd.Action == iutil.CommandDrain {
			break
		}
	}

	require.NoError(t, <-r.done)
}

// TestReconnectClaimsNothingAboutAFleetThatDidNotMove walks the chain state a
// dropped event stream puts it through - cold, warm, cold again, warm again -
// since a reconnect re-reads the whole fleet and a serial step per instance
// would have every secondary transfer it back.
func TestReconnectClaimsNothingAboutAFleetThatDidNotMove(t *testing.T) {
	const zone = "shop.example."

	seed, err := encodeCold(map[string]zoneSerial{zone: {Serial: 7}})
	require.NoError(t, err)

	r := start(t, Suffix("example"), ColdMemory(seed))

	updated := func(addr string) *iutil.Event {
		return eventOn(incusapi.EventLifecycleInstanceUpdated, "shop", "web", nil,
			map[string]string{"net-a": addr})
	}

	// The replay off disk, then the round that reads Incus.
	r.feed(t, updated("10.0.0.5"), event(iutil.ActionSweepEnd, "", ""))
	assert.True(t, r.p.view.Ready(), "a round that finished serves")

	// A live change while warm.
	r.feed(t, warm(updated("10.0.0.9")), warm(event(iutil.ActionSweepEnd, "", "")))

	// The stream drops. Nothing is confirming records any more, but none go.
	r.feed(t, event(iutil.ActionDisconnected, "", ""))
	assert.False(t, r.p.view.Ready(), "nothing is confirming what is served")

	// Reconnected: the enricher replays cold, and the fleet has not moved.
	r.feed(t, updated("10.0.0.9"), event(iutil.ActionSweepEnd, "", ""))
	assert.True(t, r.p.view.Ready(), "a round finished again")

	// And warm again, still with nothing having changed.
	r.feed(t, warm(updated("10.0.0.9")), warm(event(iutil.ActionSweepEnd, "", "")))

	r.stop(t)

	assert.Equal(t, uint32(8), r.p.state.serials[zone].Serial,
		"one change since the seed, and a reconnect is not one")

	assert.Equal(t, []string{"10.0.0.9"},
		answered(t, r.p.state.snapshot(), "web."+zone, "10.0.0.9"),
		"a lost stream drops no record")
}

// TestColdBecomesWarm drives a restart end to end: a previous run's serials
// loaded, the enricher's replay folded cold, then a round that has actually
// read Incus. Cold serves what it was handed and claims nothing about it; warm
// is what makes a change a change.
func TestColdBecomesWarm(t *testing.T) {
	const zone = "shop.example."

	// What a previous run published.
	seed, err := encodeCold(map[string]zoneSerial{zone: {Serial: 7}})
	require.NoError(t, err)

	r := start(t, Suffix("example"), ColdMemory(seed))

	// The replay: what the enricher held, stamped cold because nothing has been
	// read. Then the round that reads Incus and finds the fleet has moved.
	r.feed(t,
		eventOn(incusapi.EventLifecycleInstanceStarted, "shop", "web", nil,
			map[string]string{"net-a": "10.0.0.5"}),
		event(iutil.ActionSweepEnd, "", ""))

	r.feed(t,
		warm(eventOn(incusapi.EventLifecycleInstanceUpdated, "shop", "web", nil,
			map[string]string{"net-a": "10.0.0.9"})),
		warm(event(iutil.ActionSweepEnd, "", "")))

	r.stop(t)

	assert.Equal(t, uint32(8), r.p.state.serials[zone].Serial,
		"the replay claimed nothing and the warm change claimed once")

	assert.Equal(t, []string{"10.0.0.9"},
		answered(t, r.p.state.snapshot(), "web."+zone, "10.0.0.9"))

	// And what it published went back to the store it was seeded from.
	back, err := decodeCold(r.p.cold.held)
	require.NoError(t, err)
	assert.Equal(t, uint32(8), back[zone].Serial)
}

// TestPluginStoresTheSerialsItPublished pins what survives a restart: the
// serials, and nothing else - the records are the enricher's to replay.
func TestPluginStoresTheSerialsItPublished(t *testing.T) {
	p := fed(t,
		eventOn(incusapi.EventLifecycleInstanceStarted, "shop", "web", nil, map[string]string{"net-a": "10.0.0.5"}))

	held := p.state.serials

	require.Contains(t, held, "shop.example.")
	assert.Equal(t, uint32(1), held["shop.example."].Serial, "a zone's first publish is a birth")

	// The reverse zone its address made is published in its own right.
	assert.Contains(t, held, "0.0.10.in-addr.arpa.")

	encoded, err := encodeCold(held)
	require.NoError(t, err)

	back, err := decodeCold(encoded)
	require.NoError(t, err)

	assert.Equal(t, uint32(1), back["shop.example."].Serial)
	assert.Zero(t, back["shop.example."].names, "what a zone is made of is folded again, not stored")
}
