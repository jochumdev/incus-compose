package source

import (
	"context"
	"errors"
	"testing"
	"time"

	incusapi "github.com/lxc/incus/v7/shared/api"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lxc/incus-compose/ievent/log"
	"github.com/lxc/incus-compose/ievent/shared"
)

// sawBuffer is how many events a recorder holds. Generous, because a test that
// overran it would deadlock the source rather than fail.
const sawBuffer = 128

// recorder is a plugin that keeps what walked past it.
//
// Everything it holds is written on whichever goroutine called Handle - the
// source's, while Run has it - and read by the test only afterwards, or one
// event at a time off saw. That is the same single-owner rule the real plugins
// follow, and it is what lets this hold no lock.
type recorder struct {
	name     string
	wants    []shared.Want
	setupErr error

	next shared.Next

	// args is what Setup was handed, kept so a test can assert on the table
	// every plugin was given and raise commands the way a plugin does.
	args shared.SetupArgs

	// walked is shared between the recorders of one test, so the order between
	// them is what the slice says.
	walked *[]string

	// saw is every event that walked past, in order. A test takes one off it to
	// wait for the source to have got that far.
	saw chan *shared.Event
}

func newRecorder(name string, walked *[]string, wants ...shared.Want) *recorder {
	return &recorder{
		name:   name,
		wants:  wants,
		walked: walked,
		saw:    make(chan *shared.Event, sawBuffer),
	}
}

func (r *recorder) Name() string { return r.name }

func (r *recorder) Wants() []shared.Want { return r.wants }

func (r *recorder) Setup(args shared.SetupArgs) error {
	if r.setupErr != nil {
		return r.setupErr
	}

	r.args, r.next = args, args.Next

	return nil
}

func (r *recorder) Handle(ev *shared.Event) {
	if r.walked != nil {
		*r.walked = append(*r.walked, r.name)
	}

	r.saw <- ev

	r.next(ev)
}

// actions is every action this recorder saw, in order. Called once Run has
// returned, when nothing else can be writing.
func (r *recorder) actions() []string {
	var out []string

	for {
		select {
		case ev := <-r.saw:
			out = append(out, ev.Action())
		default:
			return out
		}
	}
}

// drainer is a plugin that owns a goroutine and answers what it is asked,
// recording the order it was asked in.
type drainer struct {
	*recorder

	in  <-chan shared.Command
	out chan<- shared.Command

	asked *[]string
}

func newDrainer(name string, asked *[]string) *drainer {
	return &drainer{recorder: newRecorder(name, nil), asked: asked}
}

func (d *drainer) Setup(args shared.SetupArgs) error {
	d.in, d.out = args.CommandIn, args.CommandOut

	return d.recorder.Setup(args)
}

// Run is here so the source counts this as a plugin with a goroutine to drain.
// It never runs; answer is what a test drives instead.
func (d *drainer) Run(ctx context.Context) error {
	<-ctx.Done()

	return nil
}

// answer takes one command and sends it back, noting who was asked.
func (d *drainer) answer() {
	cmd := <-d.in

	*d.asked = append(*d.asked, d.Name())

	d.out <- cmd
}

// listener hands out prepared streams, one per session, so a test can close one
// and watch the next open. Called from Run's goroutine alone.
type listener struct {
	streams []chan incusapi.Event
	opened  int
}

// errNoStreamLeft is what a test's listener answers once its prepared streams
// are used up. The source treats it like any other refusal: it backs off and
// tries again, which is what keeps the loop running until the test cancels.
var errNoStreamLeft = errors.New("no stream left")

func (l *listener) open(_ context.Context) (<-chan incusapi.Event, error) {
	if l.opened >= len(l.streams) {
		return nil, errNoStreamLeft
	}

	l.opened++

	return l.streams[l.opened-1], nil
}

