// Package dns is the DNS half of the chain: it folds events into records and
// answers queries from them.
//
// It owns everything it serves - the ecs_view engine, the miekg/dns listener
// Run starts, and the forwarder behind them - so main knows nothing about DNS.
package dns

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"sync"

	"github.com/coredns/coredns/plugin"
	incusapi "github.com/lxc/incus/v7/shared/api"

	"github.com/lxc/incus-compose/ievent/dns/ecs_view"
	"github.com/lxc/incus-compose/ievent/iutil"
)

// name is what this plugin is called, in the chain and in a drop's reason.
const name = "dns"

// defaultInboxSize is what the inbox absorbs before a drop is the only answer.
const defaultInboxSize = 1024

// Config is what this plugin serves and where, its own type because none of it
// is the machinery an Option carries.
type Config struct {
	// Listen is the address to answer on, UDP and TCP both.
	Listen string

	// Behind is the query chain behind the engine, in the order a query travels
	// it; empty refuses what the engine does not serve. The engine is not in it.
	//
	// plugin.Plugin rather than plugin.Handler, because that is what CoreDNS
	// wires its own stack from: a closure handed its successor.
	Behind []plugin.Plugin

	// Stop is what main does about anything in Behind with a lifecycle -
	// forward's health checkers, most of all.
	Stop func()

	// EchoSubnet turns on the RFC 7871 reply option.
	EchoSubnet bool

	// Metrics turns on the engine's counters and gauges.
	Metrics bool

	// ColdDir is where the cold store lives, the same directory the certificate
	// is in. Empty disables it, and nothing writes to disk without it.
	ColdDir string

	// TTL is what a record is rendered with. Short, because a fleet moves and a
	// cached address is one that may have been reassigned.
	TTL uint32

	// Suffix is what a project's zone is built under, unless the project names
	// its own with user.label.coredns.zone.
	Suffix string

	// InboxSize defines the backbuffer size.
	InboxSize int
}

// Plugin folds events into a fleet-wide snapshot, publishes it, and answers
// from it.
//
// The two halves meet at one atomic pointer inside the engine: Run's goroutine
// writes, every query goroutine reads. It is why there is no lock here.
type Plugin struct {
	cfg Config

	actions []string

	next  iutil.Next
	inbox chan *iutil.Event

	// out puts an event in at the head of the chain, which is how readiness
	// reaches every position rather than only what is behind this one.
	out chan<- iutil.Command

	// in is the source asking this plugin to finish, on a channel of its own so
	// it arrives whatever the inbox looks like.
	in <-chan iutil.Command

	// view is the engine. It sources nothing and derives nothing: what it holds
	// is whatever was last handed to it here.
	view *ecs_view.ECSView

	// ready is what was last announced, so a fold that changes nothing puts no
	// further event on the chain. Run's goroutine alone.
	ready bool

	// cold is what was served last, on disk. This goroutine encodes; the
	// store's own goroutine writes, so a slow disk cannot stall the chain.
	cold *coldStore

	// serials is what each zone was published under before this process started.
	// One going backwards has every secondary re-transfer.
	serials map[string]uint32

	// held is every instance this plugin serves, keyed by project and name. It
	// belongs to the goroutine Run owns, never to Handle's caller.
	held map[string]*instance

	// published is the last snapshot handed over, kept so a zone's serial can
	// be carried forward when its records did not change.
	published *ecs_view.Snapshot
}

// Option sets one of them. The zero value means unset, and New fills this
// plugin's own default in.
type Option func(*Config)

// InboxSize sets how many events this plugin buffers before it has to drop.
func InboxSize(n int) Option { return func(cfg *Config) { cfg.InboxSize = n } }

// Listen sets the address to answer on, UDP and TCP both. Empty answers no DNS
// at all, which is what a build that only folds asks for.
func Listen(addr string) Option { return func(cfg *Config) { cfg.Listen = addr } }

// Behind sets the query chain behind the engine, in the order a query travels
// it. Empty refuses what the engine does not serve.
func Behind(chain []plugin.Plugin) Option { return func(cfg *Config) { cfg.Behind = chain } }

