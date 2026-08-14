// Package debounce collapses a burst of events on one key into two: the leading
// edge goes at once, the trailing one when the key has been quiet for the
// window, and everything between is superseded.
package debounce

import (
	"context"
	"log/slog"
	"time"

	"github.com/lxc/incus-compose/ievent/shared"
)

// name is what this plugin is called, in the chain and in a drop's reason.
const name = "debounce"

// Defaults, used for whatever main left unset.
const (
	// defaultWindow is short enough that a lone change is not noticeably late
	// and long enough that a scripted burst lands inside one.
	defaultWindow = 250 * time.Millisecond
)

// inboxSize is the slack between a burst arriving and this plugin reaching it,
// matched to the Incus client's own event channel.
const defaultInboxSize = 1024

// Plugin holds a window per key, and the event that will close it. Everything
// but the inbox belongs to the goroutine Run owns.
type Plugin struct {
	window time.Duration

	// wanted is the source's finished table, read for Want.Debounce - false
	// unless every plugin that asked for the action can live with the last of a
	// burst.
	wanted map[string]shared.Want

	next  shared.Next
	inbox chan *shared.Event

	// in is the source asking this plugin to finish, on its own channel.
	in <-chan shared.Command

	// out is how the answer goes back.
	out chan<- shared.Command
}

// options is what main decides about this plugin. Its own rather than a set
// shared with every other plugin: naming one is already naming this package.
type options struct {
	Window    time.Duration
	InboxSize int
}

// Option sets one of them. The zero value of each means unset, and New fills
// this plugin's own default in.
type Option func(*options)

// Window sets how long a key must be quiet before the last of its burst goes.
func Window(d time.Duration) Option { return func(o *options) { o.Window = d } }

// InboxSize sets how many events this plugin buffers before it has to drop.
func InboxSize(n int) Option { return func(o *options) { o.InboxSize = n } }

// New builds a debounce whose window closes once a key has been quiet for it.
//
// A burst that never goes quiet never closes its window, so its trailing event
// waits - the leading one already went, and the sweep reconciles the rest.
func New(opts ...Option) *Plugin {
	o := options{
		Window:    defaultWindow,
		InboxSize: defaultInboxSize,
	}

	for _, opt := range opts {
		opt(&o)
	}

	slog.Info("Starting", "plugin", name, "config", o)

	return &Plugin{
		window: o.Window,
		inbox:  make(chan *shared.Event, o.InboxSize),
	}
}

// Name identifies the plugin, and names it in the reason of what it drops.
func (p *Plugin) Name() string { return name }

// Wants nothing: the action and the name it keys on are on the bare event.
func (p *Plugin) Wants() []shared.Want { return nil }

// Setup keeps the successor and the table, and starts nothing: the goroutine is
// the caller's, so main decides where this runs.
func (p *Plugin) Setup(args shared.SetupArgs) error {
	p.next = args.Next
	p.wanted = args.Wanted
	p.in, p.out = args.CommandIn, args.CommandOut

	return nil
}

// Handle puts the event on the inbox and returns. A full inbox is a drop rather
// than a wait, marked and handed straight on so the observers behind still see
// it.
func (p *Plugin) Handle(ev *shared.Event) {
	select {
	case p.inbox <- ev:
	default:
		p.next(ev.WithDropped(name))
	}
}

// Run holds events until told to finish. It blocks, so main owns the goroutine,
// and it returns having handed on everything it holds.
func (p *Plugin) Run(ctx context.Context) error {
	// open is one entry per key with a window running.
	open := map[string]*burst{}

	// sweeping is set between the brackets: a pass reports what is there by what
	// walks, so nothing may be held back while one runs.
	sweeping := false

	for {
		var due <-chan time.Time

		at, ok := earliest(open)
		if ok {
			due = time.After(time.Until(at))
		}

		select {
		case <-ctx.Done():
			// An abort, not a shutdown: whatever is held goes nowhere.
			return nil

		case cmd := <-p.in:
			// The inbox first and through accept, so what arrived last still
			// supersedes what it would have; then every window still open.
			p.drain(open, sweeping)
			p.closeAll(open)

			// Answered only once everything has been handed on: the source asks
			// the next plugin as soon as this answers.
			p.answer(ctx, cmd)

			return nil

		case <-due:
			p.closeExpired(open)

		case ev := <-p.inbox:
			sweeping = p.accept(open, ev, sweeping)
		}
	}
}

