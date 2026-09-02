// Package enricher reads what an event's subject looks like now, and fills the
// rest of the event in from what it already holds. See
// docs/root/architecture/ievent/enricher.md for the design.
package enricher

import (
	"context"
	"errors"
	"iter"
	"log/slog"
	"net/http"
	"strings"
	"time"

	incusapi "github.com/lxc/incus/v7/shared/api"

	"github.com/lxc/incus-compose/ievent/iutil"
)

// name is what this plugin is called, in the chain and in failure reasons.
const name = "enricher"

// Defaults, used for whatever main left unset.
const (
	defaultWorkers = 16

	defaultReadTimeout = 10 * time.Second

	// defaultReadDelay is the gap between the reads inside one project.
	defaultReadDelay = 5 * time.Second

	defaultSweepInterval = 6 * time.Hour

	// defaultStoreInterval is how often what this plugin holds is written, and
	// so how much of a fleet a start finds stale.
	defaultStoreInterval = 5 * time.Second
)

// storeStopTimeout bounds the wait for the last write on the way down. A disk
// that will not answer costs the cold store, never the shutdown.
const storeStopTimeout = 5 * time.Second

// retryDelay/retryTries/slowRetryDelay: how soon and how often a failed key
// is read again, and the slower rate it drops to once retryTries is spent.
const (
	retryDelay     = time.Second
	retryTries     = 3
	slowRetryDelay = 5 * time.Second
)

// defaultInboxSize holds a burst before Handle has to drop; matched to the
// Incus client's own event channel size.
const defaultInboxSize = 1024

// wantsInstanceRead is either way a plugin asks for an instance to be read.
// There are no interfaces without the instance they belong to, so naming only
// the second still asks for one - a plugin that asked for enrichment and got
// none silently would serve an empty fleet and never say why.
const wantsInstanceRead = iutil.EnrichedInstance | iutil.EnrichedInstanceWithInterfaces

// instancePrefix is how an event that names an instance is told from one that
// names a network with the same bare name.
const instancePrefix = "instance-"

// profilePrefix is what an action names a profile with.
const profilePrefix = "profile-"

// The resource a key names. Three characters, hand-picked rather than cut off
// the action: every Incus entity is longer than that, but profile and project
// share their first three.
const (
	kindInstance = "ins"
	kindNetwork  = "net"
	kindProject  = "prj"
)

// errRead is why an event fails: the read it was waiting on did not land.
var errRead = errors.New("enricher/read")

// poolDelay is how long a submit the nonblocking pool refused waits before
// being offered again; nothing is failed for the pool being busy.
const poolDelay = 20 * time.Millisecond

// Plugin fills events in from what it holds, reading only the subject of each.
//
// Everything below the inbox belongs to the goroutine Run owns; a pool worker
// touches nothing here, which is what keeps this package free of a mutex.
type Plugin struct {
	logger *slog.Logger

	args iutil.SetupArgs
	opts options

	inbox chan *iutil.Event

	// sweeper reads the fleet on a goroutine of its own; sweeps is the channel
	// it shares with this one, and sweeperCancel is how a run is abandoned.
	sweeperCancel context.CancelFunc
	sweeps        chan sweepMsg
	// sweepEnding says a run reached its end while its project reads were still
	// out, so the last of them announces it instead.
	sweepEnding bool

	// storeWrite is where a clone of the state is put. A field rather than the
	// path, so a test answers without a disk.
	storeWrite writeFunc

	// The store writes on a goroutine of its own. All three are nil where
	// nothing is written, and a nil channel is one a select never picks.
	storeIn   chan *state
	storeDone chan struct{}
	storeTick <-chan time.Time

	// Everything below belongs to the goroutine Run owns.
	q       *queue
	state   *state
	reads   *deferred
	retries *retries
	archive archive

	// chain is what the last event said, and what this plugin stamps on the ones
	// it creates: those never pass the source, so nothing else would.
	chain iutil.ChainState
}

var _ iutil.Plugin = (*Plugin)(nil)

// options is what main decides about this plugin, kept separate from a shared
// iutil options type since nothing here has a debounce window.
type options struct {
	Workers       int
	ReadTimeout   time.Duration
	InboxSize     int
	ReadDelay     time.Duration
	SweepInterval time.Duration
	StoreInterval time.Duration
	TTL           time.Duration
	Project       func(p *incusapi.Project) bool
	StoreFile     string
}

