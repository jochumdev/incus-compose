package dns

import (
	"net/netip"
	"slices"
	"sort"

	incusutil "github.com/lxc/incus/v7/shared/util"
	"github.com/miekg/dns"

	"github.com/lxc/incus-compose/ievent/dns/ecs_view"
	"github.com/lxc/incus-compose/ievent/iutil"
)

// instance is one instance as this plugin holds it: everything a record needs
// out of the event the enricher handed over, and nothing else.
type instance struct {
	// Event is what the enricher handed over. Held whole, so what changed is the
	// comparison it already answers rather than a second copy of its fields.
	*iutil.Event

	zone string
	meta map[string]string

	// ns is the zone's NS names, absolute. LabelProject is what keeps an
	// instance from naming them.
	ns []string

	// transfer is this instance's project opting its zone into transfer.
	transfer bool

	// nets is every network this instance sits on, by NetworkKey. The value
	// carries the network's own subnets and this instance's addresses on it
	// together, so neither is looked up twice.
	nets map[string]*netOn

	// names is every forward name this instance answers to: its own, and the
	// service name its replicas share. First is its own, which a PTR answers with.
	names []string

	// alias is every name it claims as a CNAME onto its own.
	alias []string

	// ptrs is every reverse name it answers to, and where each sits.
	ptrs map[string]ptrOn
}

// netOn is one network this instance sits on: what it holds there, and what the
// network itself serves. The addresses are the instance's; the prefixes are the
// network's, and place a querier that is not one of ours.
type netOn struct {
	ipv4 []netip.Addr
	ipv6 []netip.Addr

	prefixes []netip.Prefix
}

// sitsOn is where one instance sits, read off what the enricher handed over.
// The interfaces say where, the networks say what each place is.
func sitsOn(inst *iutil.Instance) map[string]*netOn {
	out := map[string]*netOn{}

	for iface := range inst.Interfaces() {
		key := iutil.NetworkKey(iface.Project(), iface.Network())

		on, seen := out[key]
		if !seen {
			on = &netOn{prefixes: prefixesOf(inst.Network(key))}
			out[key] = on
		}

		on.ipv4 = append(on.ipv4, addrs(iface.IPv4())...)
		on.ipv6 = append(on.ipv6, addrs(iface.IPv6())...)
	}

	return out
}

// prefixesOf is the subnets one network serves. Unparseable is skipped rather
// than fatal: a network with none covers nothing, which is what Incus reports
// for one it does not manage.
func prefixesOf(net *iutil.Network) []netip.Prefix {
	var out []netip.Prefix

	for _, subnet := range []string{net.IPv4(), net.IPv6()} {
		if subnet == "" {
			continue
		}

		prefix, err := netip.ParsePrefix(subnet)
		if err != nil {
			continue
		}

		out = append(out, prefix.Masked())
	}

	return out
}

// addrs parses what one NIC holds. Anything unparseable is skipped: an address
// we cannot read is one we cannot answer with either.
func addrs(list []string) []netip.Addr {
	out := make([]netip.Addr, 0, len(list))

	for _, held := range list {
		addr, err := netip.ParseAddr(held)
		if err != nil {
			continue
		}

		out = append(out, addr)
	}

	return out
}

// ptrOn is the network one reverse name is reachable on, and the zone holding
// it. Both come off the address, so neither is worked out twice.
type ptrOn struct {
	key  string
	zone string
}

// zoneSerial is what a zone was published under. Serial persists; what a zone
// is made of is rebuilt by folding, so the rest is unexported and the cold
// store never sees it.
type zoneSerial struct {
	Serial uint32

	// dirty is a name in this zone having moved since it was last published. A
	// serial step is a claim that the zone changed, so only a patch sets it.
	dirty bool

	// names is how many names the zone holds, so the last one leaving takes the
	// zone with it; hosts is how many of those are a host's rather than an alias
	// that landed here, so a zone with none is Shadowing.
	names int
	hosts int

	// transfers is how many names in this zone came from a project that opted
	// it in, so the last one leaving closes the zone again.
	transfers int

	// ns is every name server named for this zone and how many instances name
	// it: two projects sharing a zone name are one zone, so their servers are
	// unioned rather than agreed on.
	ns map[string]int
}

