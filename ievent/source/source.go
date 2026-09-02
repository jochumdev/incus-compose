// Package source turns an Incus connection into a stream of events and hands
// each one to a chain of plugins. It only ever reads.
package source

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	incusapi "github.com/lxc/incus/v7/shared/api"
	"github.com/lxc/incus/v7/shared/logger"

	"github.com/lxc/incus-compose/iclient"
	"github.com/lxc/incus-compose/ievent/iutil"
)

// Timings for the reconnect loop.
const (
	minBackoff = time.Second
	maxBackoff = time.Minute
)

// commandBuffer is small on purpose: Command blocks rather than drops when full.
const commandBuffer = 8

// errNoConnection is reported by Run, not New: that's where the listener is built.
var errNoConnection = errors.New("no Incus connection")

// Source reads the Incus event stream and walks each event through the
// plugin chain, in order.
type Source struct {
	logger *slog.Logger

	conn *iclient.Connection

	// head is the first plugin; the rest hangs off it.
	head iutil.Next

	// wants is the union of every plugin's Wants, keyed by action; absent
	// means never walked.
	wants map[string]iutil.Want

	// listen opens one event stream; a field so tests can substitute one
	// without a daemon.
	listen listenFunc

	// plugins is the chain in event order, which is also the order Drain
	// asks them to finish in.
	plugins []plugged

	// raised is every plugin's CommandOut folded into one; it enters the
	// chain at the head.
	raised chan iutil.Command

	// chain is stamped onto every event; a plugin sets it, nothing here
	// derives or checks it.
	chain iutil.ChainState
}

// plugged is one plugin plus the channel the source asks it questions through.
type plugged struct {
	plugin iutil.Plugin

	// in is this plugin's CommandIn. The source writes, the plugin reads.
	in chan iutil.Command

	// done closes when the plugin's Run returns, so the source stops waiting on it.
	done chan struct{}
}

// listenFunc opens one event stream. Canceling the context closes the socket.
type listenFunc func(ctx context.Context) (<-chan incusapi.Event, error)

// New builds a source over the plugin chain, wiring it backwards so main
// writes it forwards. An error from any Setup stops the process.
func New(logger *slog.Logger, conn *iclient.Connection, plugins []iutil.Plugin) (*Source, error) {
	s := &Source{
		logger: logger,
		conn:   conn,
		wants:  map[string]iutil.Want{},
		raised: make(chan iutil.Command, commandBuffer),
		chain:  iutil.ChainCold,
	}

	// seen catches a plugin listed twice, which Setup would otherwise wire twice.
	seen := map[iutil.Plugin]bool{}

	for _, p := range plugins {
		if seen[p] {
			return nil, fmt.Errorf("plugin %s is listed twice; two positions need two constructions", p.Name())
		}

		seen[p] = true

		// The first Want for an action is taken as-is; there's no identity
		// value to start a Debounce fold from.
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
		Conn:       conn,
		CommandOut: s.raised,
		Wanted:     s.wants,

		// The end of the chain does nothing: it is a call stack, so it unwinds
		// back to here by itself.
		Next: func(_ *iutil.Event) {},
	}

	// Wired backwards: a plugin needs the one after it to already be built.
	s.plugins = make([]plugged, len(plugins))

	for i := len(plugins) - 1; i >= 0; i-- {
		p := plugins[i]

		// Unbuffered: a slot would let the source believe an unlistening
		// plugin had heard it.
		in := make(chan iutil.Command)

		args.CommandIn = in

		err := p.Setup(args)
		if err != nil {
			return nil, fmt.Errorf("setting up %s: %w", p.Name(), err)
		}

		pl := plugged{plugin: p, in: in, done: make(chan struct{})}

		// A plugin with no goroutine is already finished, decided by its
		// type, not by what main remembers to say.
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
// canceled. It blocks, so main owns the goroutine, and calls it once.
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

		// Reset only when a listener actually opened: Incus accepts an
		// untrusted certificate's TLS connection and refuses only the stream.
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

// stopping reports whether the source has been told to stop.
func stopping(ctx context.Context) bool {
	select {
	case <-ctx.Done():
		return true
	default:
		return false
	}
}

// session runs one listener from open to close and reports whether it opened.
// Canceling closes the socket, so it gets a context derived from ctx.
func (s *Source) session(ctx context.Context) bool {
	sessionCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	events, err := s.listen(sessionCtx)
	if err != nil {
		s.logger.Warn("opening the Incus event listener", "err", err)

		return false
	}

	// Connected the moment Incus accepts the listener; the enricher reads a
	// whole fleet off it.
	s.hand(iutil.NewEvent(time.Now(), iutil.ActionConnected, "", "", ""))

	// Paired on every way out, including a canceled context, and cold with it.
	defer func() {
		s.chain = iutil.ChainCold

		s.hand(iutil.NewEvent(time.Now(), iutil.ActionDisconnected, "", "", ""))
	}()

	for {
		select {
		case <-ctx.Done():
			return true

		case cmd := <-s.raised:
			s.apply(cmd)

		case raw, ok := <-events:
			if !ok {
				logger.Warn("the Incus event stream closed")

				return true
			}

			s.route(raw)
		}
	}
}

// wait holds for d, still handing commands over: a reconnect is exactly when
// a pass fails and raises something.
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
			s.apply(cmd)
		}
	}
}

// route decodes one raw event and hands it over, unless nothing asked for it.
func (s *Source) route(raw incusapi.Event) {
	ev, err := decodeLifecycle(raw)
	if err != nil {
		if !errors.Is(err, errIgnored) {
			s.logger.Debug("decoding lifecycle event", "err", err)
		}

		return
	}

	// An action nobody declared never walks.
	_, wanted := s.wants[ev.Action()]
	if !wanted {
		return
	}

	s.hand(ev)
}

// hand gives one event to the head of the chain; each plugin calls its own successor.
func (s *Source) hand(ev *iutil.Event) {
	s.head(ev.WithChainState(s.chain))
}

// apply takes one command a plugin raised: a state sets, an action walks, and
// either may be left empty. Nothing here checks the transition.
func (s *Source) apply(cmd iutil.Command) {
	if cmd.ChainState != "" {
		s.chain = cmd.ChainState
	}

	if cmd.Action == "" {
		return
	}

	s.hand(iutil.NewEvent(time.Now(), cmd.Action, "", "", ""))
}

// Finished tells the source that p's Run has returned, so Drain stops waiting on it.
func (s *Source) Finished(p iutil.Plugin) {
	for _, pl := range s.plugins {
		if pl.plugin != p {
			continue
		}

		close(pl.done)

		return
	}
}

// Drain asks every plugin to finish, in chain order, and returns once the
// last has answered. Called after Run has returned, and bounded by ctx.
func (s *Source) Drain(ctx context.Context) {
	for _, pl := range s.plugins {
		select {
		case pl.in <- iutil.Command{Action: iutil.CommandDrain}:
		case <-pl.done:
			continue
		case <-ctx.Done():
			return
		}

		// The answer is the same action echoed back. Anything else raised on
		// the way out is too late to carry and is dropped rather than left to block.
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

// incusListener listens across every project the certificate can see, in one
// listener; which of them are served is the enricher's call.
func incusListener(conn *iclient.Connection) listenFunc {
	return func(ctx context.Context) (<-chan incusapi.Event, error) {
		return conn.ListenEventsAllProjects(ctx, []string{incusapi.EventTypeLifecycle})
	}
}
