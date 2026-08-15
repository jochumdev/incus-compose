package enricher

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	incusapi "github.com/lxc/incus/v7/shared/api"
	incusutil "github.com/lxc/incus/v7/shared/util"
	"github.com/panjf2000/ants/v2"

	"github.com/lxc/incus-compose/iclient"
	"github.com/lxc/incus-compose/ievent/iutil"
)

// The whole-fleet pass: what it reads, and how what it found is announced
// between the brackets it raises.

// startSweep sends the whole-fleet read to the pool, unless one is already out.
//
// One at a time: a second pass would read the same fleet to reach the same
// answer, and the events it raised would arrive interleaved with the first's,
// so absence would stop meaning anything.
func (p *Plugin) startSweep(ctx context.Context) {
	if p.sweeping {
		return
	}

	p.sweeping = true

	err := p.passes.Submit(func() {
		// The deadline binds the requests, never the send: a pass that runs out
		// of time still has to report, or sweeping never clears and no pass
		// runs again for the life of the process.
		readCtx, cancel := context.WithTimeout(ctx, sweepTimeout)
		defer cancel()

		res, err := p.list(readCtx)
		if res == nil {
			res = &sweepResult{}
		}

		res.err = err

		select {
		case p.sweeps <- *res:
		case <-ctx.Done():
		}
	})
	if err != nil {
		// The pool is full of instance reads. Those are the pass's work being
		// done piecemeal, so waiting is no loss.
		p.sweeping = false

		p.sooner(dirtyDelay)
	}
}

// announce patches the model from a finished pass and puts what it found into
// the line, between the brackets.
//
// Pushed straight into the queue rather than injected at the head: they are
// enriched already, and sending them round the chain would have debounce
// collapse a pass into the last instance of it and this plugin re-read every
// one of them.
func (p *Plugin) announce(ctx context.Context) {
	harvest := p.harvest
	p.harvest = nil

	// Held until here, not until the read landed. Between the two the bracket
	// is traveling the chain, and a second pass starting in that gap would
	// overwrite what this one found and put two opening brackets in flight.
	defer func() { p.sweeping = false }()

	p.m.putWires(harvest.networks)

	for project, config := range harvest.labels {
		p.m.putProject(project, config)
	}

	for i := range harvest.instances {
		full := harvest.instances[i]

		e := p.m.putInstance(&full.instance, full.state)

		// One event per instance, carrying what the pass read. The action is
		// instance-updated because that is what happened as far as anything
		// behind here is concerned: this is what it looks like now.
		ev := iutil.NewEvent(time.Now(),
			incusapi.EventLifecycleInstanceUpdated, full.instance.Project, full.instance.Name, "")

		// The pass pushes straight into the queue rather than going round
		// through accept, so the labels are attached here as well.
		p.q.push(p.withProject(ev.WithInstance(e.running, e.config, e.nets)), true)
	}

	// Everything the pass could not see is now absent from what it announced,
	// which is what a plugin pruning by absence acts on - so nothing is owed a
	// re-read any more.
	clear(p.m.dirty)

	p.due(p.opts.SweepInterval)
	p.raise(ctx, iutil.ActionSweepEnd)
}

// errNoNetworks reports a read in which nothing could be listed at all.
//
// A failure rather than an empty fleet. An empty map is a real answer - every
// network went away - and acting on it deletes every record there is, so it may
// only be believed when something was actually read.
var errNoNetworks = errors.New("no network list could be read")

// errNoInstances reports a pass in which no project could be listed at all.
//
// Same reason as errNoNetworks: an empty fleet is a real answer that prunes
// every record, so it may only be believed when something was actually read.
var errNoInstances = errors.New("no project could be listed")

// sweepResult is one whole-fleet read.
type sweepResult struct {
	networks  []incusapi.Network
	instances []instanceRead

	// labels is each project's own configuration, off the listing itself. See
	// fleetLabels for why the default profile is not where this comes from.
	labels map[string]map[string]string

	err error
}

// instanceRead is one instance and the state read beside it.
//
// Apart rather than an InstanceFull: full also fetches snapshots and backups,
// which is a great deal of a large fleet to carry across the wire for a record
// that needs an address and a label.
type instanceRead struct {
	instance incusapi.Instance
	state    *incusapi.InstanceState
}

// listFunc reads the whole fleet: every network, and every instance with its
// state.
type listFunc func(ctx context.Context) (*sweepResult, error)