// Option sets one of the options; New fills in defaults for whatever is left
// zero.
type Option func(*options)

// Workers caps the Incus reads this plugin may have in flight. One endpoint
// fronts a whole cluster, so this bounds load on somebody else's machine.
func Workers(n int) Option { return func(o *options) { o.Workers = n } }

// ReadTimeout bounds one read of the daemon.
func ReadTimeout(d time.Duration) Option { return func(o *options) { o.ReadTimeout = d } }

// InboxSize sets how many events this plugin buffers before it has to drop.
func InboxSize(n int) Option { return func(o *options) { o.InboxSize = n } }

// ReadDelay sets the gap between the reads inside one project.
func ReadDelay(d time.Duration) Option { return func(o *options) { o.ReadDelay = d } }

// SweepInterval sets the duration to wait between sweeps.
func SweepInterval(d time.Duration) Option { return func(o *options) { o.SweepInterval = d } }

// TTL is how long what is held stays valid. Any read revalidates it, whoever
// asked - a run, an event and a re-read all move the same clock - and a key
// still held when it expires is read again. Zero leaves the run as the only
// thing that comes back round.
//
// Set it below the time a run takes on the fleet and every key expires before
// the run reaches it, so the pool carries the whole fleet instead.
func TTL(d time.Duration) Option { return func(o *options) { o.TTL = d } }

// Project sets which projects the binary serves. Nil serves every one the
// certificate can see, which is the standalone default:
//
//	enricher.Project(func(p *incusapi.Project) bool {
//		return p.Config["user.dns"] == "true"
//	})
func Project(fn func(p *incusapi.Project) bool) Option {
	return func(o *options) { o.Project = fn }
}

// StoreFile is the file path to the cold store file, use an empty one to disable.
func StoreFile(f string) Option { return func(o *options) { o.StoreFile = f } }

// StoreInterval is how often what this plugin holds is written, where StoreFile
// says to write it. A run that changed nothing writes nothing whatever this is.
func StoreInterval(d time.Duration) Option { return func(o *options) { o.StoreInterval = d } }

// New builds an enricher.
//
// ReadTimeout starts when a worker picks a read up, not when it is offered to
// the pool, so time spent waiting for a worker is never charged to the daemon.
func New(logger *slog.Logger, opts ...Option) *Plugin {
	o := options{
		Workers:       defaultWorkers,
		ReadTimeout:   defaultReadTimeout,
		InboxSize:     defaultInboxSize,
		ReadDelay:     defaultReadDelay,
		SweepInterval: defaultSweepInterval,
		StoreInterval: defaultStoreInterval,
	}

	for _, opt := range opts {
		opt(&o)
	}

	logger.Info("Starting", "plugin", name, "config", o)

	// Unbuffered, so the sweeper's pace is felt rather than run ahead of.
	sweeps := make(chan sweepMsg)

	p := &Plugin{
		logger: logger,
		opts:   o,
		inbox:  make(chan *iutil.Event, o.InboxSize),
		sweeps: sweeps,

		q:       &queue{},
		state:   newState(o.StoreFile),
		reads:   newDeferred(o.Workers, o.ReadTimeout),
		retries: newRetries(),
		archive: archive{},
	}

	// Nil where nothing is written, which is what the fold and the shutdown
	// both check rather than the path.
	if o.StoreFile != "" {
		p.storeIn = make(chan *state, 1)
		p.storeDone = make(chan struct{})
	}

	return p
}

// storeArgs is what the store goroutine is given.
func (p *Plugin) storeArgs() storeArgs {
	return storeArgs{write: p.storeWrite, in: p.storeIn, done: p.storeDone}
}

// storeClone hands the store what this plugin holds, where anything has changed
// since the last one. On the timer, and once on the way down.
func (p *Plugin) storeClone() {
	if p.storeIn == nil || !p.state.dirty {
		return
	}

	p.state.dirty = false

	storeSend(p.storeIn, p.state.clone())
}

// Name identifies the plugin, and names it in the reason of what it fails.
func (p *Plugin) Name() string { return name }

// Wants the actions it acts on itself, and no read for any of them: it must
// read SetupArgs.Wanted instead, or it would double-count everyone else's.
func (p *Plugin) Wants() []iutil.Want {
	return []iutil.Want{
		// Collapsible: a profile carries no history an update could lose.
		{Action: incusapi.EventLifecycleProfileUpdated, Debounce: true},

		// Not collapsed: a delete lost in a burst is a delete lost.
		{Action: incusapi.EventLifecycleProfileDeleted},
	}
}

