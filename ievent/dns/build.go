package dns

import (
	"hash/fnv"
	"maps"
	"net/netip"
	"slices"
	"sort"

	incusutil "github.com/lxc/incus/v7/shared/util"
	"github.com/miekg/dns"

	"github.com/lxc/incus-compose/ievent/dns/ecs_view"
	"github.com/lxc/incus-compose/ievent/shared"
)

// instance is one instance as this plugin holds it: everything a record needs
// out of the event the enricher handed over, and nothing else.
type instance struct {
	zone string
	meta map[string]string

	// project is the project's own labels as they were last actually read, kept
	// apart from meta because most actions arrive without them. See distill.
	project map[string]string

	// transfer says the project opted its zone into zone transfer. Fed from
	// project and from nothing else, which is what keeps an instance from
	// exposing every sibling it shares a zone with.
	transfer bool

	// nets is every network this instance sits on, keyed by Network.Key. The
	// value carries the wire and this instance's addresses on it together.
	nets map[string]*shared.Network
}

// distill turns one enriched event into what is held. Nil when no networks were
// read: an instance that cannot be placed on a wire has no record to serve.
//
// prev is this instance as it was last held, under whatever name it had then,
// or nil. Only a read that reached the project may replace its labels: the
// enricher leaves them off an action nobody asked for them on, and a map that
// was never read is not a project that sets nothing.
func distill(ev *shared.Event, prev *instance, suffix string) *instance {
	if !ev.Enriched(shared.EnrichedNetworks) {
		return nil
	}

	nets := maps.Collect(ev.Networks())

	meta := instanceLabels(ev)

	var project map[string]string

	switch {
	case ev.Enriched(shared.EnrichedProject):
		project = projectLabels(ev)
	case prev != nil:
		project = prev.project
	}

	// The project's settings under the instance's, so a project label is a
	// default its instances override. Transfer is not one of them: it is the
	// project's alone, and instanceLabels has already dropped it.
	for key, value := range project {
		if key == metaTransfer {
			continue
		}

		_, own := meta[key]
		if own {
			continue
		}

		if meta == nil {
			meta = map[string]string{}
		}

		meta[key] = value
	}

	return &instance{
		zone:     zoneFor(ev.Project(), meta, suffix),
		meta:     meta,
		project:  project,
		transfer: transferable(project),
		nets:     nets,
	}
}

// transferable reports whether a project's own labels opt its zone into zone
// transfer. Two callers, and which key and which spelling of true mean yes may
// not drift between the fold and what is read back off disk.
func transferable(project map[string]string) bool {
	return incusutil.IsTrue(project[metaTransfer])
}

// zoneFor returns the zone a project serves. Its own labels may override the
// name; otherwise it is <project>.<suffix>.
func zoneFor(project string, meta map[string]string, suffix string) string {
	override := meta[metaZone]
	if override != "" {
		return dns.CanonicalName(override)
	}

	return dns.CanonicalName(project + "." + suffix)
}

