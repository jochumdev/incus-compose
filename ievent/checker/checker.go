// Package checker runs health checks and restart policies on watched instances,
// folding their lifecycle events into the checks, restarts and status writes
// they drive.
package checker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"time"

	incusApi "github.com/lxc/incus/v7/shared/api"
	"github.com/panjf2000/ants/v2"

	"github.com/lxc/incus-compose/ievent/iutil"
)

// name is what this plugin is called, in the chain and in a drop's reason.
const name = "checker"

// defaultInboxSize is what the inbox holds before a drop is the only answer.
const defaultInboxSize = 1024

// Worker counts over every watched project. Restarts count apart: one holds its
// worker for up to restartTimeout.
const (
	defaultWorkers        = 128
	defaultRestartWorkers = 32
)

// pools cap the Incus actions in flight, over every watched project.
type pools struct {
	check   *ants.Pool
	restart *ants.Pool
}

func newPools(checkWorkers, restartWorkers int) (pools, error) {
	checkPool, err := ants.NewPool(max(checkWorkers, 1), ants.WithNonblocking(true))
	if err != nil {
		return pools{}, fmt.Errorf("creating the check pool: %w", err)
	}

	restartPool, err := ants.NewPool(max(restartWorkers, 1), ants.WithNonblocking(true))
	if err != nil {
		checkPool.Release()
		return pools{}, fmt.Errorf("creating the restart pool: %w", err)
	}

	pools := pools{}
	pools.check = checkPool
	pools.restart = restartPool

	return pools, nil
}

func releasePools(pools pools) {
	pools.check.Release()
	pools.restart.Release()
}

// Config is what this plugin watches and how.
type Config struct {
	// InboxSize is what the inbox holds before a drop is the only answer.
	InboxSize int

	// Serveable decides which events this daemon acts on, built by the
	// binary from its project scope. Nil acts on every project the certificate
	// can see.
	Serveable func(ev *iutil.Event) bool

	Workers        int
	RestartWorkers int
}

// Option sets one of them. The zero value means unset, and New fills this
// plugin's own default in.
type Option func(*Config)

// Serveable sets which events this daemon acts on. Nil acts on every one the
// certificate can see, which is the standalone default.
func Serveable(fn func(ev *iutil.Event) bool) Option {
	return func(cfg *Config) { cfg.Serveable = fn }
}

// Workers sets the number of check workers.
func Workers(v int) Option { return func(cfg *Config) { cfg.Workers = v } }

// RestartWorkers sets the number of restart workers.
func RestartWorkers(v int) Option { return func(cfg *Config) { cfg.RestartWorkers = v } }

// Plugin runs health checks and restart policies on the fleet.
type Plugin struct {
	logger *slog.Logger

	cfg  Config
	args iutil.SetupArgs

	next  iutil.Next
	inbox chan *iutil.Event

	// actions is what Wants named; everything else walks past unfolded.
	actions []string
}

var _ iutil.Plugin = (*Plugin)(nil)

// New builds the checker and starts nothing: Run owns every goroutine.
func New(logger *slog.Logger, opts ...Option) *Plugin {
	cfg := Config{
		InboxSize:      defaultInboxSize,
		Workers:        defaultWorkers,
		RestartWorkers: defaultRestartWorkers,
	}

	for _, opt := range opts {
		opt(&cfg)
	}

	logger.Info("Starting", "plugin", name, "config", cfg)

	p := &Plugin{
		logger: logger,
		cfg:    cfg,
		inbox:  make(chan *iutil.Event, cfg.InboxSize),
	}

	wants := p.Wants()
	p.actions = make([]string, len(wants))
	for i, w := range wants {
		p.actions[i] = w.Action
	}

	return p
}

// Name identifies the plugin, and names it in the reason of what it drops.
func (p *Plugin) Name() string { return name }

const wantsInstance = iutil.EnrichedInstance | iutil.EnrichedProject

