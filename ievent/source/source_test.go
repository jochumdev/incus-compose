package source

import (
	"context"
	"errors"
	"testing"
	"time"

	incusapi "github.com/lxc/incus/v7/shared/api"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lxc/incus-compose/ievent/iutil"
	"github.com/lxc/incus-compose/ievent/log"
)

// sawBuffer is generous: overrunning it would deadlock the source rather than
// fail the test.
const sawBuffer = 128

// recorder is a plugin that keeps what walked past it, written from Handle's
// goroutine and read by the test only afterwards - no lock needed.
type recorder struct {
	name     string
	wants    []iutil.Want
	setupErr error

	next iutil.Next

	// args is what Setup was handed, so a test can assert on it or raise
	// commands like a plugin.
	args iutil.SetupArgs

	// walked is shared across every recorder in a test, so its order is the
	// walk order.
	walked *[]string

	// saw is every event that walked past, in order; a test reads one to
	// wait for the source.
	saw chan *iutil.Event
}

func newRecorder(name string, walked *[]string, wants ...iutil.Want) *recorder {
	return &recorder{
		name:   name,
		wants:  wants,
		walked: walked,
		saw:    make(chan *iutil.Event, sawBuffer),
	}
}

func (r *recorder) Name() string { return r.name }

func (r *recorder) Wants() []iutil.Want { return r.wants }

func (r *recorder) Setup(args iutil.SetupArgs) error {
	if r.setupErr != nil {
		return r.setupErr
	}

	r.args, r.next = args, args.Next

	return nil
}

func (r *recorder) Handle(ev *iutil.Event) {
	if r.walked != nil {
		*r.walked = append(*r.walked, r.name)
	}

	r.saw <- ev

	r.next(ev)
}

// actions is every action this recorder saw, in order; call only after Run
// has returned.
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

	in  <-chan iutil.Command
	out chan<- iutil.Command

	asked *[]string
}

func newDrainer(name string, asked *[]string) *drainer {
	return &drainer{recorder: newRecorder(name, nil), asked: asked}
}

func (d *drainer) Setup(args iutil.SetupArgs) error {
	d.in, d.out = args.CommandIn, args.CommandOut

	return d.recorder.Setup(args)
}

// Run only makes the source count this as having a goroutine; answer drives it instead.
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

// listener hands out prepared streams, one per session, so a test can close
// one and watch the next open. Called from Run's goroutine alone.
type listener struct {
	streams []chan incusapi.Event
	opened  int
}

// errNoStreamLeft is what a test's listener answers once its streams run out;
// the source treats it like any other refusal and backs off.
var errNoStreamLeft = errors.New("no stream left")

func (l *listener) open(_ context.Context) (<-chan incusapi.Event, error) {
	if l.opened >= len(l.streams) {
		return nil, errNoStreamLeft
	}

	l.opened++

	return l.streams[l.opened-1], nil
}

// mustSource builds a source with no connection; every test hands the stream
// over itself.
func mustSource(t *testing.T, plugins ...iutil.Plugin) *Source {
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

// TestNewRefusesAPluginListedTwice pins the wiring mistake that would be
// silent: a duplicate's second Setup call overwrites the first's successor.
func TestNewRefusesAPluginListedTwice(t *testing.T) {
	t.Parallel()

	twice := newRecorder("trace", nil)

	_, err := New(t.Context(), nil, []iutil.Plugin{twice, newRecorder("dns", nil), twice})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "trace")
}

// TestNewStopsOnASetupError pins that bad configuration is refused before
// anything runs.
func TestNewStopsOnASetupError(t *testing.T) {
	t.Parallel()

	bad := newRecorder("dns", nil)
	bad.setupErr = errors.New("no data_dir")

	_, err := New(t.Context(), nil, []iutil.Plugin{newRecorder("log", nil), bad})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "dns")
}