// build derives every record from everything held. Fleet-wide, never per
// project: a view is a set of network keys, and a set can span projects.
//
// It mutates nothing, reads no clock and does no I/O, and nothing it returns
// aliases what it read - so a published snapshot is safe while the fold goes on.
func build(held map[string]*instance, prev *ecs_view.Snapshot, ttl uint32) *ecs_view.Snapshot {
	snap := &ecs_view.Snapshot{
		ByZone: map[string]*ecs_view.Zone{},
		ByAddr: map[netip.Addr]ecs_view.ViewID{},
		Views:  map[ecs_view.ViewID]map[string]ecs_view.RRSets{},
		Nets:   subnets(held),
		TTL:    ttl,
	}

	// Every zone's names, and the instances answering to each. Two projects
	// resolving to one zone name really are one zone.
	hosts := map[string]map[string][]*instance{}

	// The reverse of the same thing: zone, then name, then what answers there.
	revs := map[string]map[string][]ptrEntry{}

	// A zone may be handed over whole only if every instance in it says so. Two
	// projects can resolve to one zone name, and one that did not opt in closes
	// the zone for both.
	transfers := map[string]bool{}

	for key, inst := range held {
		names := hosts[inst.zone]
		if names == nil {
			names = map[string][]*instance{}
			hosts[inst.zone] = names
			transfers[inst.zone] = true
		}

		transfers[inst.zone] = transfers[inst.zone] && inst.transfer

		host := dns.CanonicalName(nameOf(key)) + inst.zone

		names[host] = append(names[host], inst)

		// A scaled service's replicas land under one record.
		service := inst.meta[metaService]
		if service != "" {
			svc := dns.CanonicalName(service) + inst.zone
			names[svc] = append(names[svc], inst)
		}

		// An instance queries from the networks it sits on, so every address of
		// its own resolves to one view.
		id, _ := viewOf(inst)

		for netKey, net := range inst.nets {
			indexAddrs(snap, net.IPv4(), id)
			indexAddrs(snap, net.IPv6(), id)

			// The instance name alone: a reverse lookup wants the one name that
			// names this host and no other.
			addReverse(revs, netKey, host, inst.transfer, net.IPv4(), net.Prefixes())
			addReverse(revs, netKey, host, inst.transfer, net.IPv6(), net.Prefixes())
		}
	}

	// Aliases once every host name is known, since a name the fleet already
	// answers to is not one an alias may take.
	aliases := aliasRecords(held, hosts)

	for zoneName, names := range hosts {
		// Transfer stays out of the hash: flipping the label changes who may
		// take the zone, not what is in it, and a secondary already holding it
		// wants a refusal rather than a fresh serial.
		z := &ecs_view.Zone{
			Hash:     hashHosts(names, aliases[zoneName]),
			Transfer: transfers[zoneName],
		}
		z.Serial = nextSerial(prev, zoneName, z.Hash)

		byNetwork(z, names, ttl)

		snap.ByZone[zoneName] = z
	}

	// A zone per absolute alias that landed outside all of them, then the
	// aliases themselves - which need the host names rendered first.
	aliasZones(snap, prev, aliases)
	renderAliases(snap, aliases, ttl)

	for zoneName, z := range reverseZones(revs, prev, ttl) {
		// A forward zone of the same name is the one somebody asked for.
		_, taken := snap.ByZone[zoneName]
		if taken {
			continue
		}

		snap.ByZone[zoneName] = z
	}

	// Reverse zones are in place, and byView gathers over every zone there is.
	byView(snap, hosts)

	return snap
}

// nameOf is the instance name out of a held key, which carries the project too
// because two projects may each have a web.
func nameOf(key string) string {
	_, name, found := cut(key)
	if !found {
		return key
	}

	return name
}

// cut splits a held key into its project and name.
func cut(key string) (project, name string, found bool) {
	for i := range len(key) {
		if key[i] == '/' {
			return key[:i], key[i+1:], true
		}
	}

	return "", key, false
}

// heldKey is how an instance is held: by project and name, never by name alone.
func heldKey(project, name string) string { return project + "/" + name }

// nextSerial carries a zone's serial forward when its records are unchanged and
// steps it when they are not. A new zone starts at 1.
func nextSerial(prev *ecs_view.Snapshot, name string, hash uint64) uint32 {
	if prev == nil {
		return 1
	}

	old, existed := prev.ByZone[name]
	if !existed {
		return 1
	}

	if old.Hash == hash {
		return old.Serial
	}

	return old.Serial + 1
}

// viewOf names the set of networks an instance sits on.
func viewOf(inst *instance) (ecs_view.ViewID, []string) {
	keys := make([]string, 0, len(inst.nets))
	for key := range inst.nets {
		keys = append(keys, key)
	}

	sort.Strings(keys)

	return ecs_view.ViewOf(keys), keys
}

// indexAddrs makes every address resolve to the view its owner queries from.
// Two projects on overlapping subnets can claim one, so a clash is ambiguous
// rather than decided by map order.
func indexAddrs(snap *ecs_view.Snapshot, list []netip.Addr, id ecs_view.ViewID) {
	for _, addr := range list {
		held, taken := snap.ByAddr[addr]
		if taken && held != id {
			snap.ByAddr[addr] = ecs_view.AmbiguousView

			continue
		}

		snap.ByAddr[addr] = id
	}
}

