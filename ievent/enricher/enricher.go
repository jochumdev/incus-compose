// Package enricher reads what an event's subject looks like now, and fills the
// rest of the event in from what it already holds.
//
// It is a plugin like any other, at a position in the chain rather than ahead
// of it, which is what lets debounce sit in front and collapse a burst before
// it costs a read.
//
// It wants nothing of its own. What it reads is what everybody else asked for:
// SetupArgs.Wanted is the union of every plugin's Wants, so one action is one
// lookup and one fetch plan, whoever behind here needed it.
//
// Four things happen here, and only the first is a read of the event's own
// subject:
//
//   - an instance action reads that instance, one read in flight per key,
//     with a second event on the key joining the read rather than issuing one.
//
//   - a network action patches the wire and re-reads everything sitting on it,
//     because a subnet moving changes every record on that wire.
//
//   - a profile action re-reads every instance in the project, because a
//     profile re-expands their configuration and the event names none of them.
//     The model already knows who they are, so nothing is read to find out.
//
//   - a pass reads the whole fleet on connect, on its own cadence, and early
//     when a read has failed - bracketed so a plugin can tell absence from
//     silence. It is the whole of the retry policy: a failed read fails its
//     event, notes the key, and is repaired by the pass rather than retried
//     inline where it would stall everything queued behind it.
package enricher

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	incusapi "github.com/lxc/incus/v7/shared/api"
	"github.com/panjf2000/ants/v2"

	"github.com/lxc/incus-compose/iclient"
	"github.com/lxc/incus-compose/ievent/iutil"
)

// name is what this plugin is called, in the chain and in the reason of what it
// fails.
const name = "enricher"

// Defaults, used for whatever main left unset.
const (
	// defaultWorkers caps the reads in flight against one Incus endpoint.
	defaultWorkers = 16

	// defaultPassWorkers caps whole-fleet passes. Its own pool, because a pass
	// holds a worker for its whole life and then needs one per instance to read
	// state: sharing would let every worker be a pass waiting on a worker only
	// a pass can free.
	defaultPassWorkers = 2

	// defaultReadTimeout bounds one instance read.
	defaultReadTimeout = 10 * time.Second

	// defaultSweepInterval is long, because what a pass repairs is held data
	// that recovers: a dead stream is caught by the source's own reconnect, not
	// by this.
	defaultSweepInterval = 30 * time.Minute
)

// dirtyDelay is how soon a pass runs once a read has failed.
//
// Short, because a failed read leaves something stale and nothing else will
// correct it - but not immediate, so a daemon having a bad minute is met with
// one pass rather than one per failure.
const dirtyDelay = 5 * time.Second

// sweepTimeout bounds the whole-fleet read. Longer than one instance, because
// it is one request for all of them.
const sweepTimeout = 2 * time.Minute

// inboxSize is what the inbox absorbs before a drop is the only answer.
//
// The source hands events over from one goroutine, so this is the slack between
// a burst arriving and this plugin's own goroutine getting to it. Matched to
// the Incus client's own event channel, which buffers the same number.
const defaultInboxSize = 1024

// instancePrefix is what an action names an instance with.
//
// Incus prefixes a lifecycle action with the entity it happened to, so this is
// how a read that only knows how to fetch an instance tells whether the event
// in front of it names one. Without it, a name is a name: a network-updated
// carries one too, and asking the daemon for an instance called net0 is the
// bug that shape invites.
const instancePrefix = "instance-"

// profilePrefix is what an action names a profile with.
//
// Any change to one re-expands the configuration and devices of every instance
// using it, and the event names only the profile - so what it costs is a read
// per instance in the project, and never a read of the project to find out who
// that is.
const profilePrefix = "profile-"

// retryDelay paces a submit the pool refused.
//
// Refused rather than queued is the pool's whole point: a submit that waits is
// this goroutine not reading its inbox. So a refusal comes back here, waits
// this long and is offered again - the events behind it keep their place in the
// line, and nothing is failed for the pool being busy.
const retryDelay = 20 * time.Millisecond

