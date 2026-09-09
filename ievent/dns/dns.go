// Package dns folds events into records and answers queries from them. It owns
// everything it serves, so main knows nothing about DNS.
package dns

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"slices"
	"sync"
	"time"

	"github.com/coredns/coredns/plugin"
	incusapi "github.com/lxc/incus/v7/shared/api"

	"github.com/lxc/incus-compose/ievent/dns/ecs_view"
	"github.com/lxc/incus-compose/ievent/iutil"
)

// name is what this plugin is called, in the chain and in a drop's reason.
const name = "dns"

// defaultInboxSize is what the inbox holds before a drop is the only answer.
const defaultInboxSize = 1024

// publishWindow is how long patches collect before one snapshot is published.
// Trailing edge only, and invisible against a TTL measured in seconds.
const publishWindow = 150 * time.Millisecond

// Config is what this plugin serves and where.
type Config struct {
	// Listen is the address to answer on, UDP and TCP both.
	Listen string

	// Chain is the query chain that answers after the engine, in query order;
	// empty refuses what it does not serve. plugin.Plugin, since CoreDNS wires a
	// stack from closures.
	Chain []plugin.Plugin

	// Stop is what main does about anything in Chain with a lifecycle -
	// forward's health checkers, most of all.
	Stop func()

	// EchoSubnet turns on the RFC 7871 reply option.
	EchoSubnet bool

	// Metrics turns on the engine's counters and gauges.
	Metrics bool

	// AllowTransfer is who may ask for a zone transfer. Empty allows nobody, so
	// a transfer is opt-in at the listener as well as at the zone.
	AllowTransfer []netip.Prefix

	// ColdDir is where the cold store lives, the same directory the certificate
	// is in. Empty disables it, and nothing writes to disk without it.
	ColdDir string

	// ColdSeed is what a previous run published, kept in memory instead of under
	// a directory. Set by ColdMemory, and it wins over ColdDir.
	ColdSeed []byte

	// ColdMemory keeps the store in memory. Its own field because a nil seed is
	// a memory store that starts cold, not the absence of one.
	ColdMemory bool

	// TTL is what a record is rendered with. Short, because a fleet moves and a
	// cached address is one that may have been reassigned.
	TTL uint32

	// Suffix is what a project's zone is built under, unless the project names
	// its own with user.label.dns.zone.
	Suffix string

	// InboxSize defines the backbuffer size.
	InboxSize int

	// Project decides which projects this binary serves, the same predicate the
	// enricher gets. Nil serves every project the certificate can see.
	Project func(p *incusapi.Project) bool
}

// Plugin folds events into a fleet-wide snapshot, publishes it, and answers
// from it. Run's goroutine writes one atomic pointer; every query goroutine reads it.
type Plugin struct {
	cfg    Config
	logger *slog.Logger

	actions []string

	next  iutil.Next
	inbox chan *iutil.Event

	// commandOut puts an event in at the head of the chain, which is how readiness
	// reaches every position rather than only what comes after this one.
	commandOut chan<- iutil.Command

	// commandIn is the source asking this plugin to finish, on a channel of its own so
	// it arrives whatever the inbox looks like.
	commandIn <-chan iutil.Command

	// view is the engine. It sources nothing and derives nothing: what it holds
	// is whatever was last handed to it here.
	view *ecs_view.ECSView

	// xfr answers transfers, from a snapshot published to it separately. A
	// transfer derives what the query path is built not to, so it reads its own
	// pointer rather than the engine's.
	xfr *xfr

	// ready is what was last announced, so a fold that changes nothing puts no
	// further event on the chain. Run's goroutine alone.
	ready bool

	// cold is what was served last, on disk. This goroutine encodes; the
	// store's own goroutine writes, so a slow disk cannot stall the chain.
	cold *coldStore

	// state is everything folding owns - what is held, what has arrived, and the
	// trees being patched. Run's goroutine alone, never Handle's caller.
	state *state
}

var _ iutil.Plugin = (*Plugin)(nil)

// Option sets one of them. The zero value means unset, and New fills this
// plugin's own default in.
type Option func(*Config)

// InboxSize sets how many events this plugin buffers before it has to drop.
func InboxSize(n int) Option { return func(cfg *Config) { cfg.InboxSize = n } }

