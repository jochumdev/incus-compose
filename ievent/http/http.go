// Package http serves /metrics, /health and /ready.
//
// A plugin at a position rather than something main runs: it holds no reference
// to another plugin and learns everything by being in the chain.
package http

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/pprof"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/lxc/incus-compose/ievent/shared"
)

// name is what this plugin is called, in the chain and in a drop's reason.
const name = "http"

// Timeouts for the server. Small, because everything it answers is a field read.
const (
	readTimeout     = 5 * time.Second
	writeTimeout    = 10 * time.Second
	shutdownTimeout = 5 * time.Second
)

// Config is where and what to serve.
type Config struct {
	// Listen is the address to answer on. Empty serves nothing, which is what
	// a build that only wants DNS asks for.
	Listen string

	// Silence is how long the chain may say nothing before /health fails. Zero
	// never fails on silence, which is the right default for a quiet fleet.
	Silence time.Duration

	// Metrics enable/disables the metrics endpoint.
	Metrics bool

	// Pprof enables /debug/pprof. Off by default and never on in a deployment:
	// the handlers expose the command line and the heap, and a profile costs
	// what it measures. It is here because the query path's own cost is a
	// fraction of what a wire run reports, so the rest of it can only be found
	// with a profile.
	Pprof bool
}

// Plugin answers the observability endpoints.
//
// Two goroutines touch it: the chain's through Handle, and net/http's through
// the handlers. Nothing here is read and written as a pair, so atomics do.
type Plugin struct {
	cfg Config

	next shared.Next

	// in is the source asking this plugin to finish, and out is the answer.
	// Its own channel, so the question arrives whatever else is going on.
	in  <-chan shared.Command
	out chan<- shared.Command

	// ready is what dns last announced. It starts false, which is the truth
	// before anything has been read.
	ready atomic.Bool

	// connected is the source's own state, straight off the chain.
	connected atomic.Bool

	// lastEvent is when something last walked past, as UnixNano so it is one
	// atomic rather than a time.Time behind a lock.
	lastEvent atomic.Int64
}

// Option sets one of them. The zero value means unset, and New fills this
// plugin's own default in.
type Option func(*Config)

// Listen sets the address to answer on. Empty serves nothing.
func Listen(addr string) Option { return func(cfg *Config) { cfg.Listen = addr } }

// Silence sets how long the chain may say nothing before /health fails. Zero
// never fails on silence.
func Silence(d time.Duration) Option { return func(cfg *Config) { cfg.Silence = d } }

// Metrics turns the /metrics endpoint on.
func Metrics(v bool) Option { return func(cfg *Config) { cfg.Metrics = v } }

// Pprof turns /debug/pprof on. For finding where a query's time goes; not for
// a deployment.
func Pprof(v bool) Option { return func(cfg *Config) { cfg.Pprof = v } }

// New builds the endpoint server. It starts nothing: Run owns the goroutine.
func New(opts ...Option) *Plugin {
	var cfg Config

	for _, opt := range opts {
		opt(&cfg)
	}

	slog.Info("Starting", "plugin", name, "config", cfg)

	p := &Plugin{cfg: cfg}
	p.lastEvent.Store(time.Now().UnixNano())

	return p
}

func (p *Plugin) Name() string { return name }

// Wants nothing: it is in the chain, so it sees whatever walks.
func (p *Plugin) Wants() []shared.Want { return nil }

// Setup keeps the successor and starts nothing.
func (p *Plugin) Setup(args shared.SetupArgs) error {
	p.next = args.Next
	p.in, p.out = args.CommandIn, args.CommandOut

	return nil
}

// Handle folds the event into what is served and hands it straight on, on the
// caller's goroutine. No inbox, like log: staying synchronous costs three
// atomic stores.
func (p *Plugin) Handle(ev *shared.Event) {
	p.lastEvent.Store(time.Now().UnixNano())

	switch ev.Action() {
	case shared.ActionConnected:
		p.connected.Store(true)

	case shared.ActionDisconnected:
		p.connected.Store(false)

	case shared.ActionReady:
		p.ready.Store(true)

	case shared.ActionNotReady:
		p.ready.Store(false)
	}

	p.next(ev)
}

// Run serves until ctx is canceled. It blocks, so main owns the goroutine. An
// empty Listen waits rather than returning, so it does not look like a failure.
func (p *Plugin) Run(ctx context.Context) error {
	if p.cfg.Listen == "" {
		p.wait(ctx)

		return nil
	}

	mux := http.NewServeMux()

	if p.cfg.Metrics {
		mux.Handle("GET /metrics", promhttp.Handler())
	}

	mux.HandleFunc("GET /health", p.health)
	mux.HandleFunc("GET /ready", p.serveReady)

	// No method prefix: `go tool pprof` resolves symbols with a POST, and the
	// trailing slash is what routes /heap, /goroutine and /allocs to Index.
	if p.cfg.Pprof {
		mux.HandleFunc("/debug/pprof/", pprof.Index)
		mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
		mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
		mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
		mux.HandleFunc("/debug/pprof/trace", pprof.Trace)
	}

	// A profile answers only when it is finished, so ?seconds=30 is a write that
	// starts 30 s from now. The usual timeout would cut every profile longer
	// than it and report a truncated one, so pprof takes the timeout off - which
	// is the other reason this is not for a deployment.
	write := writeTimeout
	if p.cfg.Pprof {
		write = 0
	}

	server := &http.Server{
		Addr:         p.cfg.Listen,
		Handler:      mux,
		ReadTimeout:  readTimeout,
		WriteTimeout: write,
	}

	errs := make(chan error, 1)

	go func() {
		err := server.ListenAndServe()
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			errs <- err
		}
	}()

	select {
	case err := <-errs:
		return err
	case <-ctx.Done():
	case cmd := <-p.in:
		// Nothing is held here, so there is nothing to drain. The endpoints keep
		// answering until the server is shut down below.
		select {
		case p.out <- cmd:
		case <-ctx.Done():
		}
	}

	// Its own budget: ctx may already be done, and a shutdown that inherited it
	// would cut connections rather than let them finish.
	shutCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownTimeout)
	defer cancel()

	return server.Shutdown(shutCtx)
}

// wait holds until told to finish, for a build that asked for no endpoints. It
// still answers, or the source waits on a silence it cannot tell from a wedge.
func (p *Plugin) wait(ctx context.Context) {
	select {
	case <-ctx.Done():
	case cmd := <-p.in:
		select {
		case p.out <- cmd:
		case <-ctx.Done():
		}
	}
}

// health answers whether the process is worth keeping. Deliberately not
// readiness: a lost stream is unready, never unhealthy, because a restart does
// not fix an Incus that is down.
func (p *Plugin) health(w http.ResponseWriter, _ *http.Request) {
	if p.cfg.Silence > 0 {
		since := time.Since(time.Unix(0, p.lastEvent.Load()))
		if since > p.cfg.Silence {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte("no event for " + since.Round(time.Second).String() + "\n"))

			return
		}
	}

	_, _ = w.Write([]byte("ok\n"))
}

// serveReady answers whether there is anything worth sending traffic at: dns
// has published, and the stream is connected. The second is not implied by the
// first.
func (p *Plugin) serveReady(w http.ResponseWriter, _ *http.Request) {
	switch {
	case !p.connected.Load():
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte("the Incus event stream is not connected\n"))

	case !p.ready.Load():
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte("nothing published yet\n"))

	default:
		_, _ = w.Write([]byte("ready\n"))
	}
}

// _ pins the interface here, so a change to it fails the build at the plugin.
var _ shared.Plugin = (*Plugin)(nil)