// Setup keeps what it was handed and starts nothing; the goroutine is Run's.
func (p *Plugin) Setup(args iutil.SetupArgs) error {
	p.args = args

	if p.reads.read == nil {
		p.reads.read = incusReader(args.Conn)
	}

	if p.reads.readNet == nil {
		p.reads.readNet = args.Conn.GetNetwork
	}

	if p.reads.readProject == nil {
		p.reads.readProject = args.Conn.GetProject
	}

	if p.reads.fleet == nil {
		p.reads.fleet = args.Conn
	}

	if p.storeWrite == nil && p.opts.StoreFile != "" {
		p.storeWrite = fileWriter(p.opts.StoreFile)
	}

	return nil
}

// Handle puts the event on the inbox and returns.
//
// It runs on the previous plugin's goroutine and must not block, so a full
// inbox drops rather than waits; dropped, not failed, since nothing went wrong.
func (p *Plugin) Handle(ev *iutil.Event) {
	select {
	case p.inbox <- ev:
	default:
		p.args.Next(ev.WithDropped(name))
	}
}

// Run enriches until ctx is canceled. It blocks, so main owns the goroutine:
//
//	wg.Go(func() error { return enr.Run(ctx) })
//
// Reads in flight are abandoned rather than waited for; the events themselves
// still go.
func (p *Plugin) Run(ctx context.Context) error {
	err := p.reads.start(p.opts.Workers)
	if err != nil {
		return err
	}

	defer p.reads.stop()
	defer p.retries.stop()

	// Its own context, so the run stops when Run returns, not only when the
	// process does.
	sweepCtx, stopSweeping := context.WithCancel(ctx)
	p.sweeperCancel = stopSweeping
	defer func() {
		if p.sweeperCancel != nil {
			p.sweeperCancel()
		}
	}()

	runSweeper(sweepCtx, p.logger, p.sweepArgs(), p.opts.SweepInterval)

	if p.storeIn != nil {
		ticker := time.NewTicker(p.opts.StoreInterval)
		defer ticker.Stop()

		p.storeTick = ticker.C

		// Its own context, not this one: a clone offered on the way down still
		// has to be written, and an abort cancels ctx before that happens.
		storeCtx, stopStore := context.WithCancel(context.Background())

		defer func() {
			p.storeClone()
			stopStore()

			select {
			case <-p.storeDone:
			case <-time.After(storeStopTimeout):
				p.logger.Warn("the fleet was still being written when this stopped waiting",
					"plugin", name)
			}
		}()

		runStore(storeCtx, p.logger, p.storeArgs())
	}

	drain := func(cmd iutil.Command) {
	DRAINED:
		for {
			select {
			case ev := <-p.inbox:
				p.accept(ctx, ev)
			default:
				break DRAINED
			}
		}

		// Then the whole line, settled or not, in arrival order. Reads
		// still in flight are abandoned rather than waited for.
		for ev := range p.q.drain() {
			p.args.Next(ev)
		}

		// Answered last, so the plugin after this one is asked only once
		// everything this one held has been pushed into it.
		select {
		case p.args.CommandOut <- cmd:
		case <-ctx.Done():
		}
	}

	for {
		select {
		case <-ctx.Done():
			// An abort, not a shutdown: reads in flight are abandoned.
			return nil

		case cmd := <-p.args.CommandIn:
			// Everything already on the inbox; nothing feeds it further once the
			// source has stopped.
			drain(cmd)
			return nil

		case ev := <-p.inbox:
			p.accept(ctx, ev)

			for ev := range p.q.release() {
				p.args.Next(ev)
			}

		case msg := <-p.sweeps:
			p.acceptSweep(ctx, msg)

		case res := <-p.reads.results:
			// The patch happens here, not in the worker: the state belongs to
			// this goroutine.
			p.settleRead(ctx, res)

			for ev := range p.q.release() {
				p.args.Next(ev)
			}

		case <-p.retries.timer.C:
			// Through the fan-out: a re-read has to make an event of its own,
			// or what it finds reaches nobody.
			p.fanOut(ctx, p.retries.take(time.Now()))

		case <-p.storeTick:
			p.storeClone()

		case <-p.reads.timer.C:
			p.reads.retry(ctx)
		}
	}
}