func TestNewUnionsWants(t *testing.T) {
	t.Parallel()

	const action = incusapi.EventLifecycleInstanceUpdated

	cases := []struct {
		name string
		a, b []iutil.Want
		want iutil.Want
	}{
		{
			name: "a lone want stands as it was declared",
			a:    []iutil.Want{{Action: action, Enrich: iutil.EnrichedInstance, Debounce: true}},
			want: iutil.Want{Action: action, Enrich: iutil.EnrichedInstance, Debounce: true},
		},
		{
			// Two plugins wanting different depths cost the union; one read serves both.
			name: "enrichment is the union of what everybody asked for",
			a:    []iutil.Want{{Action: action, Enrich: iutil.EnrichedInstance, Debounce: true}},
			b:    []iutil.Want{{Action: action, Enrich: iutil.EnrichedNetwork, Debounce: true}},
			want: iutil.Want{
				Action:   action,
				Enrich:   iutil.EnrichedInstance | iutil.EnrichedNetwork,
				Debounce: true,
			},
		},
		{
			// The zero value vetoes: forgetting means seeing every event, never losing one.
			name: "one plugin needing every event stops the collapsing",
			a:    []iutil.Want{{Action: action, Enrich: iutil.EnrichedInstance, Debounce: true}},
			b:    []iutil.Want{{Action: action}},
			want: iutil.Want{Action: action, Enrich: iutil.EnrichedInstance},
		},
		{
			name: "and it vetoes from either position",
			a:    []iutil.Want{{Action: action}},
			b:    []iutil.Want{{Action: action, Enrich: iutil.EnrichedInstance, Debounce: true}},
			want: iutil.Want{Action: action, Enrich: iutil.EnrichedInstance},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			a := newRecorder("a", nil, tc.a...)
			b := newRecorder("b", nil, tc.b...)

			s := mustSource(t, a, b)

			assert.Equal(t, map[string]iutil.Want{action: tc.want}, s.wants)

			// Every plugin gets the same finished table, regardless of position.
			assert.Equal(t, s.wants, a.args.Wanted)
			assert.Equal(t, s.wants, b.args.Wanted)
		})
	}
}

// TestNewWiresTheChainForwards pins that main lists the chain in run order,
// even though the source wires it backwards to give each plugin the one after it.
func TestNewWiresTheChainForwards(t *testing.T) {
	t.Parallel()

	var walked []string

	s := mustSource(t,
		newRecorder("log", &walked),
		newRecorder("debounce", &walked),
		newRecorder("dns", &walked),
	)

	// The last plugin's successor does nothing, so the walk simply unwinds.
	s.hand(iutil.NewEvent(time.Now(), incusapi.EventLifecycleInstanceStarted, "shop", "web", ""))

	assert.Equal(t, []string{"log", "debounce", "dns"}, walked)
}

// TestRunWithoutAConnection covers a source with no stream to open and no
// listener handed to it.
func TestRunWithoutAConnection(t *testing.T) {
	t.Parallel()

	s := mustSource(t, newRecorder("dns", nil))

	require.ErrorIs(t, s.Run(t.Context()), errNoConnection)
}

// TestDrainAsksInChainOrder pins the reason draining is ordered: a plugin is
// asked only once the one feeding it has answered.
func TestDrainAsksInChainOrder(t *testing.T) {
	t.Parallel()

	var asked []string

	a := newDrainer("a", &asked)
	b := newDrainer("b", &asked)
	c := newDrainer("c", &asked)

	s := mustSource(t, a, b, c)

	// Answered in reverse, so passing isn't an accident of reply order.
	go c.answer()
	go b.answer()
	go a.answer()

	s.Drain(t.Context())

	assert.Equal(t, []string{"a", "b", "c"}, asked)
}

// TestDrainSkipsAPluginThatIsNotRunning covers the two ways a plugin cannot
// answer: no goroutine, or Run already returned.
func TestDrainSkipsAPluginThatIsNotRunning(t *testing.T) {
	t.Parallel()

	var asked []string

	// log has no Run at all, so the source never asks it.
	quiet := log.New(log.At("quiet"))
	gone := newDrainer("gone", &asked)
	live := newDrainer("live", &asked)

	s := mustSource(t, quiet, gone, live)

	// Started and returned without answering, the way a failed plugin does.
	s.Finished(gone)

	go live.answer()

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	s.Drain(ctx)

	// Only the plugin still running gets asked.
	assert.Equal(t, []string{"live"}, asked)
	assert.NoError(t, ctx.Err(), "Drain waited for an answer that could not come")
}

// TestRunBracketsASessionWithConnectedAndDisconnected pins connected/disconnected
// riding the chain as events, which is what the enricher reads a fleet off of.
func TestRunBracketsASessionWithConnectedAndDisconnected(t *testing.T) {
	t.Parallel()

	stream := make(chan incusapi.Event, 4)
	rec := newRecorder("dns", nil)

	s := mustSource(t, rec)
	s.listen = (&listener{streams: []chan incusapi.Event{stream}}).open

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	done := runSource(ctx, s)

	assert.Equal(t, iutil.ActionConnected, (<-rec.saw).Action())

	close(stream)

	assert.Equal(t, iutil.ActionDisconnected, (<-rec.saw).Action())

	cancel()
	require.NoError(t, <-done)
}