// Plugin fills events in from what it holds, reading only the subject of each.
//
// Everything below the inbox belongs to the goroutine Run owns, including the
// model. A pool worker reads Incus and hands the answer back on results; it
// touches nothing here, which is what keeps this package free of a mutex.
type Plugin struct {
	opts options

	// wanted is the source's finished table: what each action has to be read
	// for. An action absent from it never walks, so a lookup that misses is an
	// event nobody asked for and nothing needs doing to it.
	wanted map[string]iutil.Want

	next  iutil.Next
	inbox chan *iutil.Event

	// read and list are what a worker calls. Fields so a test can hand over
	// Incus values without a daemon; Setup and Run fill them in from the
	// connection.
	conn    *iclient.Connection
	read    readFunc
	readNet netReadFunc
	list    listFunc

	// out puts an event in at the head of the chain. The sweep brackets go this
	// way rather than out of next: everything in front of here, debounce most
	// of all, has to see them.
	out chan<- iutil.Command

	// in is the source asking this plugin to finish, on a channel of its own so
	// it arrives whatever the inbox looks like.
	in <-chan iutil.Command

	results chan result
	sweeps  chan sweepResult

	// Everything below belongs to the goroutine Run owns, and is set up there.
	// On the plugin rather than passed down because one plugin is one Run: a
	// parameter list carrying all of it to every step said the same thing at
	// more length.
	pool   *ants.Pool
	passes *ants.Pool
	q      *queue
	m      *model

	// flights is the read out for each key. A second event on a key joins the
	// one already running rather than issuing another: coalescing saves the
	// read, not the event, so both still walk carrying what it found.
	flights map[string]*flight

	// warm says the first whole-fleet pass has landed, so every network an
	// instance might sit on is in wires. An instance can only ever be created
	// on a network that already exists, so once this is true nothing an
	// instance read distills against is unknown for having not been read yet -
	// only for not existing. Never cleared once set: a reconnect leaves wires
	// exactly as stale as everything else this plugin holds, which is what the
	// pass on reconnect is for.
	warm bool

	// cold is every instance key an event arrived for before warm, in arrival
	// order, held in flights like a real flight but never submitted - issued
	// for real the moment the first pass lands.
	cold []string

	// waiting is what the pool refused, oldest first, and retry is when to
	// offer them again.
	waiting []*flight
	retry   *time.Timer

	// sweeping says a pass is out, which is what stops a second one starting
	// behind it. harvest is what the pass read, held until the bracket it
	// raised comes back around.
	sweeping bool
	harvest  *sweepResult

	sweep *time.Timer

	// sweepAt is when the pass is currently due, so that pulling it in never
	// pushes it out. See sooner.
	sweepAt time.Time
}

// options is what main decides about this plugin. Its own rather than a set
// iutil with every other plugin: naming one is already naming this package,
// and nothing here has a window, which is debounce's job in front of it.
type options struct {
	Workers       int
	ReadTimeout   time.Duration
	InboxSize     int
	SweepInterval time.Duration
	Project       func(p *incusapi.Project) bool
}

// Option sets one of them. The zero value of each means unset, and New fills
// this plugin's own default in.
type Option func(*options)

// Workers caps the Incus reads this plugin may have in flight. One endpoint
// fronts a whole cluster, so this bounds load on somebody else's machine.
func Workers(n int) Option { return func(o *options) { o.Workers = n } }

// ReadTimeout bounds one read of the daemon.
func ReadTimeout(d time.Duration) Option { return func(o *options) { o.ReadTimeout = d } }

// InboxSize sets how many events this plugin buffers before it has to drop.
func InboxSize(n int) Option { return func(o *options) { o.InboxSize = n } }

// SweepInterval sets the gap between whole-fleet passes.
func SweepInterval(d time.Duration) Option { return func(o *options) { o.SweepInterval = d } }

