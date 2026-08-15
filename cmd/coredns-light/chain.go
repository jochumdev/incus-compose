package main

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"strings"

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
// in front of the enricher so a burst costs one read instead of one per event.
func chain(cfg config) []position {
	out := []position{}

	// --trace adds a second log position in front, so ordering and what a
	// position cost can be read off the pair.
	if cfg.Trace {
		out = append(out, position{plugin: log.New(log.At("arrival"), log.Level("TRACE")), optional: true})
	}

	out = append(out,
		position{plugin: debounce.New(debounce.Window(cfg.DebounceWindow)), optional: true},
	)

	if cfg.Trace {
		out = append(out, position{plugin: log.New(log.At("received"), log.Level("DEBUG")), optional: true})
	}

	out = append(out,
		position{plugin: enricher.New(
			enricher.Workers(cfg.Workers),
			enricher.ReadTimeout(cfg.ReadTimeout),
			enricher.SweepInterval(cfg.SweepInterval),
			enricher.Project(serves(cfg)),
		)},
		position{plugin: log.New(log.At("enriched"), log.Level("TRACE")), optional: true},
		position{plugin: dns.New(
			dns.Listen(cfg.DNSAddr),
			dns.EchoSubnet(cfg.EchoSubnet),
			dns.Metrics(cfg.Metrics),
			dns.ColdDir(cfg.DataDir),
			dns.TTL(cfg.TTL),
			dns.Suffix(cfg.Suffix),
		)},
		// Behind dns, so a readiness it raises is folded before this position
		// sees the event that caused it. Any position works; this one reads best.
		position{plugin: http.New(http.Listen(cfg.HTTPAddr), http.Metrics(cfg.Metrics), http.Pprof(cfg.Pprof)), optional: true},
		position{plugin: log.New(log.At("served"), log.Level("INFO")), optional: true},
	)

	return out
}

// serves decides which projects this binary reads: an explicit list, otherwise
// the marker a project opts in with.
func serves(cfg config) func(*incusapi.Project) bool {
	if len(cfg.Projects) > 0 {
		return func(p *incusapi.Project) bool {
			serve := slices.Contains(cfg.Projects, p.Name)
			if !serve {
				slog.Debug("Not serving project", "project", p.Name)
			} else {
				slog.Log(context.Background(), shared.LevelTrace, "Serving project", "project", p.Name)
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
			slog.Debug("Not serving project", "project", p.Name)
		} else {
			slog.Log(context.Background(), shared.LevelTrace, "Serving project", "project", p.Name)
		}
		return serve
	}
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