// mustSource builds a source over the plugins, with no connection: every test
// below hands the stream over itself.
func mustSource(t *testing.T, plugins ...shared.Plugin) *Source {
	t.Helper()

	s, err := New(t.Context(), nil, plugins)
	require.NoError(t, err)

	return s
}

// runSource starts Run and returns what it came back with.
func runSource(ctx context.Context, s *Source) <-chan error {
	out := make(chan error, 1)

	go func() { out <- s.Run(ctx) }()

	return out
}

// instanceEvent is one instance action as incusd sends it.
func instanceEvent(t *testing.T, action, project, name string) incusapi.Event {
	t.Helper()

	return rawEvent(t, project, incusapi.EventLifecycle{Action: action, Name: name})
}

// TestNewRefusesAPluginListedTwice pins the one wiring mistake that would be
// silent: Setup called twice on one value has the second successor overwrite
// the first, so the chain would skip whatever sat between the two positions.
func TestNewRefusesAPluginListedTwice(t *testing.T) {
	twice := newRecorder("trace", nil)

	_, err := New(t.Context(), nil, []shared.Plugin{twice, newRecorder("dns", nil), twice})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "trace")
}

// TestNewStopsOnASetupError pins that configuration which cannot work is
// refused before anything is running, rather than degrading once events flow.
func TestNewStopsOnASetupError(t *testing.T) {
	bad := newRecorder("dns", nil)
	bad.setupErr = errors.New("no data_dir")

	_, err := New(t.Context(), nil, []shared.Plugin{newRecorder("log", nil), bad})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "dns")
}

func TestNewUnionsWants(t *testing.T) {
	const action = incusapi.EventLifecycleInstanceUpdated

	cases := []struct {
		name string
		a, b []shared.Want
		want shared.Want
	}{
		{
			name: "a lone want stands as it was declared",
			a:    []shared.Want{{Action: action, Enrich: shared.EnrichedInstance, Debounce: true}},
			want: shared.Want{Action: action, Enrich: shared.EnrichedInstance, Debounce: true},
		},
		{
			// Two plugins wanting different depths of one action cost the union
			// of what they asked for, and one read serves both.
			name: "enrichment is the union of what everybody asked for",
			a:    []shared.Want{{Action: action, Enrich: shared.EnrichedInstance, Debounce: true}},
			b:    []shared.Want{{Action: action, Enrich: shared.EnrichedNetworks, Debounce: true}},
			want: shared.Want{
				Action:   action,
				Enrich:   shared.EnrichedInstance | shared.EnrichedNetworks,
				Debounce: true,
			},
		},
		{
			// The zero value vetoes, so a plugin that forgot to ask for the
			// saving sees every event rather than losing one.
			name: "one plugin needing every event stops the collapsing",
			a:    []shared.Want{{Action: action, Enrich: shared.EnrichedInstance, Debounce: true}},
			b:    []shared.Want{{Action: action}},
			want: shared.Want{Action: action, Enrich: shared.EnrichedInstance},
		},
		{
			name: "and it vetoes from either position",
			a:    []shared.Want{{Action: action}},
			b:    []shared.Want{{Action: action, Enrich: shared.EnrichedInstance, Debounce: true}},
			want: shared.Want{Action: action, Enrich: shared.EnrichedInstance},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := newRecorder("a", nil, tc.a...)
			b := newRecorder("b", nil, tc.b...)

			s := mustSource(t, a, b)

			assert.Equal(t, map[string]shared.Want{action: tc.want}, s.wants)

			// The same finished table for everybody, whichever side of the
			// enricher they sit. A plugin in front is handed what the plugins
			// behind it asked for, which is what debounce reads its own answer
			// out of.
			assert.Equal(t, s.wants, a.args.Wanted)
			assert.Equal(t, s.wants, b.args.Wanted)
		})
	}
}