// Listen sets the address to answer on, UDP and TCP both. Empty answers no DNS
// at all, which is what a build that only folds asks for.
func Listen(addr string) Option { return func(cfg *Config) { cfg.Listen = addr } }

// Chain sets the query chain that answers after the engine, in the order a
// query travels it. Empty refuses what the engine does not serve.
func Chain(chain []plugin.Plugin) Option { return func(cfg *Config) { cfg.Chain = chain } }

// Stop sets what to do about anything in Chain with a lifecycle of its own.
func Stop(fn func()) Option { return func(cfg *Config) { cfg.Stop = fn } }

// EchoSubnet turns the RFC 7871 reply option on.
func EchoSubnet(v bool) Option { return func(cfg *Config) { cfg.EchoSubnet = v } }

// AllowTransfer sets who may ask for a zone transfer. Empty allows nobody.
func AllowTransfer(prefixes []netip.Prefix) Option {
	return func(cfg *Config) { cfg.AllowTransfer = prefixes }
}

// Metrics turns the engine's counters and gauges on.
func Metrics(v bool) Option { return func(cfg *Config) { cfg.Metrics = v } }

// ColdDir sets where the cold store lives. Empty writes nothing to disk.
func ColdDir(dir string) Option { return func(cfg *Config) { cfg.ColdDir = dir } }

// ColdMemory keeps the cold store in memory, seeded with what a previous run
// published. Nil starts cold, which is a first start.
func ColdMemory(seed []byte) Option {
	return func(cfg *Config) { cfg.ColdSeed, cfg.ColdMemory = seed, true }
}

// TTL sets what a record is rendered with.
func TTL(ttl uint32) Option { return func(cfg *Config) { cfg.TTL = ttl } }

// Suffix sets what a project's zone is built under, unless the project names
// its own.
func Suffix(suffix string) Option { return func(cfg *Config) { cfg.Suffix = suffix } }

// Project sets which projects this binary serves. Nil serves every one the
// certificate can see, which is the standalone default.
func Project(fn func(p *incusapi.Project) bool) Option {
	return func(cfg *Config) { cfg.Project = fn }
}

// New builds the DNS plugin and starts nothing: Run owns every goroutine,
// listener and cold store included.
func New(logger *slog.Logger, opts ...Option) *Plugin {
	cfg := Config{
		InboxSize: defaultInboxSize,
	}

	for _, opt := range opts {
		opt(&cfg)
	}

	logger.Info("Starting", "plugin", name, "config", cfg)

	view := ecs_view.New(logger)
	view.EchoSubnet = cfg.EchoSubnet
	view.Metrics = cfg.Metrics
	view.Server = cfg.Listen

	p := &Plugin{
		cfg:    cfg,
		logger: logger,
		view:   view,
		xfr:    newXFR(logger, cfg.AllowTransfer),
		cold:   newCold(logger, cfg),
		state:  newState(map[string]zoneSerial{}),
		inbox:  make(chan *iutil.Event, cfg.InboxSize),
	}

	wants := p.Wants()
	knownActions := make([]string, len(wants))
	for i, w := range wants {
		knownActions[i] = w.Action
	}

	p.actions = knownActions
	return p
}

// newCold is where this plugin keeps what it published: memory where asked for,
// otherwise the data directory.
func newCold(logger *slog.Logger, cfg Config) *coldStore {
	if cfg.ColdMemory {
		return newMemoryStore(cfg.ColdSeed)
	}

	return newColdStore(logger, cfg.ColdDir)
}

// Name identifies the plugin, and names it in the reason of what it drops.
func (p *Plugin) Name() string { return name }

// Addr is where this plugin answers, for a main that wants to report it.
func (p *Plugin) Addr() string { return p.cfg.Listen }

const wantsInstance = iutil.EnrichedInstance |
	iutil.EnrichedInstanceWithInterfaces |
	iutil.EnrichedProject

