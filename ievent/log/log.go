// Package log prints every event that walks past it.
//
// It is the reason drops travel the chain rather than being taken out of it, and
// the one plugin worth listing more than once: in front of debounce it reports
// what Incus sent, behind dns what became of it.
package log

import (
	"context"
	"log/slog"
	"time"

	"github.com/lxc/incus-compose/ievent/iutil"
	"github.com/lxc/incus-compose/shared"
)

// name is what this plugin is called when a position was not named.
const name = "log"

// Config is what this plugin prints and how loudly.
type Config struct {
	// At names this position, so two of these in one chain are told apart:
	// "arrival" in front of everything, "served" behind the last plugin that
	// acts. Empty is the plugin's own name, which is the right answer for a
	// chain with only one of them in it.
	//
	// New qualifies it, so what this holds afterwards is the whole name.
	At string

	// Level is what a routine event is printed at. A failed one is never printed
	// below Warn whatever this says.
	Level slog.Level
}

// Plugin prints events. It has no inbox and no Run, alone among the plugins:
// an observer that buffered would report the chain in an order it did not run in.
type Plugin struct {
	cfg Config

	// ctx is the process lifetime, kept because slog takes one.
	ctx context.Context

	next iutil.Next
}

// Option sets one of them. The zero value means unset, and New fills this
// plugin's own default in.
type Option func(*Config)

// At names this position by where it sits.
func At(at string) Option { return func(cfg *Config) { cfg.At = at } }

// Level sets what a routine event is printed at, Debug by default - so a chain
// that carries this position costs only the call unless the handler was opened
// up. It is per position, because two logs in one chain are two constructions.
func Level(l string) Option {
	return func(cfg *Config) { cfg.Level = shared.StringToSlogLevel(l) }
}

// New builds a log for one position.
func New(opts ...Option) *Plugin {
	cfg := Config{
		Level: slog.LevelDebug,
	}

	for _, opt := range opts {
		opt(&cfg)
	}

	// Qualified once, here, so everything downstream reads the whole name off
	// the same field.
	if cfg.At != "" {
		cfg.At = name + "/" + cfg.At
	} else {
		cfg.At = name
	}

	slog.Info("Starting", "plugin", cfg.At, "config", cfg)

	return &Plugin{cfg: cfg}
}

// Name identifies the position rather than the plugin, because two of these are
// two positions.
func (p *Plugin) Name() string { return p.cfg.At }

// Wants nothing of its own: it is in the chain, so it sees whatever walks.
func (p *Plugin) Wants() []iutil.Want { return nil }

// Setup keeps the successor and the context, and starts nothing.
func (p *Plugin) Setup(args iutil.SetupArgs) error {
	p.ctx, p.next = args.Context, args.Next

	return nil
}

// Handle prints the event and hands it straight on, on the caller's goroutine.
// It never guards on State: seeing dropped and failed events is the point of it.
func (p *Plugin) Handle(ev *iutil.Event) {
	level := p.cfg.Level
	if ev.State() == iutil.StateFailed {
		level = max(level, slog.LevelWarn)
	}

	attrs := []any{
		"action", ev.Action(),
		"state", string(ev.State()),
	}

	if p.cfg.At != name {
		attrs = append(attrs, "at", p.cfg.At)
	}

	if ev.Project() != "" {
		attrs = append(attrs, "project", ev.Project())
	}

	if ev.Name() != "" {
		attrs = append(attrs, "name", ev.Name())
	}

	if ev.Enriched(iutil.EnrichedInstance) {
		attrs = append(attrs, "running", ev.Running())
	}

	if ev.OldName() != "" {
		attrs = append(attrs, "old", ev.OldName())
	}

	if ev.Reason() != "" {
		attrs = append(attrs, "reason", ev.Reason())
	}

	// Which reads landed, never what they found.
	if ev.Enrichments() != 0 {
		attrs = append(attrs, "enriched", ev.Enrichments())
	}

	// Since the source decoded it, so two of these bracket the walk and the
	// difference is what it cost.
	attrs = append(attrs, "age", time.Since(ev.At()).Round(time.Microsecond))

	slog.Log(p.ctx, level, "event", attrs...)

	p.next(ev)
}