// withProject attaches the project's own configuration out of the state, not a
// read of its own, where somebody asked for it.
//
// A project no run has reached is left unenriched rather than enriched with
// nothing, so a consumer can tell "sets none" from "not read yet".
func (p *Plugin) withProject(ev *iutil.Event) *iutil.Event {
	if p.args.Wanted[ev.Action()].Enrich&iutil.EnrichedProject == 0 {
		return ev
	}

	config := p.state.projectConfig(ev.ProjectName())
	if config == nil {
		return ev
	}

	return ev.WithProject(iutil.NewProject(config))
}

// readProject reads a project nothing has read yet.
//
// The event goes on unenriched: what lands answers the next one about that
// project. Coalesced by key, so a burst naming one project costs one read, and
// one a run has already asked for costs none.
func (p *Plugin) readProject(ctx context.Context, ev *iutil.Event) {
	if p.args.Wanted[ev.Action()].Enrich&iutil.EnrichedProject == 0 {
		return
	}

	project := ev.ProjectName()
	if project == "" || p.state.projectConfig(project) != nil {
		return
	}

	p.reads.send(ctx, &call{
		key:     resourceKey(kindProject, project, ""),
		kind:    kindProject,
		project: project,
	})
}

// readNetwork reads a network a NIC named and nothing has read.
//
// Asked for in the instance's own project: a project without features.networks
// references the default project's, and Incus answers with the project on it, so
// one read resolves either shape.
func (p *Plugin) readNetwork(ctx context.Context, project, name string) {
	if name == "" {
		return
	}

	p.reads.send(ctx, &call{
		key:     resourceKey(kindNetwork, project, name),
		kind:    kindNetwork,
		project: project,
		name:    name,
	})
}

// settleRead patches the state from one read and settles whatever was waiting
// on it. It runs on the goroutine Run owns, because the state does.
func (p *Plugin) settleRead(ctx context.Context, res result) {
	c := res.call
	p.reads.done(ctx, c)

	switch c.kind {
	case kindNetwork:
		if res.err != nil {
			// Left to the run: a re-read here would ask the daemon for an
			// instance by this name, since the fan-out only makes those.
			return
		}

		// The network first, so the re-reads resolve against it, not what it
		// replaced. Keyed by the project that owns it rather than the one that
		// named it, which for a bridge may be the default project.
		p.state.setNetwork(*res.network)
		p.fanOut(ctx, p.state.networkInstances(res.network.Project, res.network.Name))

		return

	case kindProject:
		if res.err == nil {
			p.state.setProject(c.project, res.project.Config)
		}

		// A project that would not answer still counts: the run is over either
		// way, and holding warm on it would leave the chain cold for ever.
		if p.sweepEnding && p.reads.owes() == 0 {
			p.sweepEnd(ctx)
		}

		return
	}

	inst, landed := p.patchState(ctx, c.project, c.name, res.instance, res.state, res.err)

	// Before the event it made below, which is then compared against what they
	// filed.
	for _, it := range c.items {
		if !landed {
			// Not filed: the next read compares against the last answer there
			// was, not one that did not land.
			p.q.settle(it, it.ev.WithFailed(errRead))

			continue
		}

		ev := it.ev
		if inst != nil {
			// Merged after the read, not before: the instance's own side of it
			// is what the read just brought back.
			ev = p.withProject(ev.WithInstance(inst, true))
		}

		if !p.archive.changed(ev) {
			p.q.trash(it)

			continue
		}

		p.q.settle(it, ev)
	}

	// Through withProject like every other instance event emitted here, or it is
	// not the same event a live one on this key is.
	if c.ev != nil && inst != nil {
		syn := p.withProject(c.ev.WithInstance(inst, true))
		if p.archive.changed(syn) {
			p.q.push(syn, true)
		}
	}
}