// TestNewWiresTheChainForwards pins that main lists the chain in the order it
// runs, even though the source wires it backwards to give each plugin the one
// after it.
func TestNewWiresTheChainForwards(t *testing.T) {
	var walked []string

	s := mustSource(t,
		newRecorder("log", &walked),
		newRecorder("debounce", &walked),
		newRecorder("dns", &walked),
	)

	// The last plugin's successor does nothing, so the walk simply unwinds.
	s.hand(shared.NewEvent(time.Now(), incusapi.EventLifecycleInstanceStarted, "shop", "web", ""))

	assert.Equal(t, []string{"log", "debounce", "dns"}, walked)
}

// TestRunWithoutAConnection covers a source that has no stream to open and no
// listener handed to it.
func TestRunWithoutAConnection(t *testing.T) {
	s := mustSource(t, newRecorder("dns", nil))

	require.ErrorIs(t, s.Run(t.Context()), errNoConnection)
}

// TestDrainAsksInChainOrder pins the whole reason draining is a command: a
// plugin is asked only once the plugin feeding it has answered, so nothing is
// still pushing into an inbox whose reader has stopped.
func TestDrainAsksInChainOrder(t *testing.T) {
	var asked []string

	a := newDrainer("a", &asked)
	b := newDrainer("b", &asked)
	c := newDrainer("c", &asked)

	s := mustSource(t, a, b, c)

	// Answered in reverse, so passing could not be an accident of who replied
	// first: the order asserted below is the order they were asked in.
	go c.answer()
	go b.answer()
	go a.answer()

	s.Drain(t.Context())

	assert.Equal(t, []string{"a", "b", "c"}, asked)
}

// TestDrainSkipsAPluginThatIsNotRunning covers the two ways a plugin cannot
// answer: it never had a goroutine, or its Run has already returned. Either way
// the shutdown carries on rather than waiting out its budget.
func TestDrainSkipsAPluginThatIsNotRunning(t *testing.T) {
	var asked []string

	// log has no Run at all, so the source never asks it.
	quiet := log.New(log.At("quiet"))
	gone := newDrainer("gone", &asked)
	live := newDrainer("live", &asked)

	s := mustSource(t, quiet, gone, live)

	// Started and returned without ever answering, the way a plugin that
	// failed does.
	s.Finished(gone)

	go live.answer()

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	s.Drain(ctx)

	// Neither the plugin with no goroutine nor the one that already returned is
	// asked, and the one that is still running is.
	assert.Equal(t, []string{"live"}, asked)
	assert.NoError(t, ctx.Err(), "Drain waited for an answer that could not come")
}

// TestRunBracketsASessionWithConnectedAndDisconnected pins the source's own
// state riding the chain as events. The enricher reads a whole fleet off the
// back of connected, so a session that opened without saying so would leave
// everything held from before the outage standing.
func TestRunBracketsASessionWithConnectedAndDisconnected(t *testing.T) {
	stream := make(chan incusapi.Event, 4)
	rec := newRecorder("dns", nil)

	s := mustSource(t, rec)
	s.listen = (&listener{streams: []chan incusapi.Event{stream}}).open

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	done := runSource(ctx, s)

	assert.Equal(t, shared.ActionConnected, (<-rec.saw).Action())

	close(stream)

	assert.Equal(t, shared.ActionDisconnected, (<-rec.saw).Action())

	cancel()
	require.NoError(t, <-done)
}

