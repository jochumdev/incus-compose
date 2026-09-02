package main

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"slices"
	"strings"

	"github.com/coredns/coredns/plugin"
	"github.com/coredns/coredns/plugin/cache"
	"github.com/coredns/coredns/plugin/forward"
	"github.com/coredns/coredns/plugin/loop"
	proxypkg "github.com/coredns/coredns/plugin/pkg/proxy"
	incusapi "github.com/lxc/incus/v7/shared/api"

	"github.com/lxc/incus-compose/ievent/debounce"
	"github.com/lxc/incus-compose/ievent/dns"
	"github.com/lxc/incus-compose/ievent/enricher"
	"github.com/lxc/incus-compose/ievent/http"
	"github.com/lxc/incus-compose/ievent/iutil"
	"github.com/lxc/incus-compose/ievent/log"
	"github.com/lxc/incus-compose/shared"
)

// position is one entry in the chain this binary was compiled with. Order is
// the binary's; a deployment may only leave out the positions marked optional.
type position struct {
	plugin iutil.Plugin

	// optional says --exclude may drop this position. The enricher and dns may
	// not go: without a read the process starts, answers, and serves nothing.
	optional bool
}

// runner is a plugin that owns a goroutine. main starts each one and waits for
// it, which is what lets the shutdown order be written down in one place.
type runner interface {
	iutil.Plugin

	Run(ctx context.Context) error
}

// chain is the compiled-in list, in the order events travel it. debounce sits
// before the enricher so a burst costs one read instead of one per event.
func chain(logger *slog.Logger, cfg config) []position {
	out := []position{}

	dnsPlugins, stop := queryChain(cfg)

	// TRACE adds a log position on either side of debounce, so ordering and
	// what a position cost can be read off the pair.
	trace := shared.StringToSlogLevel(cfg.Log) <= shared.LevelTrace

	if trace {
		out = append(out, logAt(logger, cfg, "arrival")...)
	}

	out = append(out,
		position{plugin: debounce.New(logger, debounce.Window(cfg.DebounceWindow)), optional: true},
	)

	if trace {
		out = append(out, logAt(logger, cfg, "received")...)
	}

	serves := func(cfg config) func(*incusapi.Project) bool {
		if len(cfg.Projects) > 0 {
			return func(p *incusapi.Project) bool {
				serve := slices.Contains(cfg.Projects, p.Name)
				if !serve {
					logger.Debug("Not serving project", "project", p.Name)
				} else {
					logger.Log(context.Background(), shared.LevelTrace, "Serving project", "project", p.Name)
				}
				return serve
			}
		}

		if cfg.ProjectMarker == "" {
			// Every project the certificate can see, which is the only answer that
			// works on a plain Incus.
			return nil
		}

		return func(p *incusapi.Project) bool {
			serve := p.Config[cfg.ProjectMarker] == cfg.ProjectMarkerValue
			if !serve {
				logger.Debug("Not serving project", "project", p.Name)
			} else {
				logger.Log(context.Background(), shared.LevelTrace, "Serving project", "project", p.Name)
			}
			return serve
		}
	}

	out = append(out,
		position{plugin: enricher.New(
			logger,
			enricher.Workers(cfg.Workers),
			enricher.ReadTimeout(cfg.ReadTimeout),
			enricher.ReadDelay(cfg.ReadDelay),
			enricher.Project(serves(cfg)),
			enricher.StoreFile(filepath.Join(cfg.DataDir, "enricher-cold-store.json")),
		)},
	)

	out = append(out, logAt(logger, cfg, "enriched")...)

	out = append(out,
		position{plugin: dns.New(
			logger,
			dns.Listen(cfg.DNSAddr),
			dns.Chain(dnsPlugins),
			dns.Stop(stop),
			dns.EchoSubnet(cfg.EchoSubnet),
			dns.Metrics(cfg.Metrics),
			dns.ColdDir(cfg.DataDir),
			dns.TTL(cfg.TTL),
			dns.Suffix(cfg.Suffix),
			dns.Project(serves(cfg)),
			dns.AllowTransfer(cfg.AllowTransfer),
		)},
		// After dns, so a readiness it raises is folded before this position
		// sees the event that caused it. Any position works; this one reads best.
		position{plugin: http.New(logger, http.Listen(cfg.HTTPAddr), http.Metrics(cfg.Metrics), http.Pprof(cfg.Pprof)), optional: true},
	)

	out = append(out, logAt(logger, cfg, "served")...)

	return out
}

// logAt is one log position, or nothing at all when --log is empty. Every
// position prints a line per event, so a run not reading them does not pay for
// them - which is the default, and why this returns a slice rather than a plugin.
func logAt(logger *slog.Logger, cfg config, at string) []position {
	if cfg.Log == "" {
		return nil
	}

	return []position{{plugin: log.New(logger, log.At(at), log.Level(cfg.Log)), optional: true}}
}

// queryChain is what answers after the engine, in CoreDNS's own order: cache,
// loop, forward. Listing any of them links core/dnsserver; a -light build does not.
func queryChain(cfg config) ([]plugin.Plugin, func()) {
	if len(cfg.Forward) == 0 {
		return nil, nil
	}

	// Before the forwarder, never before the engine: what the engine
	// answers is in memory and cannot go stale.
	c := cache.New()

	// The upstream being our own resolver is how a query this server sends
	// itself comes back to it.
	lp := loop.New(".")

	fwd := forward.New()

	for _, upstream := range cfg.Forward {
		// SetProxy also starts the health checker, so OnStartup is not called.
		fwd.SetProxy(proxypkg.NewProxy(upstream, upstream, "dns"))
	}

	dnsPlugins := []plugin.Plugin{
		func(next plugin.Handler) plugin.Handler { c.Next = next; return c },
		func(next plugin.Handler) plugin.Handler { lp.Next = next; return lp },
		func(next plugin.Handler) plugin.Handler { fwd.Next = next; return fwd },
	}

	// forward is the only one here with anything to stop: its health checkers.
	return dnsPlugins, func() { _ = fwd.OnShutdown() }
}

// assemble drops what --exclude named and reports what is left, plus the ones
// main has to run a goroutine for. An exclude that names nothing is an error.
func assemble(positions []position, exclude []string) ([]iutil.Plugin, []runner, error) {
	optional := []string{}

	for _, p := range positions {
		if p.optional {
			optional = append(optional, p.plugin.Name())
		}
	}

	for _, name := range exclude {
		if slices.Contains(optional, name) {
			continue
		}

		return nil, nil, fmt.Errorf(
			"cannot exclude %q; this binary allows %s",
			name, strings.Join(optional, ", "),
		)
	}

	var (
		plugins []iutil.Plugin
		runners []runner
	)

	for _, p := range positions {
		if slices.Contains(exclude, p.plugin.Name()) {
			continue
		}

		plugins = append(plugins, p.plugin)

		r, ok := p.plugin.(runner)
		if ok {
			runners = append(runners, r)
		}
	}

	return plugins, runners, nil
}
