// Package source turns an Incus connection into a stream of events and hands
// each one to a chain of plugins.
//
// It only ever reads, and what is left of that is one read: the stream. The
// enrichment and the sweep belong to the enricher, at a position in the chain.
package source

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	incusapi "github.com/lxc/incus/v7/shared/api"

	"github.com/lxc/incus-compose/iclient"
	"github.com/lxc/incus-compose/ievent/iutil"
)

// Timings for the reconnect loop.
const (
	// minBackoff and maxBackoff bound the gap between one session and the next.
	minBackoff = time.Second
	maxBackoff = time.Minute
)

// commandBuffer is how many plugin-minted actions the line holds. Small on
// purpose: Command blocks rather than drops, and what it waits on is a goroutine
// that only ever enqueues.
const commandBuffer = 8

// errNoConnection reports a source built without one, from Run rather than New
// because that is where the listener is built.
var errNoConnection = errors.New("no Incus connection")

// Source reads the Incus event stream and walks every event through the
// plugins, in the order it was given them.
//
// It is main's object rather than a plugin. Every field belongs to the goroutine
// running Run, except the channels, which are how the plugins and Run meet.
type Source struct {
	conn *iclient.Connection

	// head is the first plugin; the rest hangs off it.
	head iutil.Next

	// wants is the union of every plugin's Wants, keyed by action. An action
	// absent from it is never walked.
	wants map[string]iutil.Want

	// listen opens one event stream. A field so a test can hand events over
	// without a daemon.
	listen listenFunc

	// plugins is the chain in the order events travel it, which is the order
	// Drain asks them to finish in.
	plugins []plugged

	// raised is every plugin's CommandOut, folded into one. What arrives here
	// goes in at the head, so it reaches every position and in order.
	raised chan iutil.Command
}

// plugged is one plugin and the door the source asks it questions through, per
// plugin because a question has to reach one whose event inbox is full.
type plugged struct {
	plugin iutil.Plugin

	// in is this plugin's CommandIn. The source writes, the plugin reads.
	in chan iutil.Command

	// done closes when the plugin's Run returns, so the source stops waiting
	// for an answer that is not coming.
	done chan struct{}
}

// listenFunc opens one event stream. Canceling the context closes the socket.
type listenFunc func(ctx context.Context) (<-chan incusapi.Event, error)

// New builds a source over a chain that main lists in order, wiring it
// backwards so main writes it forwards. An error from any Setup stops the
// process.
func New(ctx context.Context, conn *iclient.Connection, plugins []iutil.Plugin) (*Source, error) {
	s := &Source{
		conn:   conn,
		wants:  map[string]iutil.Want{},
		raised: make(chan iutil.Command, commandBuffer),
	}

	// Wants first and from every plugin, because the enricher serves the whole
	// chain from one action and needs the finished union.
	//
	// A value listed twice would have Setup called twice and the second successor
	// overwrite the first, so it is refused before anything is wired.
	seen := map[iutil.Plugin]bool{}

	for _, p := range plugins {
		if seen[p] {
			return nil, fmt.Errorf("plugin %s is listed twice; two positions need two constructions", p.Name())
		}

		seen[p] = true

		// One entry per action. The two fields fold opposite ways and both
		// toward doing more work; the first Want is taken as it stands, there
		// being no identity value to start a Debounce fold from.
		for _, w := range p.Wants() {
			was, ok := s.wants[w.Action]
			if !ok {
				s.wants[w.Action] = w

				continue
			}

			was.Enrich |= w.Enrich
			was.Debounce = was.Debounce && w.Debounce
			s.wants[w.Action] = was
		}
	}

	args := iutil.SetupArgs{
		Context:    ctx,
		Conn:       conn,
		CommandOut: s.raised,
		Wanted:     s.wants,

		// The end of the chain does nothing: it is a call stack, so it unwinds
		// back to here by itself.
		Next: func(_ *iutil.Event) {},
	}

	// Backwards, because a plugin needs the one after it. The finished table is
	// iutil rather than cloned per plugin.
	s.plugins = make([]plugged, len(plugins))

	for i := len(plugins) - 1; i >= 0; i-- {
		p := plugins[i]

		// Unbuffered: a slot would let the source ask a plugin that is not
		// listening and believe it had been heard.
		in := make(chan iutil.Command)

		args.CommandIn = in

		err := p.Setup(args)
		if err != nil {
			return nil, fmt.Errorf("setting up %s: %w", p.Name(), err)
		}

		pl := plugged{plugin: p, in: in, done: make(chan struct{})}

		// A plugin with no goroutine is finished before it starts, decided by
		// what it is rather than by what main remembers to say.
		_, runs := p.(interface{ Run(context.Context) error })
		if !runs {
			close(pl.done)
		}

		s.plugins[i] = pl

		args.Next = p.Handle
	}

	s.head = args.Next

	return s, nil
}