// Project sets which projects the binary serves. Nil serves every one the
// certificate can see, which is the standalone default:
//
//	enricher.Project(func(p *incusapi.Project) bool {
//		return p.Config["user.coredns"] == "true"
//	})
func Project(fn func(p *incusapi.Project) bool) Option {
	return func(o *options) { o.Project = fn }
}

// New builds an enricher.
//
// ReadTimeout starts when a worker picks a read up rather than when it was
// first offered to the pool. Waiting for a worker is this plugin being busy,
// and charging that to the daemon's budget would fail reads never given their
// time.
func New(opts ...Option) *Plugin {
	o := options{
		Workers:       defaultWorkers,
		ReadTimeout:   defaultReadTimeout,
		InboxSize:     defaultInboxSize,
		SweepInterval: defaultSweepInterval,
	}

	for _, opt := range opts {
		opt(&o)
	}

	slog.Info("Starting", "plugin", name, "config", o)

	return &Plugin{
		opts:  o,
		inbox: make(chan *iutil.Event, o.InboxSize),
		// Buffered by the same number as there can be reads in flight, so a
		// worker never blocks handing its answer back.
		results: make(chan result, o.Workers),
		sweeps:  make(chan sweepResult, 1),
	}
}

// Name identifies the plugin, and names it in the reason of what it fails.
func (p *Plugin) Name() string { return name }

// Wants nothing, on purpose.
//
// It is the plugin that performs everybody else's wants, so wanting any of its
// own would double-count them. What it must read is SetupArgs.Wanted, which the
// source has finished building before any Setup runs.
func (p *Plugin) Wants() []iutil.Want { return nil }

// Setup keeps the successor, the table and the connection, and starts nothing.
func (p *Plugin) Setup(args iutil.SetupArgs) error {
	p.next = args.Next
	p.wanted = args.Wanted

	p.in, p.out = args.CommandIn, args.CommandOut

	if p.read == nil {
		p.read = incusReader(args.Conn)
	}

	if p.readNet == nil {
		p.readNet = incusNetReader(args.Conn)
	}

	p.conn = args.Conn

	return nil
}

// Handle puts the event on the inbox and returns.
//
// It runs on the previous plugin's goroutine and must not block, so a full
// inbox is a drop rather than a wait. Dropped rather than failed: nothing went
// wrong with a read, this plugin is simply behind, and StateFailed is reserved
// for an event that asked for something and did not get it.
func (p *Plugin) Handle(ev *iutil.Event) {
	select {
	case p.inbox <- ev:
	default:
		p.next(ev.WithDropped(name))
	}
}