// Wants the instance and its networks on anything that moves it, plus what its
// project sets, since a name is built from them. A delete needs no read: the
// name is in the event.
func (p *Plugin) Wants() []iutil.Want {
	// Debounce only where the action repeats. A start or a delete happens once,
	// so collapsing it buys nothing and costs the whole window in latency.
	return []iutil.Want{
		{Action: incusapi.EventLifecycleInstanceStarted, Enrich: wantsInstance},
		{Action: incusapi.EventLifecycleInstanceRestarted, Enrich: wantsInstance},
		{Action: incusapi.EventLifecycleInstanceResumed, Enrich: wantsInstance},
		{Action: incusapi.EventLifecycleInstanceStopped, Enrich: wantsInstance},
		{Action: incusapi.EventLifecycleInstanceShutdown, Enrich: wantsInstance},
		{Action: incusapi.EventLifecycleInstanceUpdated, Enrich: wantsInstance, Debounce: true},
		{Action: incusapi.EventLifecycleInstanceDeleted},
		{Action: incusapi.EventLifecycleInstanceRenamed, Enrich: wantsInstance},
		{Action: incusapi.EventLifecycleNetworkUpdated, Enrich: iutil.EnrichedNetwork, Debounce: true},
	}
}

// Setup keeps the successor, the two command doors and the connection - what
// this plugin reads for itself is only which instances still exist.
func (p *Plugin) Setup(args iutil.SetupArgs) error {
	p.next = args.Next
	p.commandIn, p.commandOut = args.CommandIn, args.CommandOut

	return nil
}

// Handle puts the event on the inbox and returns. It runs on the previous
// plugin's goroutine, so a full inbox is a marked drop rather than a wait.
func (p *Plugin) Handle(ev *iutil.Event) {
	select {
	case p.inbox <- ev:
	default:
		p.next(ev.WithDropped(name))
	}
}

// Run folds events and answers queries until told to finish. The listener
// outlives the folding: answering from the last snapshot beats refusing.
func (p *Plugin) Run(ctx context.Context) error {
	wire(p.xfr, p.view, p.cfg.Chain)

	if p.cfg.Stop != nil {
		defer p.cfg.Stop()
	}

	// What each zone was published under, so a serial never goes backwards. The
	// records themselves are replayed onto this plugin by the enricher.
	p.state = newState(p.cold.load())

	loopCtx, stopLoop := context.WithCancelCause(ctx)

	// Not ctx, because this is the part that has to outlive the fold loop.
	serveCtx, stopServing := context.WithCancel(context.WithoutCancel(loopCtx))

	var wg sync.WaitGroup

	// Unwinding the other way round: close the store, then stop the listener and
	// wait for both - closing last would drop the last encoding.
	defer wg.Wait()
	defer stopServing()
	defer p.cold.close()
	defer stopLoop(errors.New("end"))

	if p.cold.enabled() {
		wg.Go(p.cold.run)
	}

	// An empty address answers no DNS at all, which is what a build that only
	// folds asks for.
	if p.cfg.Listen != "" {
		wg.Go(func() {
			err := serveDNS(serveCtx, p.cfg.Listen, &adapter{logger: p.logger, chain: p.xfr})
			if err != nil {
				stopLoop(fmt.Errorf("answering on %s: %w", p.cfg.Listen, err))
			}
		})
	}

	// The publish window, armed by the first patch of a burst and never reset by
	// the ones after it: a leading edge would step the serial twice per burst.
	var window <-chan time.Time

	for {
		select {
		case <-loopCtx.Done():
			// An abort, not a shutdown: whatever arrived goes nowhere, and the
			// transactions it went into are never committed.
			return nil

		case cmd := <-p.commandIn:
			// Everything already on the inbox. Nothing is still feeding it - the
			// source stopped, and the enricher answered its own drain first.
		drained:
			for {
				select {
				case ev := <-p.inbox:
					p.fold(ev)
				default:
					break drained
				}
			}

			// Before the answer: what the drain collected reaches a snapshot
			// here or nowhere, and nothing would say it was lost.
			p.write()

			// Answered last, so the plugin after this one is asked only once
			// everything held here has been pushed into it.
			select {
			case p.commandOut <- cmd:
			case <-loopCtx.Done():
			}

			return nil

		case <-window:
			window = nil

			p.write()

		case ev := <-p.inbox:
			p.fold(ev)

			if window == nil && len(p.state.pending) > 0 {
				window = time.After(publishWindow)
			}
		}
	}
}

