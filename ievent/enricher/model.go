package enricher

import (
	"net/netip"
	"slices"
	"strings"

	incusapi "github.com/lxc/incus/v7/shared/api"

	"github.com/lxc/incus-compose/ievent/iutil"
)

// What the enricher holds of the fleet, and how one read patches it.
//
// Local patches rather than re-reads: an event says what changed, and what it
// changed is one instance, one network or one project. Reading the fleet to
// learn what a delete already told us is the cost this exists to avoid.
//
// It belongs to the goroutine Run owns. Nothing here is locked, and nothing
// here may be touched from a pool worker - a worker reads Incus and hands the
// answer back, and the patch happens where the model lives.

// defaultProject is where a bridge lives unless a project owns its own.
const defaultProject = "default"

// featuresNetworks is the project config key that gives a project networks of
// its own, rather than referencing the default project's.
const featuresNetworks = "features.networks"

// model is the fleet as the enricher last saw it.
type model struct {
	// instances is keyed by project/name, which is what an event names.
	instances map[string]*instance

	// wires is every network by owner and name. Apart from the instances
	// because a bridge is iutil: one entry answers for every instance on it,
	// and a network event patches one place rather than every instance.
	wires map[string]wire

	// projects is each project's own configuration, as `incus project set`
	// writes it - never its default profile, whose keys every instance already
	// carries expanded.
	projects map[string]map[string]string

	// dirty is what this holds and does not trust: a key whose read failed,
	// leaving whatever was there before it standing.
	//
	// A failure is drift, and the sweep is what repairs drift, so nothing
	// retries here - the key is noted and the next pass is pulled in early.
	// Retrying inline would stall every event queued behind this one for the
	// whole of the backoff.
	dirty map[string]bool
}

func newModel() *model {
	return &model{
		instances: map[string]*instance{},
		wires:     map[string]wire{},
		projects:  map[string]map[string]string{},
		dirty:     map[string]bool{},
	}
}

// instance is what one instance read amounted to. Facts, not verdicts: running
// with no addresses is reported as exactly that, and what it means is the
// business of whoever asked.
type instance struct {
	running bool
	config  map[string]string

	// nets is what this instance holds on each network it sits on, keyed the
	// same way wires is.
	nets map[string]*iutil.Network
}

// wire is one network, without anybody's addresses on it.
type wire struct {
	name    string
	project string
	managed bool

	prefixes []netip.Prefix
}

// key identifies an instance, and a wire, by the project that owns it.
//
// Every bridge lives in the default project and other projects reference it, so
// a bridge two projects both use is one entry. The owner has to be part of it
// because two projects with features.networks may each own the same name.
func key(project, name string) string { return project + "/" + name }

// putWires replaces every network in one read. A whole listing is the only
// answer that can say a network has gone.
func (m *model) putWires(networks []incusapi.Network) {
	m.wires = make(map[string]wire, len(networks))

	for _, n := range networks {
		m.wires[key(n.Project, n.Name)] = newWire(n)
	}
}

// putWire patches one network in.
func (m *model) putWire(n incusapi.Network) {
	m.wires[key(n.Project, n.Name)] = newWire(n)
}

// dropWire removes one network. The instances on it keep their addresses: what
// went is the wire, and the next read of each says what that means for it.
func (m *model) dropWire(project, name string) {
	delete(m.wires, key(project, name))
}

// putProject records a project's own configuration, as it was read.
func (m *model) putProject(project string, config map[string]string) {
	m.projects[project] = config
}

// dropProject removes a project and everything in it. A project cannot be
// deleted while it holds instances, but a project we stop serving takes its
// instances out of the model with it either way.
func (m *model) dropProject(project string) {
	delete(m.projects, project)

	prefix := project + "/"

	for k := range m.instances {
		if strings.HasPrefix(k, prefix) {
			delete(m.instances, k)
		}
	}
}

// putInstance distills one instance read and patches it in.
func (m *model) putInstance(inst *incusapi.Instance, state *incusapi.InstanceState) *instance {
	e := &instance{
		running: inst.StatusCode == incusapi.Running,
		nets:    m.addressesByNetwork(inst, state),
	}

	config := inst.Config
	if len(inst.ExpandedConfig) > 0 {
		config = inst.ExpandedConfig
	}

	e.config = map[string]string{}
	for k, v := range config {
		if !strings.HasPrefix(k, "volatile.") {
			e.config[k] = v
		}
	}

	m.instances[key(inst.Project, inst.Name)] = e

	return e
}

// markDirty notes that a read of one instance failed, so what is held for it is
// older than the event that asked.
func (m *model) markDirty(project, name string) {
	m.dirty[key(project, name)] = true
}

// dropInstance removes one instance. This is what a delete does, and half of
// what a rename does - the other half is a read of the new name.
func (m *model) dropInstance(project, name string) {
	k := key(project, name)

	delete(m.instances, k)

	// Gone is not stale. Nothing is owed a re-read of something that no longer
	// exists, and leaving it noted would pull a pass in for nothing.
	delete(m.dirty, k)
}

