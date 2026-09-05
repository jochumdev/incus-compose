package main

import (
	"context"
	"log/slog"
	"slices"

	incusapi "github.com/lxc/incus/v7/shared/api"

	"github.com/lxc/incus-compose/ievent/checker"
	"github.com/lxc/incus-compose/ievent/debounce"
	"github.com/lxc/incus-compose/ievent/enricher"
	"github.com/lxc/incus-compose/ievent/http"
	"github.com/lxc/incus-compose/ievent/iutil"
	"github.com/lxc/incus-compose/ievent/log"
	"github.com/lxc/incus-compose/shared"
)

// runner is a plugin that owns a goroutine. main starts each one and waits for
// it, which is what lets the shutdown order be written down in one place.
type runner interface {
	iutil.Plugin

	Run(ctx context.Context) error
}

// chain is the compiled-in list, in the order events travel it. debounce sits
// before the enricher so a burst costs one read instead of one per event.
func chain(logger *slog.Logger, cfg *config) ([]iutil.Plugin, []runner) {
	plugins := []iutil.Plugin{}

	add := func(p iutil.Plugin) {
		plugins = append(plugins, p)
	}

	// TRACE adds a log position on either side of the enricher, so what a read
	// cost can be read off the pair.
	if cfg.Trace {
		add(log.New(logger, log.At("arrival"), log.Level("TRACE")))
	}

	add(debounce.New(logger))
	add(enricher.New(logger, enricher.Project(serves(logger, cfg))))

	if cfg.Trace {
		add(log.New(logger, log.At("enriched"), log.Level("TRACE")))
	}

	add(checker.New(logger,
		checker.Workers(cfg.Workers),
		checker.RestartWorkers(cfg.RestartWorkers),
		checker.Serveable(serveable(cfg)),
	))
	add(http.New(logger, http.Listen(cfg.HTTPAddr)))

	runners := []runner{}

	for _, p := range plugins {
		r, ok := p.(runner)
		if ok {
			runners = append(runners, r)
		}
	}

	return plugins, runners
}

// serves decides which projects this daemon watches, as the enricher's sweep
// asks: an explicit list wins, else the marker opts a project in.
func serves(logger *slog.Logger, cfg *config) func(*incusapi.Project) bool {
	if len(cfg.Projects) > 0 {
		return func(p *incusapi.Project) bool {
			serve := slices.Contains(cfg.Projects, p.Name)
			if !serve {
				logger.Debug("Not watching project", "project", p.Name)
			} else {
				logger.Log(context.Background(), shared.LevelTrace, "Watching project", "project", p.Name)
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
			logger.Debug("Not watching project", "project", p.Name)
		} else {
			logger.Log(context.Background(), shared.LevelTrace, "Watching project", "project", p.Name)
		}
		return serve
	}
}

// serveable is the same decision per event, for the checker: the list checks
// the event's project, the marker checks what the enricher read onto it.
func serveable(cfg *config) func(*iutil.Event) bool {
	if len(cfg.Projects) > 0 {
		return func(ev *iutil.Event) bool {
			return slices.Contains(cfg.Projects, ev.ProjectName())
		}
	}

	if cfg.ProjectMarker == "" {
		return nil
	}

	return func(ev *iutil.Event) bool {
		if !ev.Enriched(iutil.EnrichedProject) {
			return false
		}

		value, ok := ev.Project().ConfigValue(cfg.ProjectMarker)
		return ok && value == cfg.ProjectMarkerValue
	}
}