// fold applies one event to what is held, and hands it on. Everything it
// touches belongs to Run's goroutine.
func (p *Plugin) fold(ev *iutil.Event) {
	// Unwinding the other way round: hand the event on, then say what it
	// changed, so the cause reaches the chain ahead of its consequence.
	defer func() {
		now := p.view.Ready()
		if now == p.ready {
			return
		}

		p.ready = now

		// Ours to name, since nothing else raises or reads it.
		action := "dns/not-ready"
		if now {
			action = "dns/ready"
		}

		// Non-blocking: the source may already have stopped reading, and waiting
		// would hold up the drain this runs inside.
		select {
		case p.commandOut <- iutil.Command{Action: action}:
		default:
		}
	}()

	defer p.next(ev)

	if ev.ChainState() != p.state.chain {
		p.state.chain = ev.ChainState()
	}

	// A lost stream drops no record, but nothing is confirming them any more,
	// which is what unready says.
	if ev.Action() == iutil.ActionDisconnected {
		p.view.SetHealthy(false)

		return
	}

	if ev.Action() == iutil.ActionSweepEnd {
		p.rounded()

		return
	}

	if ev.Err() != nil || !slices.Contains(p.actions, ev.Action()) {
		return
	}

	// Everything from here keys on the name. Without this, an action carrying
	// none folds into an entry called "" and reaches the cold store.
	if ev.Name() == "" {
		return
	}

	key := heldKey(ev.ProjectName(), ev.Name())

	// A rename is a new name and an old one gone. The event carries both, and
	// the enricher already read the instance under its new name.
	if ev.OldName() != "" {
		p.state.pending[heldKey(ev.ProjectName(), ev.OldName())] = nil
	}

	if ev.Action() == incusapi.EventLifecycleInstanceDeleted {
		p.state.pending[key] = nil

		return
	}

	if !p.serveable(ev) {
		return
	}

	inst := patchInstance(ev, p.cfg.Suffix)
	if inst == nil {
		return
	}

	p.state.pending[key] = inst
}

// rounded takes the end of a round. Healthy is set here and nowhere else: a
// replay off disk is structurally whole but not fresh, and has to serve clamped
// until Incus itself has been read.
//
// The window is flushed rather than published beside, or the end of a round
// publishes twice.
func (p *Plugin) rounded() {
	p.state.write(p.cfg.TTL)
	p.publish()

	p.view.SetHealthy(true)
}

// write applies what has arrived and publishes it. Nothing arrived is nothing
// to publish: the window fires once per burst whether or not the burst changed
// anything.
func (p *Plugin) write() {
	if !p.state.write(p.cfg.TTL) {
		return
	}

	p.publish()
}

// publish hands the working copy over as current, and stores what it was
// published under.
func (p *Plugin) publish() {
	p.state.step(p.state.chain == iutil.ChainWarm, p.cfg.TTL)

	// Written twice, one pointer each: neither reads the other's, and the value
	// itself is finished and shared.
	snap := p.state.snapshot()

	p.view.Replace(snap)
	p.xfr.Replace(snap)

	// Encoded on this goroutine, so what crosses to the writer is a finished
	// []byte and it cannot reach into live state.
	b, err := encodeCold(p.state.serials)
	if err != nil {
		p.logger.Warn("encoding the cold store", "err", err)

		return
	}

	p.cold.store(b)
}

// serveable reports whether this read is one to fold. Up or down is not this
// plugin's business; addresses are. A running instance with no address raced
// DHCP and is left as it was, while one that is not running has lost them, and
// that loss is the answer.
func (p *Plugin) serveable(ev *iutil.Event) bool {
	if !ev.Enriched(iutil.EnrichedInstanceWithInterfaces) {
		return false
	}

	inst := ev.Instance()
	for iface := range inst.Interfaces() {
		if len(iface.IPv4()) > 0 || len(iface.IPv6()) > 0 {
			return true
		}
	}

	return !inst.Running()
}

// _ pins the interface here, so a change to it fails the build at the plugin
// rather than at the source that walks it.
var _ iutil.Plugin = (*Plugin)(nil)
