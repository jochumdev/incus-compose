// Package http serves /metrics, /health and /ready.
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

	"github.com/lxc/incus-compose/ievent/iutil"
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
	// Listen is the address to answer on; empty serves nothing.
	Listen string

	// Silence is how long the chain may say nothing before /health fails; zero never fails on silence.
	Silence time.Duration

	// Metrics turns the /metrics endpoint on.
	Metrics bool

	// Pprof enables /debug/pprof; never on in a deployment.
	Pprof bool
}

// Plugin answers the observability endpoints.
type Plugin struct {
	cfg Config

	next iutil.Next

	// in is the source asking this plugin to finish; out is the answer.
	in  <-chan iutil.Command
	out chan<- iutil.Command

	// ready latches on the chain turning warm and clears when it turns cold.
	ready atomic.Bool

	// connected is the source's own state, straight off the chain.
	connected atomic.Bool

	// lastEvent is when something last walked past, as UnixNano.
	lastEvent atomic.Int64
}

// Option sets one field of Config; the zero value means unset.
type Option func(*Config)

// Listen sets the address to answer on. Empty serves nothing.
func Listen(addr string) Option { return func(cfg *Config) { cfg.Listen = addr } }

// Silence sets how long the chain may say nothing before /health fails; zero never fails.
func Silence(d time.Duration) Option { return func(cfg *Config) { cfg.Silence = d } }

// Metrics turns the /metrics endpoint on.
func Metrics(v bool) Option { return func(cfg *Config) { cfg.Metrics = v } }

// Pprof turns /debug/pprof on; not for a deployment.
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

// Name identifies the plugin in logs, in metrics, and in the chain.
func (p *Plugin) Name() string { return name }

// Wants nothing: it is in the chain, so it sees whatever walks.
func (p *Plugin) Wants() []iutil.Want { return nil }

// Setup keeps the successor and starts nothing.
func (p *Plugin) Setup(args iutil.SetupArgs) error {
	p.next = args.Next
	p.in, p.out = args.CommandIn, args.CommandOut

	return nil
}

// Handle folds the event into what is served and hands it straight on, on the caller's goroutine.
func (p *Plugin) Handle(ev *iutil.Event) {
	p.lastEvent.Store(time.Now().UnixNano())

	switch ev.Action() {
	case iutil.ActionConnected:
		p.connected.Store(true)

	case iutil.ActionDisconnected:
		p.connected.Store(false)
	}

	switch ev.ChainState() {
	case iutil.ChainCold:
		p.ready.Store(false)

	case iutil.ChainWarm:
		p.ready.Store(true)
	}

	p.next(ev)
}

// Run serves until ctx is canceled; it blocks, so main owns the goroutine.
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

	// ?seconds=30 only answers once finished, so the write timeout would cut it short.
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
		// Nothing is held here, so there is nothing to drain.
		select {
		case p.out <- cmd:
		case <-ctx.Done():
		}
	}

	// Its own budget: ctx may already be done, and inheriting it would cut live connections.
	shutCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownTimeout)
	defer cancel()

	return server.Shutdown(shutCtx)
}

// wait holds until told to finish, for a build that asked for no endpoints.
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

// health answers whether the process is worth keeping, not whether it is ready.
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

// serveReady answers whether there is anything worth sending traffic at.
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
var _ iutil.Plugin = (*Plugin)(nil)