// incusLister reads the fleet through the connection, fanning the state reads
// out over the pool it is given.
//
// That pool is not the one the pass itself runs on. A pass holds its worker for
// its whole life and then needs one per instance, so sharing would let every
// worker be a pass waiting on a worker only a pass can free.
func incusLister(
	conn *iclient.Connection,
	reads *ants.Pool,
	timeout time.Duration,
	serves func(*incusapi.Project) bool,
) listFunc {
	return func(ctx context.Context) (*sweepResult, error) {
		// One listing of the projects answers both halves: which of them own
		// networks, and which of them to list instances in.
		all, err := conn.GetProjects(ctx)
		if err != nil {
			return nil, fmt.Errorf("listing projects: %w", err)
		}

		projects := make([]incusapi.Project, 0, len(all))
		for i := range all {
			if serves == nil || serves(&all[i]) {
				projects = append(projects, all[i])
			}
		}

		networks, err := fleetNetworks(ctx, conn, projects)
		if err != nil {
			return nil, err
		}

		listed, err := fleetInstances(ctx, conn, projects)
		if err != nil {
			return nil, err
		}

		labels := fleetLabels(projects)

		out := make([]instanceRead, len(listed))

		var wg sync.WaitGroup

		for i := range listed {
			out[i] = instanceRead{instance: listed[i]}

			// Refused rather than queued, so a full pool means this pass reads
			// the state itself rather than waiting for a worker that is busy
			// with the events a pass exists to make unnecessary.
			err := reads.Submit(func() {
				defer wg.Done()

				out[i].state = instanceState(ctx, conn, timeout, out[i].instance)
			})
			if err != nil {
				out[i].state = instanceState(ctx, conn, timeout, out[i].instance)

				continue
			}

			wg.Add(1)
		}

		wg.Wait()

		return &sweepResult{networks: networks, instances: out, labels: labels}, nil
	}
}

// instanceState reads one instance's state, or nil if it could not be read.
//
// Nil is not an error here: an instance that went away between the listing and
// this is one the next pass will not list at all, and failing the whole pass
// over it would cost the fleet its repair.
func instanceState(
	ctx context.Context,
	conn *iclient.Connection,
	timeout time.Duration,
	inst incusapi.Instance,
) *incusapi.InstanceState {
	readCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	state, _, err := conn.WithProject(inst.Project).GetInstanceState(readCtx, inst.Name)
	if err != nil {
		return nil
	}

	return state
}

// fleetInstances lists every instance of every project, one project at a time.
//
// Per project rather than one all-projects listing. The bulk call has to render
// every instance of every project the certificate can see to answer at all,
// which is the whole fleet's worth of work for one request; asked a project at
// a time, incusd does that much only for the project asked about, and a project
// that fails costs its own instances rather than everybody's.
//
// Without Full either way: the listing carries configuration and devices, which
// is what a record is built from. State comes per instance, because the only
// bulk call that carries it also renders every snapshot and lists every backup
// of every instance, none of which a record depends on.
func fleetInstances(
	ctx context.Context,
	conn *iclient.Connection,
	projects []incusapi.Project,
) ([]incusapi.Instance, error) {
	var (
		out  []incusapi.Instance
		read bool
	)

	for _, project := range projects {
		list, err := conn.WithProject(project.Name).GetInstances(ctx, nil)
		if err != nil {
			// One project failing does not fail the pass, for the same reason
			// one network owner failing does not: its instances keep what they
			// had, where losing the fleet drops every record served.
			continue
		}

		read = true

		for i := range list {
			out = append(out, list[i].Instance)
		}
	}

	if !read {
		return nil, errNoInstances
	}

	return out, nil
}

// fleetNetworks reads every network of every project that owns any.
//
// Not one listing: a project with features.networks owns its own, and asking
// one project answers with that project's alone. So the projects say which of
// them own networks, and each owner is read.
//
// incusd decides that with IsTrue, so "1" and "yes" own their networks too -
// comparing against "true" alone is blind to those projects.
func fleetNetworks(
	ctx context.Context,
	conn *iclient.Connection,
	projects []incusapi.Project,
) ([]incusapi.Network, error) {
	owners := []string{defaultProject}

	for _, project := range projects {
		if project.Name != defaultProject && incusutil.IsTrue(project.Config[featuresNetworks]) {
			owners = append(owners, project.Name)
		}
	}

	var (
		out  []incusapi.Network
		read bool
	)

	for _, owner := range owners {
		list, err := conn.WithProject(owner).GetNetworks(ctx)
		if err != nil {
			// One owner failing does not fail the read: its networks keep
			// whatever they had, where losing the fleet's would drop every
			// record served.
			continue
		}

		read = true

		out = append(out, list...)
	}

	if !read {
		return nil, errNoNetworks
	}

	return out, nil
}

// fleetLabels takes each project's own configuration off the listing.
//
// The project itself, which is what `incus project set` writes, and not its
// default profile. A profile's keys are already in every instance's expanded
// configuration, so reading them here said nothing the instance had not said
// and left a project-wide setting nowhere to live.
//
// No read of its own: GetProjects answers with the configuration, so the whole
// of this is picking it out of a listing the pass has already done.
func fleetLabels(projects []incusapi.Project) map[string]map[string]string {
	out := make(map[string]map[string]string, len(projects))

	for _, project := range projects {
		out[project.Name] = project.Config
	}

	return out
}