// instance returns what is held for one instance, or nil.
func (m *model) instance(project, name string) *instance {
	return m.instances[key(project, name)]
}

// instancesOn is every instance the model holds that sits on one network.
//
// What a network change fans out over. A wire is iutil, so one subnet moving
// changes every record on it - and this answers which without a read, because
// the addresses were already grouped by wire when each instance was distilled.
func (m *model) instancesOn(wire string) []subject {
	var out []subject

	for k, e := range m.instances {
		_, on := e.nets[wire]
		if !on {
			continue
		}

		project, name, found := strings.Cut(k, "/")
		if !found {
			continue
		}

		out = append(out, subject{project: project, name: name})
	}

	return out
}

// subject is one instance to re-read, named the way an event names it.
type subject struct {
	project string
	name    string
}

// instancesIn is every instance name the model holds for one project.
//
// What a profile change fans out over: a profile re-expands the configuration
// of every instance using it, so each needs a read of its own - and this is how
// the enricher knows which, without reading the project to find out.
func (m *model) instancesIn(project string) []subject {
	prefix := project + "/"

	var out []subject

	for k := range m.instances {
		name, ok := strings.CutPrefix(k, prefix)
		if ok {
			out = append(out, subject{project: project, name: name})
		}
	}

	return out
}

// newWire distills one network down to what a record needs of it.
//
// Incus reports the owning project on every listing, including for a network a
// project merely references, so the owner is known here and never has to be
// looked up in a map that could be older than the network.
func newWire(n incusapi.Network) wire {
	w := wire{name: n.Name, project: n.Project, managed: n.Managed}

	// An unmanaged network has no subnet Incus knows about, so it identifies no
	// querier. It still keys records: two instances on one unmanaged wire can
	// reach each other, and that is what a iutil key means.
	if !n.Managed {
		return w
	}

	for _, k := range []string{"ipv4.address", "ipv6.address"} {
		// Incus stores the gateway address with the prefix length
		// ("10.37.154.1/24"), and the sentinels "none" and "auto" when there is
		// no usable subnet.
		raw := n.Config[k]
		if raw == "" || raw == "none" || raw == "auto" {
			continue
		}

		prefix, err := netip.ParsePrefix(raw)
		if err != nil {
			continue
		}

		w.prefixes = append(w.prefixes, prefix.Masked())
	}

	return w
}

// addressesByNetwork groups an instance's global addresses by the network each
// interface is attached to.
//
// Devices and state share the device name as key; interface naming is not
// stable, so it is never assumed. A NIC whose network the model does not know
// is skipped rather than keyed under an empty owner - two projects whose
// private networks share a bare name would otherwise collapse into one.
func (m *model) addressesByNetwork(
	inst *incusapi.Instance,
	state *incusapi.InstanceState,
) map[string]*iutil.Network {
	if state == nil {
		return nil
	}

	// Prefer the expanded devices, so NICs supplied by a profile are seen, and
	// fall back to the local ones. A compose stack puts most of its NICs on a
	// profile, so the local devices alone would see almost none of them.
	devices := inst.ExpandedDevices
	if len(devices) == 0 {
		devices = inst.Devices
	}

	type addrs struct {
		ipv4 []netip.Addr
		ipv6 []netip.Addr
	}

	found := map[string]addrs{}
	wires := map[string]wire{}

	for device, iface := range state.Network {
		if iface.Type == "loopback" {
			continue
		}

		network := devices[device]["network"]
		if network == "" {
			continue
		}

		// A NIC names its network bare, and that name resolves in the instance's
		// own project first and the default project's after - which is what
		// Incus does for a project without features.networks.
		w, known := m.wires[key(inst.Project, network)]
		if !known {
			w, known = m.wires[key(defaultProject, network)]
		}

		if !known {
			continue
		}

		k := key(w.project, w.name)
		wires[k] = w
		a := found[k]

		for _, address := range iface.Addresses {
			if address.Scope != "global" {
				continue
			}

			ip, err := netip.ParseAddr(address.Address)
			if err != nil {
				continue
			}

			if ip.Is4() {
				a.ipv4 = append(a.ipv4, ip)

				continue
			}

			a.ipv6 = append(a.ipv6, ip)
		}

		found[k] = a
	}

	if len(found) == 0 {
		return nil
	}

	out := make(map[string]*iutil.Network, len(found))

	for k, a := range found {
		w := wires[k]
		// The prefixes are cloned because the wire outlives the event and is
		// patched in place by the next network read. Handing the same slice to
		// every instance on it would let one read rewrite what an event already
		// carries.
		out[k] = iutil.NewNetwork(w.name, w.project, w.managed, slices.Clone(w.prefixes), a.ipv4, a.ipv6)
	}

	return out
}