// Run reads the event stream and hands every event to the chain until ctx is
// canceled. It blocks, so main owns the goroutine, and it is called once.
//
// A daemon that is not there is not a failure: it keeps opening listeners on a
// backoff. Returning says the stream is closed and says nothing about the
// plugins, which is why main drains those only afterwards.
func (s *Source) Run(ctx context.Context) error {
	if s.listen == nil {
		if s.conn == nil {
			return errNoConnection
		}

		s.listen = incusListener(s.conn)
	}

	backoff := minBackoff

	for {
		opened := s.session(ctx)

		if stopping(ctx) {
			return nil
		}

		// Reset on a session that opened a listener, never on one that merely
		// tried: Incus accepts the TLS connection of a certificate it does not
		// trust and refuses only the stream.
		if opened {
			backoff = minBackoff
		}

		s.wait(ctx, backoff)

		if stopping(ctx) {
			return nil
		}

		if !opened {
			backoff = min(backoff*2, maxBackoff)
		}
	}
}

// stopping reports whether the source has been told to stop. A canceled context
// is how this ends rather than something it failed at.
func stopping(ctx context.Context) bool {
	select {
	case <-ctx.Done():
		return true
	default:
		return false
	}
}

// session runs one listener from open to close, and reports whether it opened at
// all. Canceling the context it was opened with is what closes the socket, so it
// gets one of this session's.
func (s *Source) session(ctx context.Context) bool {
	sessionCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	events, err := s.listen(sessionCtx)
	if err != nil {
		slog.Warn("opening the Incus event listener", "err", err)

		return false
	}

	// Connected once the listener is open, which is when Incus has accepted us.
	// The enricher reads a whole fleet off the back of it.
	s.hand(iutil.NewEvent(time.Now(), iutil.ActionConnected, "", "", ""))

	// Paired on every way out, including a canceled context.
	defer func() {
		s.hand(iutil.NewEvent(time.Now(), iutil.ActionDisconnected, "", "", ""))
	}()

	for {
		select {
		case <-ctx.Done():
			return true

		case cmd := <-s.raised:
			s.hand(iutil.NewEvent(time.Now(), cmd.Action, "", "", ""))

		case raw, ok := <-events:
			if !ok {
				slog.Warn("the Incus event stream closed")

				return true
			}

			s.route(raw)
		}
	}
}

// wait holds for d, still handing commands over: the line has to be served
// between sessions as well as inside one, and a reconnect is exactly when a
// pass fails and raises something.
func (s *Source) wait(ctx context.Context, d time.Duration) {
	timer := time.NewTimer(d)
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			return

		case <-timer.C:
			return

		case cmd := <-s.raised:
			s.hand(iutil.NewEvent(time.Now(), cmd.Action, "", "", ""))
		}
	}
}

// route decodes one raw event and hands it over, unless nothing asked for it.
func (s *Source) route(raw incusapi.Event) {
	ev, err := decodeLifecycle(raw)
	if err != nil {
		if !errors.Is(err, errIgnored) {
			slog.Debug("decoding lifecycle event", "err", err)
		}

		return
	}

	// An action nobody declared never walks, which is most of what a lifecycle
	// stream carries.
	_, wanted := s.wants[ev.Action()]
	if !wanted {
		return
	}

	s.hand(ev)
}

// hand gives one event to the head of the chain. The source does not walk: each
// plugin holds its successor and calls it.
func (s *Source) hand(ev *iutil.Event) {
	s.head(ev)
}

// Finished says a plugin's Run has returned, so the source stops waiting for
// answers it will never get. main calls it, having started the goroutine.
func (s *Source) Finished(p iutil.Plugin) {
	for _, pl := range s.plugins {
		if pl.plugin != p {
			continue
		}

		close(pl.done)

		return
	}
}

// Drain asks every plugin to finish, in the order events travel the chain, and
// returns when the last of them has.
//
// One at a time, so a plugin is asked only after the one feeding it has
// answered. Called after Run has returned, and bounded by ctx.
func (s *Source) Drain(ctx context.Context) {
	for _, pl := range s.plugins {
		select {
		case pl.in <- iutil.Command{Action: iutil.CommandDrain}:
		case <-pl.done:
			continue
		case <-ctx.Done():
			return
		}

		// The answer is the same action back. Anything else is raised on the way
		// out and too late to carry, so it is read and dropped rather than left
		// to block whoever sent it.
		answered := false

		for !answered {
			select {
			case cmd := <-s.raised:
				answered = cmd.Action == iutil.CommandDrain

			case <-pl.done:
				answered = true

			case <-ctx.Done():
				return
			}
		}
	}
}

// incusListener opens the stream through the connection: every project the
// certificate can see, in one listener. Which of them are served is the
// enricher's, on what it reads.
func incusListener(conn *iclient.Connection) listenFunc {
	return func(ctx context.Context) (<-chan incusapi.Event, error) {
		return conn.ListenEvents(ctx, []string{incusapi.EventTypeLifecycle}, true)
	}
}
