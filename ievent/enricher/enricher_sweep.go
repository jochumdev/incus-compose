package enricher

import (
	"context"
	"time"

	incusapi "github.com/lxc/incus/v7/shared/api"

	"github.com/lxc/incus-compose/ievent/iutil"
)

// sweepArgs is what every run this plugin starts is given. The reads are the
// plugin's own, so a test that answers them answers the run as well.
func (p *Plugin) sweepArgs() sweepArgs {
	return sweepArgs{
		conn:     p.reads.fleet,
		readNet:  p.reads.readNet,
		sweepOut: p.sweeps,
		evOut:    p.inbox,
		serves:   p.opts.Project,
		timeout:  p.opts.ReadTimeout,
		delay:    p.opts.ReadDelay,
	}
}

// restartSweep abandons the run where it stands and starts a fast one. What a
// reconnect asks for: everything held is as old as the stream was down.
func (p *Plugin) restartSweep(ctx context.Context) {
	if p.sweeperCancel != nil {
		p.sweeperCancel()
	}

	sweepCtx, stopSweeping := context.WithCancel(ctx)
	p.sweeperCancel = stopSweeping

	runSweeper(sweepCtx, p.logger, p.sweepArgs(), p.opts.SweepInterval)
}

// acceptSweep takes one message from the sweeper.
func (p *Plugin) acceptSweep(ctx context.Context, msg sweepMsg) {
	switch msg.action {
	case sweepActionProjects:
		p.state.pruneProjects(msg.names)

		// The run lists, this plugin reads: a project's own configuration is
		// what an instance event in it carries, and it goes through the pool
		// like every other read this plugin makes.
		for _, project := range msg.names {
			p.reads.send(ctx, &call{
				key:     resourceKey(kindProject, project, ""),
				kind:    kindProject,
				project: project,
			})
		}

	case sweepActionNetworks:
		p.state.pruneNetworks(msg.project, msg.names)

	case sweepActionInstances:
		// Through accept, which is where a delete drops the instance and its
		// archive entry: a name Incus no longer lists is gone the same way one
		// it announced is, and nothing downstream can tell them apart.
		for _, s := range p.state.missingInstances(msg.project, msg.names) {
			p.accept(ctx, iutil.NewEvent(time.Now(),
				incusapi.EventLifecycleInstanceDeleted, s.project, s.instance, ""))
		}

		for ev := range p.q.release() {
			p.args.Next(ev)
		}

	case sweepActionNetwork:
		p.state.setNetwork(*msg.network)

	case sweepActionNetworksWarm:
		// Every network an instance could sit on is now known, so the reads held
		// back until one could be placed may go.
		p.reads.flush(ctx)

	case sweepActionDone:
		// Held back while the run's own project reads are still out: warm says
		// the fleet has been read whole, and it has not been until they land.
		// A run over a project with no instances reaches here first.
		if p.reads.owes() > 0 {
			p.sweepEnding = true

			return
		}

		p.sweepEnd(ctx)

	case sweepActionFailed:
		if p.opts.Metrics {
			sweepsTotal.WithLabelValues("failed").Inc()
		}
		p.logger.Warn("the run could not read part of the fleet, serving what is held",
			"plugin", name, "err", msg.err)
	}
}

// sweepEnd says a run has been all the way round.
//
// At the head rather than through next, so debounce sees the end of a run too.
// Warm rides with it: this plugin is what decides the fleet has been read whole,
// and a run reaching its end is when that becomes true.
func (p *Plugin) sweepEnd(ctx context.Context) {
	p.sweepEnding = false
	if p.opts.Metrics {
		sweepsTotal.WithLabelValues("success").Inc()
	}
	p.updateMetrics()

	select {
	case p.args.CommandOut <- iutil.Command{Action: iutil.ActionSweepEnd, ChainState: iutil.ChainWarm}:
	case <-ctx.Done():
	}
}