// Stop sets what to do about anything Behind with a lifecycle of its own.
func Stop(fn func()) Option { return func(cfg *Config) { cfg.Stop = fn } }

// EchoSubnet turns the RFC 7871 reply option on.
func EchoSubnet(v bool) Option { return func(cfg *Config) { cfg.EchoSubnet = v } }

// Metrics turns the engine's counters and gauges on.
func Metrics(v bool) Option { return func(cfg *Config) { cfg.Metrics = v } }

// ColdDir sets where the cold store lives. Empty writes nothing to disk.
func ColdDir(dir string) Option { return func(cfg *Config) { cfg.ColdDir = dir } }

// TTL sets what a record is rendered with.
func TTL(ttl uint32) Option { return func(cfg *Config) { cfg.TTL = ttl } }

// Suffix sets what a project's zone is built under, unless the project names
// its own.
func Suffix(suffix string) Option { return func(cfg *Config) { cfg.Suffix = suffix } }

// New builds the DNS plugin and starts nothing: Run owns every goroutine,
// listener and cold store included.
func New(opts ...Option) *Plugin {
	cfg := Config{
		InboxSize: defaultInboxSize,
	}

	for _, opt := range opts {
		opt(&cfg)
	}

	slog.Info("Starting", "plugin", name, "config", cfg)

	view := ecs_view.New()
	view.EchoSubnet = cfg.EchoSubnet
	view.Metrics = cfg.Metrics
	view.Server = cfg.Listen

	p := &Plugin{
		cfg:   cfg,
		view:  view,
		cold:  newColdStore(cfg.ColdDir),
		held:  map[string]*instance{},
		inbox: make(chan *iutil.Event, cfg.InboxSize),
	}

	wants := p.Wants()
	knownActions := make([]string, len(wants))
	for i, w := range wants {
		knownActions[i] = w.Action
	}

	p.actions = knownActions
	return p
}

func (p *Plugin) Name() string { return name }

// Addr is where this plugin answers, for a main that wants to report it.
func (p *Plugin) Addr() string { return p.cfg.Listen }

// Wants the instance and its networks on anything that moves an instance, plus
// the project labels where a name is built from them. A delete needs no read:
// the name is in the event.
func (p *Plugin) Wants() []iutil.Want {
	// Debounce only where the action repeats. A start or a delete happens once,
	// so collapsing it buys nothing and costs the whole window in latency.
	return []iutil.Want{
		{Action: incusapi.EventLifecycleInstanceStarted, Enrich: iutil.EnrichedInstance | iutil.EnrichedNetworks | iutil.EnrichedProject},
		{Action: incusapi.EventLifecycleInstanceStopped, Enrich: iutil.EnrichedInstance | iutil.EnrichedNetworks | iutil.EnrichedProject},
		{Action: incusapi.EventLifecycleInstanceUpdated, Enrich: iutil.EnrichedInstance | iutil.EnrichedNetworks | iutil.EnrichedProject, Debounce: true},
		{Action: incusapi.EventLifecycleInstanceDeleted},
		// No Debounce: collapsing two renames keeps the last OldName and loses
		// the middle name, so the record under it would never be dropped.
		{Action: incusapi.EventLifecycleInstanceRenamed, Enrich: iutil.EnrichedInstance | iutil.EnrichedNetworks},
		{Action: incusapi.EventLifecycleNetworkUpdated, Enrich: iutil.EnrichedNetworks, Debounce: true},
	}
}

