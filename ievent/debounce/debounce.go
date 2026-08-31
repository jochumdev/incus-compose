// Package debounce collapses a burst of events on one key into two: the leading edge at once, the trailing one once quiet.
package debounce

import (
	"context"
	"log/slog"
	"time"

	"github.com/lxc/incus-compose/ievent/iutil"
)

// name is what this plugin is called, in the chain and in a drop's reason.
const name = "debounce"

// Defaults, used for whatever main left unset.
const (
	defaultWindow    = 250 * time.Millisecond
	defaultInboxSize = 1024
)

// Plugin holds a window per key, and the event that will close it. Everything
// but the inbox belongs to the goroutine Run owns.
type Plugin struct {
	window time.Duration

	// wanted is the source's finished table, read for Want.Debounce.
	wanted map[string]iutil.Want

	next  iutil.Next
	inbox chan *iutil.Event

	// in is the source asking this plugin to finish, on its own channel.
	in <-chan iutil.Command

	// out is how the answer goes back.
	out chan<- iutil.Command
}

// options is what main decides about this plugin. Its own rather than a type
// shared in iutil: naming one is already naming this package.
type options struct {
	Window    time.Duration
	InboxSize int
}

// Option sets one of the plugin's options; the zero value means unset.
type Option func(*options)

// Window sets how long a key must be quiet before the last of its burst goes.
func Window(d time.Duration) Option { return func(o *options) { o.Window = d } }

// InboxSize sets how many events this plugin buffers before it has to drop.
func InboxSize(n int) Option { return func(o *options) { o.InboxSize = n } }

// New builds a debounce whose window closes once a key has been quiet for it.
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
		inbox:  make(chan *iutil.Event, o.InboxSize),
	}
}

// Name identifies the plugin, and names it in the reason of what it drops.
func (p *Plugin) Name() string { return name }

// Wants nothing: the action and the name it keys on are on the bare event.
func (p *Plugin) Wants() []iutil.Want { return nil }

// Setup keeps the successor and the table, and starts nothing: the goroutine is
// the caller's, so main decides where this runs.
func (p *Plugin) Setup(args iutil.SetupArgs) error {
	p.next = args.Next
	p.wanted = args.Wanted
	p.in, p.out = args.CommandIn, args.CommandOut

	return nil
}

// Handle puts the event on the inbox and returns. A full inbox drops the event
// rather than blocking.
func (p *Plugin) Handle(ev *iutil.Event) {
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
			// Drain first so anything still arriving supersedes before windows close.
			p.drain(open)
			p.closeAll(open)

			// Answered only once everything is handed on, so the source can chain
			// the next plugin.
			p.answer(ctx, cmd)

			return nil

		case <-due:
			p.closeExpired(open)

		case ev := <-p.inbox:
			p.accept(open, ev)
		}
	}
}

// answer sends a command back, and gives up on a context that is already gone.
func (p *Plugin) answer(ctx context.Context, cmd iutil.Command) {
	select {
	case p.out <- cmd:
	case <-ctx.Done():
	}
}

// drain takes everything already on the inbox. Nothing is still feeding it, so
// the inbox is finite and this is the whole of it.
func (p *Plugin) drain(open map[string]*burst) {
	for {
		select {
		case ev := <-p.inbox:
			p.accept(open, ev)
		default:
			return
		}
	}
}

// accept takes one event off the inbox.
func (p *Plugin) accept(open map[string]*burst, ev *iutil.Event) {
	// Nothing to key on, which is where the source's own actions land.
	if ev.Name() == "" {
		p.next(ev)

		return
	}

	key := ev.ProjectName() + "/" + ev.Name()

	// The error and ChainState are prerequisites; Want.Debounce is the actual choice.
	collapse := ev.Err() == nil &&
		ev.ChainState() == iutil.ChainWarm &&
		p.wanted[ev.Action()].Debounce

	if !collapse {
		// Whatever this key holds arrived first, so it goes first.
		p.close(open, key)
		p.next(ev)

		return
	}

	b, ok := open[key]
	if !ok {
		// Leading edge: nothing in flight for this key, so it goes at once.
		p.next(ev)

		open[key] = &burst{at: time.Now().Add(p.window)}

		return
	}

	// Inside an open window. The first event here has nothing to supersede.
	if b.ev != nil {
		p.next(b.ev.WithDropped(name))
	}

	b.ev = ev
	b.at = time.Now().Add(p.window)
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
	ev *iutil.Event
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