// accept puts one event in the line, issuing whatever read it needs first.
func (p *Plugin) accept(ctx context.Context, ev *iutil.Event) {
	if ev.ChainState() == "" {
		// The run's own events arrive here unstamped: nothing minted them past
		// the source, so this plugin carries over what it last saw.
		ev = ev.WithChainState(p.chain)
	} else {
		p.chain = ev.ChainState()
	}

	// A stream coming back makes everything held here suspect at once: whatever
	// happened while it was down was announced to nobody.
	if ev.Action() == iutil.ActionConnected {
		p.q.push(ev, true)
		p.restartSweep(ctx)

		return
	}

	want := p.args.Wanted[ev.Action()]

	// Before every push below, since what the project sets is the state's and
	// owes nothing to the read this event may or may not go on to need.
	ev = p.withProject(ev)

	p.readProject(ctx, ev)

	// Already finished, or no name to read: neither is worth a read, and both
	// keep their place rather than overtaking what is still waiting.
	if ev.Err() != nil || ev.Name() == "" {
		p.q.push(ev, true)

		return
	}

	// A network moving changes every record on it, so that path patches and fans
	// out rather than enriching the event before it.
	if p.acceptNetwork(ctx, ev) {
		return
	}

	// A profile re-expands every instance using it, so this fans out over the
	// state. Except a delete, which Incus refuses while anything still uses it.
	if strings.HasPrefix(ev.Action(), profilePrefix) {
		p.q.push(ev, true)

		if ev.Action() != incusapi.EventLifecycleProfileDeleted {
			p.fanOut(ctx, p.state.projectInstances(ev.ProjectName()))
		}

		return
	}

	// A delete is complete as it stands; reading to confirm it would answer a
	// question we already have the answer to.
	if ev.Action() == incusapi.EventLifecycleInstanceDeleted {
		p.state.deleteInstance(ev.ProjectName(), ev.Name())
		p.archive.forget(ev.ProjectName(), ev.Name())
		p.q.push(ev, true)

		return
	}

	// A rename takes the old name out. The new one is read like any other
	// change, which is what the event that follows this line does.
	if ev.Action() == incusapi.EventLifecycleInstanceRenamed && ev.OldName() != "" {
		p.state.deleteInstance(ev.ProjectName(), ev.OldName())
		p.archive.forget(ev.ProjectName(), ev.OldName())
	}

	// Both have to be true: wanted for instance enrichment, and the action
	// actually names an instance (a network action can be wanted too).
	instance := strings.HasPrefix(ev.Action(), instancePrefix) &&
		want.Enrich&wantsInstanceRead != 0

	if !instance {
		p.q.push(ev, true)

		return
	}

	it := p.q.push(ev, false)

	c := &call{
		key:     resourceKey(kindInstance, ev.ProjectName(), ev.Name()),
		kind:    kindInstance,
		project: ev.ProjectName(),
		name:    ev.Name(),
		items:   []*item{it},
	}

	p.reads.send(ctx, c)
}

// fanOut re-reads a set of instances nothing named directly, each as an
// instance-updated of its own held on its call, going on only if the read
// found something new.
func (p *Plugin) fanOut(ctx context.Context, subjects iter.Seq[subject]) {
	for s := range subjects {
		p.reads.send(ctx, &call{
			key:     resourceKey(kindInstance, s.project, s.instance),
			kind:    kindInstance,
			project: s.project,
			name:    s.instance,
			ev: iutil.NewEvent(time.Now(),
				incusapi.EventLifecycleInstanceUpdated, s.project, s.instance, "").
				WithChainState(p.chain),
		})
	}
}

// patchState patches the state from one instance read and decides what that key is
// owed - the only place either happens.
//
// The second return says whether the read landed; false fails the events
// waiting on it, the same as a read that errored.
func (p *Plugin) patchState(
	ctx context.Context,
	project, name string,
	inst *incusapi.Instance,
	instanceState *incusapi.InstanceState,
	err error,
) (*iutil.Instance, bool) {
	switch {
	case err == nil:

	case incusapi.StatusErrorCheck(err, http.StatusNotFound):
		// Read after it went; the delete is on its way with the same news.
		p.state.deleteInstance(project, name)
		p.retries.done(project, name)
		p.archive.forget(project, name)

		return nil, true

	default:
		p.retries.soon(project, name)

		return nil, false
	}

	i, missing := p.state.setInstance(inst, instanceState)

	// A NIC on a network nothing has read yet: nothing was stored, so read the
	// network, and the re-read below places the NIC once it lands. The re-read
	// is what corrects it either way, since the network's own fan-out only
	// reaches instances the state already holds.
	if i == nil {
		p.readNetwork(ctx, project, missing)
		p.retries.soon(project, name)

		return nil, false
	}

	// No lease yet, or a state read that could not be made.
	if i.Running() && !addressed(i) {
		p.retries.soon(project, name)

		return i, true
	}

	p.retries.done(project, name)

	if p.opts.TTL > 0 {
		p.retries.at(project, name, p.opts.TTL)
	}

	return i, true
}