// Wants the instance and what its project sets on anything that moves it: the
// instance is what a check reads, the project is what serveable decides by. A
// delete needs no read: the name is in the event.
func (p *Plugin) Wants() []iutil.Want {
	// Debounce only where the action repeats. A start or a delete happens once,
	// so collapsing it buys nothing and costs the whole window in latency.
	return []iutil.Want{
		{Action: incusApi.EventLifecycleInstanceStarted, Enrich: wantsInstance},
		{Action: incusApi.EventLifecycleInstanceRestarted, Enrich: wantsInstance},
		{Action: incusApi.EventLifecycleInstanceResumed, Enrich: wantsInstance},
		{Action: incusApi.EventLifecycleInstanceStopped, Enrich: wantsInstance},
		{Action: incusApi.EventLifecycleInstanceShutdown, Enrich: wantsInstance},
		{Action: incusApi.EventLifecycleInstanceUpdated, Enrich: wantsInstance, Debounce: true},
		{Action: incusApi.EventLifecycleInstanceDeleted},
		{Action: incusApi.EventLifecycleInstanceRenamed, Enrich: wantsInstance},
	}
}

// Setup keeps the successor and the command doors.
func (p *Plugin) Setup(args iutil.SetupArgs) error {
	p.args = args
	p.next = args.Next

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

// serveable reports whether this event is one the daemon acts on. A delete
// goes either way: forgetting what is held is never wrong.
func (p *Plugin) serveable(ev *iutil.Event) bool {
	if ev.Action() == incusApi.EventLifecycleInstanceDeleted || p.cfg.Serveable == nil {
		return true
	}

	return p.cfg.Serveable(ev)
}

// Run folds events and runs the checks and restarts until told to finish.
func (p *Plugin) Run(ctx context.Context) error {
	pools, err := newPools(p.cfg.Workers, p.cfg.RestartWorkers)
	if err != nil {
		return err
	}
	defer releasePools(pools)

	// Dies with Run, so what the handlers start does not outlive the fold loop.
	loopCtx, stopLoop := context.WithCancelCause(ctx)
	defer stopLoop(errors.New("end"))

	instances := map[string]*instance{}
	results := make(chan instanceResult, 32)

	// fold applies one event to what is held, and hands it on. Everything it
	// touches belongs to Run's goroutine.
	fold := func(ev *iutil.Event) {
		if ev.Err() == nil && slices.Contains(p.actions, ev.Action()) {
			if p.serveable(ev) {
				handleInstanceEvent(loopCtx, p.logger, p.args.Conn, instances, results, ev)
			} else {
				k := instKey(ev)

				// The project is not this daemon's to watch: forget the
				// instance the way a delete would.
				if inst, ok := instances[k]; ok {
					p.logger.Debug("Forgetting an instance its project no longer watches",
						"project", ev.ProjectName(), "instance", ev.Name())

					if inst.actionCancel != nil {
						inst.actionCancel()
					}

					delete(instances, k)
				}
			}
		}

		p.next(ev)
	}

	nextCheck := time.NewTimer(time.Hour)
	defer nextCheck.Stop()

	for {
		earliest := runInstanceActions(ctx, p.logger, p.args.Conn, pools, instances, results)
		nextCheck.Stop()
		if earliest.IsZero() {
			nextCheck.Reset(time.Hour)
		} else {
			nextCheck.Reset(time.Until(earliest))
		}

		select {
		case <-loopCtx.Done():
			// An abort, not a shutdown: whatever arrived goes nowhere.
			return nil

		case cmd := <-p.args.CommandIn:
			// Everything already on the inbox. Nothing is still feeding it - the
			// source stopped, and the enricher answered its own drain first.
		drained:
			for {
				select {
				case ev := <-p.inbox:
					fold(ev)
				default:
					break drained
				}
			}

			// Answered last, so the plugin after this one is asked only once
			// everything held here has been pushed into it.
			select {
			case p.args.CommandOut <- cmd:
			case <-loopCtx.Done():
			}

			return nil

		case ev := <-p.inbox:
			fold(ev)

		case res := <-results:
			handleInstanceResult(loopCtx, p.logger, p.args.Conn, instances, results, res)
		case <-nextCheck.C:
			_ = runInstanceActions(ctx, p.logger, p.args.Conn, pools, instances, results)
		}
	}
}