// Run enriches until ctx is canceled. It blocks, so main owns the goroutine:
//
//	wg.Go(func() error { return enr.Run(ctx) })
//
// Returning says everything it took has been handed on. Reads in flight are
// abandoned rather than waited for - what they would have filled in is
// enrichment nobody is left to serve, and the events themselves still go.
func (p *Plugin) Run(ctx context.Context) error {
	pool, err := ants.NewPool(p.opts.Workers, ants.WithNonblocking(true))
	if err != nil {
		return fmt.Errorf("creating the read pool: %w", err)
	}

	defer pool.Release()

	passes, err := ants.NewPool(defaultPassWorkers, ants.WithNonblocking(true))
	if err != nil {
		return fmt.Errorf("creating the pass pool: %w", err)
	}

	defer passes.Release()

	p.pool = pool
	p.passes = passes

	if p.list == nil {
		p.list = incusLister(p.conn, pool, p.opts.ReadTimeout, p.opts.Project)
	}
	p.q = &queue{}
	p.m = newModel()
	p.flights = map[string]*flight{}
	p.warm = false
	p.cold = nil
	p.retry = time.NewTimer(retryDelay)
	p.sweep = time.NewTimer(p.opts.SweepInterval)

	p.retry.Stop()

	defer p.retry.Stop()
	defer p.sweep.Stop()

	for {
		select {
		case <-ctx.Done():
			// An abort, not a shutdown. Reads in flight are abandoned and the
			// line goes nowhere, which is what canceling the context means now
			// that finishing is a command.
			return nil

		case cmd := <-p.in:
			// Everything already on the inbox. Nothing is still feeding it: the
			// source has stopped, and debounce has already answered its own
			// drain, which is what the source waited for before asking this one.
		drained:
			for {
				select {
				case ev := <-p.inbox:
					p.accept(ctx, ev)
				default:
					break drained
				}
			}

			// Then the whole line, settled or not. Between the two that is
			// every event this plugin ever took, in the order it took them.
			// Reads still in flight are abandoned rather than waited for - what
			// they would have filled in is enrichment nobody is left to serve.
			for ev := range p.q.drain() {
				p.next(ev)
			}

			// Answered last, so the plugin behind is asked only once everything
			// this one held has been pushed into it.
			select {
			case p.out <- cmd:
			case <-ctx.Done():
			}

			return nil

		case ev := <-p.inbox:
			p.accept(ctx, ev)

			for ev := range p.q.release() {
				p.next(ev)
			}

		case res := <-p.sweeps:
			if res.err != nil {
				// Nothing is patched from a pass that failed and no bracket is
				// raised: what is held is older than it should be, and a
				// bracket around no news would have everything behind here
				// prune a fleet it simply could not read.
				//
				// Said out loud, though. Nothing else reports it - no event
				// walks, no metric moves and no bracket appears - so a pass
				// failing every five seconds for ever looks exactly like a
				// server with nothing to do. That silence is what a marker
				// matching no project reads as.
				slog.Warn("the whole-fleet pass failed, serving what is held",
					"err", res.err, "retrying_in", dirtyDelay)

				p.sweeping = false

				p.sooner(dirtyDelay)

				break
			}

			slog.Debug("the whole-fleet pass read",
				"instances", len(res.instances),
				"networks", len(res.networks),
				"projects", len(res.labels))

			// Held until the bracket comes back around. The bracket goes in at
			// the head and travels the chain to get here, so announcing the
			// pass before it arrived would put the news in front of the notice.
			p.harvest = &res
			p.raise(ctx, iutil.ActionSweepStart)

		case <-p.sweep.C:
			p.startSweep(ctx)

		case res := <-p.results:
			// The patch happens here rather than in the worker: the model is
			// this goroutine's, and a worker that touched it would need a lock
			// nothing else in this package has.
			f := res.flight
			delete(p.flights, f.key)

			var e *instance

			switch {
			case res.err != nil:

			case f.network:
				// The wire first, then everything on it, so the re-reads
				// resolve against what was just read rather than what it
				// replaced.
				p.m.putWire(*res.wire)
				p.fanOut(ctx, p.m.instancesOn(f.key[2:]))

			default:
				e = p.m.putInstance(res.instance, res.state)
			}

			switch {
			case res.err == nil:

			case incusapi.StatusErrorCheck(res.err, http.StatusNotFound):
				// Read after it went. Nothing is owed a repair, and the delete
				// that overtook this is on its way with the same news.
				p.m.dropInstance(f.project, f.name)

			default:
				// Noted rather than retried. The pass repairs it, and pulling
				// one in early is the whole of the retry policy - a backoff
				// here would stall every event queued behind this one.
				p.m.markDirty(f.project, f.name)
				p.sooner(dirtyDelay)
			}

			for _, it := range f.items {
				if res.err != nil && !incusapi.StatusErrorCheck(res.err, http.StatusNotFound) {
					p.q.settle(it, it.ev.WithFailed("source/read"))
					continue
				}

				if e == nil {
					p.q.settle(it, it.ev)
					continue
				}

				p.q.settle(it, it.ev.WithInstance(e.running, e.config, e.nets))
			}

			for ev := range p.q.release() {
				p.next(ev)
			}

		case <-p.retry.C:
			// Offered again in the order they were refused, and the first
			// refusal stops the round: the pool is still full, and walking the
			// rest would only reorder what has been waiting longest.
			for len(p.waiting) > 0 {
				err := p.submit(ctx, p.waiting[0])
				if err != nil {
					break
				}

				p.waiting = p.waiting[1:]
			}

			if len(p.waiting) > 0 {
				p.retry.Reset(retryDelay)
			}
		}
	}
}

