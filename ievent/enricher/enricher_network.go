package enricher

import (
	"context"
	"fmt"
	"strings"

	incusapi "github.com/lxc/incus/v7/shared/api"

	"github.com/lxc/incus-compose/iclient"
	"github.com/lxc/incus-compose/ievent/iutil"
)

// The network path: a wire is patched in place, and everything sitting on it is
// re-read, because a subnet moving changes every record on that wire.

// networkPrefix is what an action names a network with.
const networkPrefix = "network-"

// netReadFunc is one network read. Its own type beside readFunc so a test can
// answer either without a daemon.
type netReadFunc func(ctx context.Context, project, name string) (*incusapi.Network, error)

// incusNetReader reads one network through the connection.
func incusNetReader(conn *iclient.Connection) netReadFunc {
	return func(ctx context.Context, project, name string) (*incusapi.Network, error) {
		net, _, err := conn.WithProject(project).GetNetwork(ctx, name)
		if err != nil {
			return nil, fmt.Errorf("reading network %s/%s: %w", project, name, err)
		}

		return net, nil
	}
}

// acceptNetwork handles one network action, and reports whether it was one.
//
// The event itself carries no enrichment: its subject is a wire, and there is
// nothing about a wire that an event shaped for an instance can hold. What it
// causes is the patch, and the re-read of everything on that wire.
func (p *Plugin) acceptNetwork(ctx context.Context, ev *iutil.Event) bool {
	if !strings.HasPrefix(ev.Action(), networkPrefix) {
		return false
	}

	// The event that happened goes in first, ahead of anything it causes. What
	// follows are re-reads this plugin decided on, and a record of the change
	// arriving after its own consequences reads backwards.
	p.q.push(ev, true)

	// A rename leaves the old key behind whatever else happens. Everything that
	// was on it is re-read, because the key its addresses were filed under has
	// gone even though the wire has not.
	if ev.OldName() != "" {
		p.forget(ctx, ev.Project(), ev.OldName())
	}

	if ev.Action() == incusapi.EventLifecycleNetworkDeleted {
		p.forget(ctx, ev.Project(), ev.Name())

		return true
	}

	// Created, updated or renamed: the wire is read and patched, and the
	// re-reads follow once it lands, so what they resolve against is the new
	// subnet rather than the old one.
	p.issue(ctx, &flight{
		key:     flightKey(true, ev.Project(), ev.Name()),
		project: ev.Project(),
		name:    ev.Name(),
		network: true,
	})

	return true
}

// forget drops one wire and re-reads what was on it.
//
// Both halves matter. Dropping alone leaves every instance holding addresses
// under a key nothing describes any more; re-reading alone leaves the wire
// there for the next instance to resolve against.
func (p *Plugin) forget(ctx context.Context, project, name string) {
	on := p.m.instancesOn(key(project, name))

	p.m.dropWire(project, name)
	p.fanOut(ctx, on)
}