// answer sends a command back, and gives up on a context that is already gone.
func (p *Plugin) answer(ctx context.Context, cmd shared.Command) {
	select {
	case p.out <- cmd:
	case <-ctx.Done():
	}
}

// drain takes everything already on the inbox. Nothing is still feeding it, so
// the inbox is finite and this is the whole of it.
func (p *Plugin) drain(open map[string]*burst, sweeping bool) {
	for {
		select {
		case ev := <-p.inbox:
			sweeping = p.accept(open, ev, sweeping)
		default:
			return
		}
	}
}

// accept takes one event off the inbox, and reports whether a sweep is running
// after it.
func (p *Plugin) accept(open map[string]*burst, ev *shared.Event, sweeping bool) bool {
	switch ev.Action() {
	case shared.ActionSweepStart:
		// Every open window closes before the bracket goes, so the pass contains
		// what arrived before it started.
		p.closeAll(open)
		p.next(ev)

		return true

	case shared.ActionSweepEnd:
		p.next(ev)

		return false
	}

	// Nothing to key on, which is where the source's own actions land.
	if ev.Name() == "" {
		p.next(ev)

		return sweeping
	}

	key := ev.Project() + "/" + ev.Name()

	// Only the last of the three is about collapsing: a dropped event is already
	// a report of something that happened, and a pass may not be held back.
	collapse := ev.State() == shared.StateOk &&
		!sweeping &&
		p.wanted[ev.Action()].Debounce

	if !collapse {
		// Whatever this key holds arrived first, so it goes first: handing this
		// one on while an older waits would put them through backwards.
		p.close(open, key)
		p.next(ev)

		return sweeping
	}

	b, ok := open[key]
	if !ok {
		// Leading edge: nothing is in flight for this key, so this goes at once
		// and opens the window that collects whatever follows.
		p.next(ev)

		open[key] = &burst{at: time.Now().Add(p.window)}

		return sweeping
	}

	// Inside an open window. The first event here has nothing to supersede.
	if b.ev != nil {
		p.next(b.ev.WithDropped(name))
	}

	b.ev = ev
	b.at = time.Now().Add(p.window)

	return sweeping
}

// closeExpired closes every window whose key has been quiet long enough.
func (p *Plugin) closeExpired(open map[string]*burst) {
	now := time.Now()

	for key, b := range open {
		if b.at.After(now) {
			continue
		}

		p.close(open, key)
	}
}

// closeAll closes every open window, whatever its deadline.
func (p *Plugin) closeAll(open map[string]*burst) {
	for key := range open {
		p.close(open, key)
	}
}

// close ends one key's window, handing on the trailing event if there is one.
// A burst of one has none: the leading edge already carried it.
func (p *Plugin) close(open map[string]*burst, key string) {
	b, ok := open[key]
	if !ok {
		return
	}

	delete(open, key)

	if b.ev == nil {
		return
	}

	p.next(b.ev)
}

// burst is one key's open window. ev is nil while nothing has followed the
// leading edge, which is what tells a burst of one from a burst of many.
type burst struct {
	ev *shared.Event
	at time.Time
}

// earliest is the nearest deadline open, and whether there is one at all.
func earliest(open map[string]*burst) (time.Time, bool) {
	var out time.Time

	for _, b := range open {
		if out.IsZero() || b.at.Before(out) {
			out = b.at
		}
	}

	return out, !out.IsZero()
}