// raise tells the chain something, at the head rather than out of next: this
// plugin sits at a position, and debounce - the backbuffer a bracket exists to
// flush - is in front of it.
func (p *Plugin) raise(ctx context.Context, action string) {
	select {
	case p.out <- iutil.Command{Action: action}:
	case <-ctx.Done():
	}
}

// withProject attaches the project's own labels, where somebody asked for them.
//
// Out of the model rather than a read of its own. A project's configuration is
// read once per whole-fleet pass, off the listing the pass already does, and an
// instance moving is not a reason to read it again - which is the same argument
// the fan-out rests on.
//
// A project the pass has not reached is left unenriched rather than enriched
// with nothing, so a consumer can tell "this project sets none" from "this
// project has not been read". Both arrive as an empty map otherwise, and only
// one of them is worth acting on.
func (p *Plugin) withProject(ev *iutil.Event) *iutil.Event {
	if p.wanted[ev.Action()].Enrich&iutil.EnrichedProject == 0 {
		return ev
	}

	config, known := p.m.projects[ev.Project()]
	if !known {
		return ev
	}

	return ev.WithProject(config)
}

// accept puts one event in the line, issuing whatever read it needs first.
func (p *Plugin) accept(ctx context.Context, ev *iutil.Event) {
	// The source reporting a stream is the one thing that makes everything held
	// here suspect at once: whatever happened while it was down was announced
	// to nobody.
	if ev.Action() == iutil.ActionConnected {
		p.q.push(ev, true)
		p.startSweep(ctx)

		return
	}

	// The bracket this plugin raised, having traveled the chain. Everything
	// the pass found goes out behind it, before the closing bracket does.
	if ev.Action() == iutil.ActionSweepStart && p.harvest != nil {
		p.q.push(ev, true)
		p.announce(ctx)

		return
	}

	want := p.wanted[ev.Action()]

	// Before every push below, since the labels are the model's and owe nothing
	// to the read this event may or may not go on to need.
	ev = p.withProject(ev)

	// An event somebody has already finished with is walking for the observers,
	// and one with no name has no subject to read. Neither is worth a read, and
	// both keep their place rather than overtaking what is still waiting.
	if ev.State() != iutil.StateOk || ev.Name() == "" {
		p.q.push(ev, true)

		return
	}

	// A wire moving changes every record on it, so that path patches and fans
	// out rather than enriching the event in front of it.
	if p.acceptNetwork(ctx, ev) {
		return
	}

	// A profile re-expands the configuration of every instance using it, and
	// the event names the profile rather than any of them. Reading the project
	// whole to find out what changed is what this fans out to avoid: the model
	// already knows who is in it.
	if strings.HasPrefix(ev.Action(), profilePrefix) {
		p.q.push(ev, true)
		p.fanOut(ctx, p.m.instancesIn(ev.Project()))

		return
	}

	// A delete is the one action that is complete as it stands: what it says is
	// that the subject is gone, and reading to confirm it would be a read whose
	// only possible answer we already have.
	if ev.Action() == incusapi.EventLifecycleInstanceDeleted {
		p.m.dropInstance(ev.Project(), ev.Name())
		p.q.push(ev, true)

		return
	}

	// A rename takes the old name out. The new one is read like any other
	// change, which is what the event that follows this line does.
	if ev.Action() == incusapi.EventLifecycleInstanceRenamed && ev.OldName() != "" {
		p.m.dropInstance(ev.Project(), ev.OldName())
	}

	// Two questions, and both have to be yes. Did somebody ask for an instance
	// read of this action, and does this action name an instance at all? The
	// second is not implied by the first: a network action can be wanted for
	// its networks, and it has a name, and that name is not an instance's.
	instance := strings.HasPrefix(ev.Action(), instancePrefix) &&
		want.Enrich&iutil.EnrichedInstance != 0

	if !instance {
		p.q.push(ev, true)

		return
	}

	it := p.q.push(ev, false)

	p.issueOrHold(ctx, &flight{
		key:     flightKey(false, ev.Project(), ev.Name()),
		project: ev.Project(),
		name:    ev.Name(),
		items:   []*item{it},
	})
}