// subnets maps every network's prefixes to its key. Duplicates go: a shared
// network is listed by each project referencing it, and LookupNet needs one.
func subnets(held map[string]*instance) []ecs_view.NetEntry {
	var entries []ecs_view.NetEntry

	seen := map[ecs_view.NetEntry]struct{}{}

	for _, inst := range held {
		for key, net := range inst.nets {
			for _, prefix := range net.Prefixes() {
				entry := ecs_view.NetEntry{Prefix: prefix, Key: key}

				_, dup := seen[entry]
				if dup {
					continue
				}

				seen[entry] = struct{}{}
				entries = append(entries, entry)
			}
		}
	}

	return entries
}

// hashHosts digests one zone from the instances rather than the rendered
// records, so it notices a change in reachability and not only in addresses.
//
// The zone's aliases go into the same digest with their target and networks,
// for the same reason. Everything is sorted first, since the maps are not.
func hashHosts(names map[string][]*instance, aliases map[string]*aliasRecord) uint64 {
	sorted := make([]string, 0, len(names))
	for name := range names {
		sorted = append(sorted, name)
	}

	sort.Strings(sorted)

	h := fnv.New64a()

	items := make([]string, 0, 16)
	buf := make([]byte, 0, 32)

	for _, name := range sorted {
		_, _ = h.Write([]byte(name))

		items = items[:0]

		for _, inst := range names[name] {
			for key, net := range inst.nets {
				for _, addr := range append(net.IPv4(), net.IPv6()...) {
					buf = addr.AppendTo(buf[:0])
					items = append(items, key+"\x00"+string(buf))
				}
			}
		}

		sort.Strings(items)

		for _, item := range items {
			_, _ = h.Write([]byte(item))
		}
	}

	sorted = sorted[:0]
	for name := range aliases {
		sorted = append(sorted, name)
	}

	sort.Strings(sorted)

	for _, name := range sorted {
		rec := aliases[name]

		_, _ = h.Write([]byte(name))
		_, _ = h.Write([]byte("\x00"))
		_, _ = h.Write([]byte(rec.target))

		for _, key := range rec.keys {
			_, _ = h.Write([]byte("\x00"))
			_, _ = h.Write([]byte(key))
		}
	}

	return h.Sum64()
}

// byNetwork renders every name's records once per network it is reachable on,
// so every view that can see it holds the same records by reference.
func byNetwork(z *ecs_view.Zone, names map[string][]*instance, ttl uint32) {
	z.Names = make(map[string]map[string]ecs_view.RRSets, len(names))

	for name, list := range names {
		type addrs struct {
			v4 []netip.Addr
			v6 []netip.Addr
		}

		perNet := map[string]addrs{}

		for _, inst := range list {
			for key, net := range inst.nets {
				seen := perNet[key]
				seen.v4 = append(seen.v4, net.IPv4()...)
				seen.v6 = append(seen.v6, net.IPv6()...)
				perNet[key] = seen
			}
		}

		rendered := make(map[string]ecs_view.RRSets, len(perNet))
		for key, seen := range perNet {
			// Sorted, so two builds of one fleet render identical records.
			slices.SortFunc(seen.v4, netip.Addr.Compare)
			slices.SortFunc(seen.v6, netip.Addr.Compare)

			rendered[key] = ecs_view.Render(name, seen.v4, seen.v6, ttl)
		}

		z.Names[name] = rendered
	}
}

// byView precomputes, per set of networks some instance sits on, the answer
// every name has when seen from there - so a query is a lookup, not a gather.
//
// Collected over the whole fleet and gathered against every zone, which is what
// makes a set spanning a project boundary hold the names on both sides.
func byView(snap *ecs_view.Snapshot, hosts map[string]map[string][]*instance) {
	sets := map[ecs_view.ViewID][]string{}

	for _, names := range hosts {
		for _, list := range names {
			for _, inst := range list {
				id, keys := viewOf(inst)
				sets[id] = keys
			}
		}
	}

	snap.Views = make(map[ecs_view.ViewID]map[string]ecs_view.RRSets, len(sets))

	for id, keys := range sets {
		visible := map[string]ecs_view.RRSets{}

		for _, z := range snap.ByZone {
			for name, perNet := range z.Names {
				// Absent means invisible, which the query path reports as
				// NXDOMAIN exactly like a name that does not exist.
				gathered, reachable := ecs_view.Gather(perNet, keys)
				if !reachable {
					continue
				}

				visible[name] = gathered
			}
		}

		snap.Views[id] = visible
	}
}
