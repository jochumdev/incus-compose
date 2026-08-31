package enricher

import (
	"context"
	"strings"

	incusapi "github.com/lxc/incus/v7/shared/api"

	"github.com/lxc/incus-compose/ievent/iutil"
)

// networkPrefix is what an action names a network with.
const networkPrefix = "network-"

// acceptNetwork handles one network action, and reports whether it was one.
// The event itself carries no enrichment, since its subject is a network; what
// it causes is the patch, and the re-read of everything on that network.
func (p *Plugin) acceptNetwork(ctx context.Context, ev *iutil.Event) bool {
	if !strings.HasPrefix(ev.Action(), networkPrefix) {
		return false
	}

	// The event that happened goes in first, ahead of anything it causes.
	p.q.push(ev, true)

	// A rename leaves the old key behind; everything that was on it is re-read,
	// because the key its addresses were filed under has gone.
	if ev.OldName() != "" {
		p.forgetNetwork(ctx, ev.ProjectName(), ev.OldName())
	}

	if ev.Action() == incusapi.EventLifecycleNetworkDeleted {
		p.forgetNetwork(ctx, ev.ProjectName(), ev.Name())

		return true
	}

	// Created, updated or renamed: the network is read and patched first, so the
	// re-reads resolve against the new subnet rather than the old one.
	p.reads.send(ctx, &call{
		key:     resourceKey(kindNetwork, ev.ProjectName(), ev.Name()),
		kind:    kindNetwork,
		project: ev.ProjectName(),
		name:    ev.Name(),
	})

	return true
}

// forgetNetwork drops one network and re-reads what was on it. Both halves matter:
// dropping alone leaves addresses filed under a key nothing describes any
// more, and re-reading alone leaves the network there to resolve against.
func (p *Plugin) forgetNetwork(ctx context.Context, project, name string) {
	on := p.state.networkInstances(project, name)

	p.state.deleteNetwork(project, name)
	p.fanOut(ctx, on)
}