// patchInstance turns one enriched event into what is held, or nil if no
// networks were read.
func patchInstance(ev *iutil.Event, suffix string) *instance {
	if !ev.Enriched(iutil.EnrichedInstanceWithInterfaces) {
		return nil
	}

	nets := sitsOn(ev.Instance())

	own := meta(ev)
	zone := zoneFor(ev.ProjectName(), own, suffix)

	inst := &instance{
		Event:    ev,
		zone:     zone,
		meta:     own,
		ns:       relativeNames(own[metaNS], zone),
		transfer: incusutil.IsTrue(own[metaTransfer]),
		nets:     nets,
		names:    []string{dns.CanonicalName(ev.Name()) + zone},
		alias:    aliasNames(own, zone),
	}

	// A scaled service's replicas land under one record.
	service := own[metaService]
	if service != "" {
		inst.names = append(inst.names, dns.CanonicalName(service)+zone)
	}

	for key, net := range nets {
		for _, addr := range slices.Concat(net.ipv4, net.ipv6) {
			if !covered(net.prefixes, addr) {
				continue
			}

			name, zone, ok := reverseName(addr)
			if !ok {
				continue
			}

			if inst.ptrs == nil {
				inst.ptrs = map[string]ptrOn{}
			}

			inst.ptrs[name] = ptrOn{key: key, zone: zone}
		}
	}

	return inst
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

// zone renders what a denial needs, once, so answering synthesizes nothing.
//
// Shadowing while no host name is in it: it claims the aliases that landed here
// and falls through for the rest of the domain.
func zone(name string, held zoneSerial, ns []string, ttl uint32) *ecs_view.Zone {
	return &ecs_view.Zone{
		Name:      name,
		SOA:       soa(name, ns, held.Serial, ttl),
		NS:        nsRecords(name, ns, ttl),
		Shadowing: held.hosts == 0,
		Transfer:  held.transfers > 0,
	}
}

// heldKey is how an instance is held: by project and name, never by name alone.
func heldKey(project, name string) string { return project + "/" + name }

// netKeys is the networks an instance sits on, sorted so any two builds of one
// fleet name the same view.
func netKeys(inst *instance) []string {
	keys := make([]string, 0, len(inst.nets))
	for key := range inst.nets {
		keys = append(keys, key)
	}

	sort.Strings(keys)

	return keys
}

// soa is the zone's SOA, with the zone's own serial. MNAME is the first of the
// operator's NS names, so it always names a server in the set.
func soa(zoneName string, ns []string, serial, ttl uint32) dns.RR {
	if len(ns) == 0 {
		ns = synthesizedNS(zoneName)
	}

	return &dns.SOA{
		Hdr:     dns.RR_Header{Name: zoneName, Rrtype: dns.TypeSOA, Class: dns.ClassINET, Ttl: ttl},
		Ns:      ns[0],
		Mbox:    "hostmaster." + zoneName,
		Serial:  serial,
		Refresh: 7200,
		Retry:   1800,
		Expire:  86400,
		Minttl:  ttl,
	}
}

// nsRecords is the zone's own NS set, one record per name. ns is the operator's
// list; empty falls back to the synthesized name.
func nsRecords(zoneName string, ns []string, ttl uint32) []dns.RR {
	if len(ns) == 0 {
		ns = synthesizedNS(zoneName)
	}

	out := make([]dns.RR, len(ns))
	for i, name := range ns {
		out[i] = &dns.NS{
			Hdr: dns.RR_Header{Name: zoneName, Rrtype: dns.TypeNS, Class: dns.ClassINET, Ttl: ttl},
			Ns:  name,
		}
	}

	return out
}

// synthesizedNS is served for a zone no operator has named servers for.
func synthesizedNS(zoneName string) []string { return []string{"ns.dns." + zoneName} }
