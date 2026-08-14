package main

import (
	"context"
	"fmt"
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
	"github.com/lxc/incus-compose/ievent/log"
	"github.com/lxc/incus-compose/ievent/shared"
)

// position is one entry in the chain this binary was compiled with. Order is
// the binary's; a deployment may only leave out the positions marked optional.
type position struct {
	plugin shared.Plugin

	// optional says --exclude may drop this position. The enricher and dns may
	// not go: without a read the process starts, answers, and serves nothing.
	optional bool
}

// runner is a plugin that owns a goroutine. main starts each one and waits for
// it, which is what lets the shutdown order be written down in one place.
type runner interface {
	shared.Plugin

	Run(ctx context.Context) error
}

// chain is the compiled-in list, in the order events travel it. debounce sits
// in front of the enricher so a burst costs one read instead of one per event.
func chain(cfg config) []position {
	out := []position{}

	behind, stop := queryChain(cfg)

	// --trace adds a second log position in front, so ordering and what a
	// position cost can be read off the pair.
	if cfg.Trace {
		out = append(out, position{plugin: log.New(log.At("arrival")), optional: true})
	}

	out = append(out,
		position{plugin: debounce.New(debounce.Window(cfg.DebounceWindow)), optional: true},
	)

	if cfg.Trace {
		out = append(out, position{plugin: log.New(log.At("received"), log.Level("TRACE")), optional: true})
	}

	out = append(out,
		position{plugin: enricher.New(
			enricher.Workers(cfg.Workers),
			enricher.ReadTimeout(cfg.ReadTimeout),
			enricher.SweepInterval(cfg.SweepInterval),
			enricher.Project(serves(cfg)),
		)},
		position{plugin: dns.New(
			dns.Listen(cfg.DNSAddr),
			dns.Behind(behind),
			dns.Stop(stop),
			dns.EchoSubnet(cfg.EchoSubnet),
			dns.Metrics(cfg.Metrics),
			dns.ColdDir(cfg.DataDir),
			dns.TTL(cfg.TTL),
			dns.Suffix(cfg.Suffix),
		)},
		// Behind dns, so a readiness it raises is folded before this position
		// sees the event that caused it. Any position works; this one reads best.
		position{plugin: http.New(http.Listen(cfg.HTTPAddr), http.Metrics(cfg.Metrics), http.Pprof(cfg.Pprof)), optional: true},
		position{plugin: log.New(log.At(traceName(cfg)), log.Level("DEBUG")), optional: true},
	)

	return out
}

// traceName names the last log position, and only when --trace put another one
// in front of it.
func traceName(cfg config) string {
	if cfg.Trace {
		return "served"
	}

	return ""
}

// queryChain is what answers behind the engine, in CoreDNS's own order: cache,
// loop, forward. Listing any of them links core/dnsserver; a -light build does not.
func queryChain(cfg config) ([]plugin.Plugin, func()) {
	if len(cfg.Forward) == 0 {
		return nil, nil
	}

	// In front of the forwarder, never in front of the engine: what the engine
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

	behind := []plugin.Plugin{
		func(next plugin.Handler) plugin.Handler { c.Next = next; return c },
		func(next plugin.Handler) plugin.Handler { lp.Next = next; return lp },
		func(next plugin.Handler) plugin.Handler { fwd.Next = next; return fwd },
	}

	// forward is the only one here with anything to stop: its health checkers.
	return behind, func() { _ = fwd.OnShutdown() }
}

// serves decides which projects this binary reads: an explicit list, otherwise
// the marker a project opts in with.
func serves(cfg config) func(*incusapi.Project) bool {
	if len(cfg.Projects) > 0 {
		return func(p *incusapi.Project) bool { return slices.Contains(cfg.Projects, p.Name) }
	}

	if cfg.ProjectMarker == "" {
		// Every project the certificate can see, which is the only answer that
		// works on a plain Incus.
		return nil
	}

	return func(p *incusapi.Project) bool {
		return p.Config[cfg.ProjectMarker] == cfg.ProjectMarkerValue
	}
}

// assemble drops what --exclude named and reports what is left, plus the ones
// main has to run a goroutine for. An exclude that names nothing is an error.
func assemble(positions []position, exclude []string) ([]shared.Plugin, []runner, error) {
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
		plugins []shared.Plugin
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