// TestRunWalksOnlyWhatSomebodyWanted pins the wants table deciding what enters
// the chain; most of a lifecycle stream is actions nobody here asked for.
func TestRunWalksOnlyWhatSomebodyWanted(t *testing.T) {
	t.Parallel()

	stream := make(chan incusapi.Event, 4)

	rec := newRecorder("dns", nil,
		iutil.Want{Action: incusapi.EventLifecycleInstanceStarted, Enrich: iutil.EnrichedInstance})

	s := mustSource(t, rec)
	s.listen = (&listener{streams: []chan incusapi.Event{stream}}).open

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	done := runSource(ctx, s)

	assert.Equal(t, iutil.ActionConnected, (<-rec.saw).Action())

	stream <- instanceEvent(t, incusapi.EventLifecycleInstanceStarted, "shop", "web")
	// Wanted by nobody, so it goes nowhere.
	stream <- instanceEvent(t, incusapi.EventLifecycleInstanceCreated, "shop", "db")
	// Malformed rather than uninteresting, and it goes nowhere either.
	stream <- incusapi.Event{Type: incusapi.EventTypeLifecycle, Project: "shop", Metadata: []byte("{")}

	// Everything buffered is read before the close is seen, so both dropped
	// events had their chance.
	close(stream)

	assert.Equal(t, incusapi.EventLifecycleInstanceStarted, (<-rec.saw).Action())
	assert.Equal(t, iutil.ActionDisconnected, (<-rec.saw).Action())

	cancel()
	require.NoError(t, <-done)

	// And nothing else ever walked.
	assert.Empty(t, rec.actions())
}

// TestRunHandsCommandsOverAtTheHead pins where a plugin's own action enters:
// at the head, after whatever the source's goroutine has already handed on.
func TestRunHandsCommandsOverAtTheHead(t *testing.T) {
	t.Parallel()

	stream := make(chan incusapi.Event, 4)

	rec := newRecorder("enricher", nil,
		iutil.Want{Action: incusapi.EventLifecycleInstanceStarted, Enrich: iutil.EnrichedInstance})

	s := mustSource(t, rec)
	s.listen = (&listener{streams: []chan incusapi.Event{stream}}).open

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	done := runSource(ctx, s)

	assert.Equal(t, iutil.ActionConnected, (<-rec.saw).Action())

	stream <- instanceEvent(t, incusapi.EventLifecycleInstanceStarted, "shop", "web")
	assert.Equal(t, incusapi.EventLifecycleInstanceStarted, (<-rec.saw).Action())

	// Raised the way the enricher raises the end of a round: on its own CommandOut.
	rec.args.CommandOut <- iutil.Command{Action: iutil.ActionSweepEnd}

	ev := <-rec.saw
	assert.Equal(t, iutil.ActionSweepEnd, ev.Action())

	// The source's own actions name nothing, which puts them straight through debounce.
	assert.Empty(t, ev.ProjectName())
	assert.Empty(t, ev.Name())
	assert.Nil(t, ev.Err())

	cancel()
	require.NoError(t, <-done)
}

// TestRunReopensAClosedStream covers a reconnect, and the gap in the middle
// of one: a command raised with no stream open still has to be handed over.
func TestRunReopensAClosedStream(t *testing.T) {
	t.Parallel()

	first := make(chan incusapi.Event, 1)
	second := make(chan incusapi.Event, 1)

	rec := newRecorder("dns", nil)
	list := &listener{streams: []chan incusapi.Event{first, second}}

	s := mustSource(t, rec)
	s.listen = list.open

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	done := runSource(ctx, s)

	assert.Equal(t, iutil.ActionConnected, (<-rec.saw).Action())

	close(first)

	assert.Equal(t, iutil.ActionDisconnected, (<-rec.saw).Action())

	// The source is now in the backoff between sessions, with no stream at all.
	rec.args.CommandOut <- iutil.Command{Action: iutil.ActionSweepEnd}
	assert.Equal(t, iutil.ActionSweepEnd, (<-rec.saw).Action())

	assert.Equal(t, iutil.ActionConnected, (<-rec.saw).Action())

	cancel()
	require.NoError(t, <-done)

	assert.Equal(t, 2, list.opened)
}