// TestRunWalksOnlyWhatSomebodyWanted pins the table deciding what enters the
// chain at all. Most of a lifecycle stream is actions no plugin here asked for,
// and walking those would cost every plugin behind a call per event.
func TestRunWalksOnlyWhatSomebodyWanted(t *testing.T) {
	stream := make(chan incusapi.Event, 4)

	rec := newRecorder("dns", nil,
		shared.Want{Action: incusapi.EventLifecycleInstanceStarted, Enrich: shared.EnrichedInstance})

	s := mustSource(t, rec)
	s.listen = (&listener{streams: []chan incusapi.Event{stream}}).open

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	done := runSource(ctx, s)

	assert.Equal(t, shared.ActionConnected, (<-rec.saw).Action())

	stream <- instanceEvent(t, incusapi.EventLifecycleInstanceStarted, "shop", "web")
	// Wanted by nobody, so it goes nowhere.
	stream <- instanceEvent(t, incusapi.EventLifecycleInstanceCreated, "shop", "db")
	// Malformed rather than uninteresting, and it goes nowhere either.
	stream <- incusapi.Event{Type: incusapi.EventTypeLifecycle, Project: "shop", Metadata: []byte("{")}

	// Everything buffered is read before the close is seen, so the two that
	// went nowhere had their chance before disconnected arrives.
	close(stream)

	assert.Equal(t, incusapi.EventLifecycleInstanceStarted, (<-rec.saw).Action())
	assert.Equal(t, shared.ActionDisconnected, (<-rec.saw).Action())

	cancel()
	require.NoError(t, <-done)

	// And nothing else ever walked.
	assert.Empty(t, rec.actions())
}

// TestRunHandsCommandsOverAtTheHead pins where a plugin's own action enters:
// at the head, on the goroutine that hands events over, and behind whatever
// that goroutine has already handed on. A bracket that overtook the events it
// brackets is the whole reason the sweep rides the chain rather than a path of
// its own.
func TestRunHandsCommandsOverAtTheHead(t *testing.T) {
	stream := make(chan incusapi.Event, 4)

	rec := newRecorder("enricher", nil,
		shared.Want{Action: incusapi.EventLifecycleInstanceStarted, Enrich: shared.EnrichedInstance})

	s := mustSource(t, rec)
	s.listen = (&listener{streams: []chan incusapi.Event{stream}}).open

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	done := runSource(ctx, s)

	assert.Equal(t, shared.ActionConnected, (<-rec.saw).Action())

	stream <- instanceEvent(t, incusapi.EventLifecycleInstanceStarted, "shop", "web")
	assert.Equal(t, incusapi.EventLifecycleInstanceStarted, (<-rec.saw).Action())

	// Raised the way the enricher raises a bracket: on the plugin's own
	// CommandOut, from its own goroutine, which is this one.
	rec.args.CommandOut <- shared.Command{Action: shared.ActionSweepStart}

	ev := <-rec.saw
	assert.Equal(t, shared.ActionSweepStart, ev.Action())

	// The source's own actions name nothing, which the source/ namespace
	// already says - and it is what puts them straight through debounce.
	assert.Empty(t, ev.Project())
	assert.Empty(t, ev.Name())
	assert.Equal(t, shared.StateOk, ev.State())

	cancel()
	require.NoError(t, <-done)
}

// TestRunReopensAClosedStream covers a reconnect, and the gap in the middle of
// one. A command raised while there is no stream still has to be handed over:
// the enricher holds a finished pass until the bracket it raised comes round,
// and a pass that failed is exactly what a lost stream produces.
func TestRunReopensAClosedStream(t *testing.T) {
	first := make(chan incusapi.Event, 1)
	second := make(chan incusapi.Event, 1)

	rec := newRecorder("dns", nil)
	list := &listener{streams: []chan incusapi.Event{first, second}}

	s := mustSource(t, rec)
	s.listen = list.open

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	done := runSource(ctx, s)

	assert.Equal(t, shared.ActionConnected, (<-rec.saw).Action())

	close(first)

	assert.Equal(t, shared.ActionDisconnected, (<-rec.saw).Action())

	// The source is now in the backoff between sessions, with no stream at all.
	rec.args.CommandOut <- shared.Command{Action: shared.ActionSweepStart}
	assert.Equal(t, shared.ActionSweepStart, (<-rec.saw).Action())

	assert.Equal(t, shared.ActionConnected, (<-rec.saw).Action())

	cancel()
	require.NoError(t, <-done)

	assert.Equal(t, 2, list.opened)
}