// Setup keeps the successor and the two command doors, and not the connection:
// everything this plugin knows about Incus arrives as an event.
func (p *Plugin) Setup(args iutil.SetupArgs) error {
	p.next = args.Next
	p.in, p.out = args.CommandIn, args.CommandOut

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
	wire(p.view, p.cfg.Behind)

	if p.cfg.Stop != nil {
		defer p.cfg.Stop()
	}

	// What was served last, before anything is answered from it.
	held, serials := p.cold.load()

	p.serials = serials

	maps.Copy(p.held, held)

	// Not ctx, because this is the part that has to outlive the fold loop.
	serveCtx, stopServing := context.WithCancel(context.WithoutCancel(ctx))

	var wg sync.WaitGroup

	errs := make(chan error, 1)

	// Unwinding the other way round: close the store, stop the listener, wait
	// for both. Closing last would drop the encoding holding the serials.
	defer wg.Wait()
	defer stopServing()
	defer p.cold.close()

	if p.cold.enabled() {
		wg.Go(p.cold.run)
	}

	// An empty address answers no DNS at all, which is what a build that only
	// folds asks for.
	if p.cfg.Listen != "" {
		wg.Go(func() {
			err := serveDNS(serveCtx, p.cfg.Listen, &adapter{chain: p.view})
			if err != nil {
				errs <- fmt.Errorf("answering on %s: %w", p.cfg.Listen, err)
			}
		})
	}

	for {
		select {
		case <-ctx.Done():
			// An abort, not a shutdown: whatever is held goes nowhere.
			return nil

		case cmd := <-p.in:
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

			// Answered last, so the plugin behind is asked only once everything
			// held here has been pushed into it.
			select {
			case p.out <- cmd:
			case <-ctx.Done():
			}

			return nil

		case err := <-errs:
			return err

		case ev := <-p.inbox:
			p.fold(ev)
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

		action := iutil.ActionNotReady
		if now {
			action = iutil.ActionReady
		}

		// Non-blocking: the source may already have stopped reading, and waiting
		// would hold up the drain this runs inside.
		select {
		case p.out <- iutil.Command{Action: action}:
		default:
		}
	}()

	defer p.next(ev)

	// A lost stream drops no record, but nothing is confirming them any more,
	// which is what unready says.
	if ev.Action() == iutil.ActionDisconnected {
		p.view.SetHealthy(false)

		return
	}

	// The closing bracket of a pass is where what is held is known to be
	// everything there is.
	if ev.Action() == iutil.ActionSweepEnd {
		p.publish()

		return
	}

	if ev.State() != iutil.StateOk || !slices.Contains(p.actions, ev.Action()) {
		return
	}

	// Everything from here keys on the name. Without this, an action carrying
	// none folds into an entry called "" and reaches the cold store.
	if ev.Name() == "" {
		return
	}

	key := heldKey(ev.Project(), ev.Name())

	// What this instance was last read as, which distill needs for the parts of
	// an event that did not arrive - the project's labels, above all.
	prev := p.held[key]

	// A rename is a new name and an old one gone. The event carries both, and
	// the enricher already read the instance under its new name.
	if ev.OldName() != "" {
		old := heldKey(ev.Project(), ev.OldName())

		was, held := p.held[old]
		if held {
			prev = was
		}

		delete(p.held, old)
	}

	if ev.Action() == incusapi.EventLifecycleInstanceDeleted {
		delete(p.held, key)
		return
	}

	if !p.serveable(ev) {
		return
	}

	inst := patchInstance(ev, prev, p.cfg.Suffix)
	if inst == nil {
		return
	}

	p.held[key] = inst
}

// publish renders what is held, hands it to the engine, and stores it.
func (p *Plugin) publish() {
	snap := build(p.held, p.published, p.cfg.TTL)

	// A zone starts no lower than it was last published at, across a restart as
	// well as within a run.
	for name, zone := range snap.ByZone {
		was, ok := p.serials[name]
		if ok && zone.Serial < was {
			zone.Serial = was
		}
	}

	p.published = snap

	p.view.Replace(snap)
	p.view.SetHealthy(true)

	// Encoded on this goroutine, so what crosses to the writer is a finished
	// []byte and it cannot reach into live state.
	b, err := encodeCold(p.held, snap)
	if err != nil {
		slog.Warn("encoding the cold store", "err", err)

		return
	}

	p.cold.store(b)
}

// serveable reports whether this read found an address worth a record.
func (p *Plugin) serveable(ev *iutil.Event) bool {
	if !ev.Enriched(iutil.EnrichedNetworks) {
		return false
	}

	for _, net := range ev.Networks() {
		if len(net.IPv4()) > 0 || len(net.IPv6()) > 0 {
			return true
		}
	}

	return false
}

// _ pins the interface here, so a change to it fails the build at the plugin
// rather than at the source that walks it.
var _ iutil.Plugin = (*Plugin)(nil)