// issueOrHold sends one instance read, unless the first whole-fleet pass has
// not landed yet - in which case it joins flights the same way issue does, but
// leaves the read for thaw to send once wires can be trusted.
//
// Only instance reads wait: a network read patches the one wire it names
// directly, and a profile or network fan-out only ever names instances the
// model already holds, which is nothing while cold either way.
func (p *Plugin) issueOrHold(ctx context.Context, f *flight) {
	if p.warm {
		p.issue(ctx, f)

		return
	}

	out, holding := p.flights[f.key]
	if holding {
		out.items = append(out.items, f.items...)

		return
	}

	p.flights[f.key] = f
	p.cold = append(p.cold, f.key)
}

// thaw sends every read cold held back, in the order their events arrived.
//
// Called once, from the first pass to land: wires answers for every network an
// instance could be on from here on, so nothing distilled from here on can
// read one as unknown for want of having been read yet.
func (p *Plugin) thaw(ctx context.Context) {
	keys := p.cold
	p.cold = nil
	p.warm = true

	for _, key := range keys {
		f := p.flights[key]
		delete(p.flights, key)

		p.issue(ctx, f)
	}
}

// issue sends one read, or joins the one already out for that key.
//
// Coalescing saves the read, not the event: a second event on a key waits on
// the read already running, and both still walk carrying what it found.
func (p *Plugin) issue(ctx context.Context, f *flight) {
	out, running := p.flights[f.key]
	if running {
		out.items = append(out.items, f.items...)

		return
	}

	p.flights[f.key] = f

	err := p.submit(ctx, f)
	if err != nil {
		// Refused, not failed. It keeps its place in the line and is offered
		// again shortly; nothing is failed for the pool being busy.
		p.waiting = append(p.waiting, f)
		p.retry.Reset(retryDelay)
	}
}

// fanOut re-reads a set of instances nothing named directly.
//
// A profile or a network changed underneath them, so each needs a read of its
// own - but the event that said so named neither. One synthetic instance-updated
// each, put in the line here rather than at the head of the chain: they are
// this plugin's own work, and sending them round would have debounce collapse
// the set into whichever of them arrived last.
func (p *Plugin) fanOut(ctx context.Context, subjects []subject) {
	for _, s := range subjects {
		ev := iutil.NewEvent(time.Now(),
			incusapi.EventLifecycleInstanceUpdated, s.project, s.name, "")

		it := p.q.push(ev, false)

		p.issue(ctx, &flight{
			key:     flightKey(false, s.project, s.name),
			project: s.project,
			name:    s.name,
			items:   []*item{it},
		})
	}
}

// sooner pulls the pass in, and never pushes it out.
//
// Reset alone would do the second. A read failing sets the pass to five seconds
// away; the next failure a second later sets it five seconds away again, and a
// daemon refusing steadily would keep the repair permanently just out of reach.
func (p *Plugin) sooner(d time.Duration) {
	at := time.Now().Add(d)

	if !p.sweepAt.IsZero() && !at.Before(p.sweepAt) {
		return
	}

	p.sweepAt = at

	p.sweep.Reset(d)
}

// due puts the next pass a whole interval away, which only a finished pass does.
func (p *Plugin) due(d time.Duration) {
	p.sweepAt = time.Now().Add(d)

	p.sweep.Reset(d)
}
