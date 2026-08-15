package dns

import (
	"hash/fnv"
	"net/netip"
	"slices"
	"sort"
	"strings"

	"github.com/miekg/dns"

	"github.com/lxc/incus-compose/ievent/dns/ecs_view"
)

// ptrEntry is one address's reverse record: the network it is reachable on, and
// the name it answers with. The key is what makes a PTR obey the forward rule.
type ptrEntry struct {
	key    string
	target string

	// transfer is the opt-in of the project the named instance is in. A reverse
	// zone belongs to no project, so it is transferable only if every instance
	// answering in it agrees.
	transfer bool
}

// v6HostLabels is how many nibble labels of an ip6.arpa name belong to the host
// rather than to its zone. Sixteen nibbles are the low 64 bits, so the zone is
// the address's /64 - which is what Incus gives a bridge.
const v6HostLabels = 16

// reverseName returns the PTR owner name for addr, and the zone holding it. The
// zone comes from the address, so a subnet holding no instance is never claimed.
func reverseName(addr netip.Addr) (string, string, bool) {
	// dns.ReverseAddr reads a 4-in-6 address as IPv4 and answers under
	// in-addr.arpa, so the family has to be settled before both of them.
	addr = addr.Unmap()

	name, err := dns.ReverseAddr(addr.String())
	if err != nil {
		return "", "", false
	}

	labels := v6HostLabels
	if addr.Is4() {
		labels = 1
	}

	zone := name

	for range labels {
		cut := strings.IndexByte(zone, '.')
		if cut < 0 {
			return "", "", false
		}

		zone = zone[cut+1:]
	}

	return name, zone, true
}

// addReverse files one network's addresses under the reverse names they answer
// to. An address outside every prefix of its network is served forward only:
// that subnet is somebody else's.
func addReverse(revs map[string]map[string][]ptrEntry, key, target string, transfer bool, list []netip.Addr, prefixes []netip.Prefix) {
	for _, addr := range list {
		if !covered(prefixes, addr) {
			continue
		}

		name, zone, ok := reverseName(addr)
		if !ok {
			continue
		}

		names := revs[zone]
		if names == nil {
			names = map[string][]ptrEntry{}
			revs[zone] = names
		}

		names[name] = append(names[name], ptrEntry{key: key, target: target, transfer: transfer})
	}
}

// covered reports whether a network's own addressing reaches addr. A network
// with no prefixes covers nothing, which is what Incus reports for one it does
// not manage.
func covered(prefixes []netip.Prefix, addr netip.Addr) bool {
	for _, prefix := range prefixes {
		if prefix.Contains(addr) {
			return true
		}
	}

	return false
}

// reverseZones renders a zone per /24 and /64 that holds an address, keyed by
// network exactly as the forward records are so the same Gather reaches them.
func reverseZones(
	revs map[string]map[string][]ptrEntry,
	prev *ecs_view.Snapshot,
	ttl uint32,
) map[string]*ecs_view.Zone {
	out := make(map[string]*ecs_view.Zone, len(revs))

	for zoneName, names := range revs {
		z := &ecs_view.Zone{
			Names:    make(map[string]map[string]ecs_view.RRSets, len(names)),
			Hash:     hashPTR(names),
			Transfer: true,
		}
		z.Serial = nextSerial(prev, zoneName, z.Hash)

		for name, entries := range names {
			byKey := map[string][]string{}
			for _, e := range entries {
				byKey[e.key] = append(byKey[e.key], e.target)

				// One instance that did not opt in closes the whole reverse
				// zone: it is iutil, and nothing else says whose it is.
				z.Transfer = z.Transfer && e.transfer
			}

			rendered := make(map[string]ecs_view.RRSets, len(byKey))

			for key, targets := range byKey {
				sort.Strings(targets)

				rendered[key] = ecs_view.RenderPTR(name, slices.Compact(targets), ttl)
			}

			z.Names[name] = rendered
		}

		out[zoneName] = z
	}

	return out
}

// hashPTR digests one reverse zone over the entries rather than the rendered
// records, so a serial notices a change in who an address is answered to.
func hashPTR(names map[string][]ptrEntry) uint64 {
	sorted := make([]string, 0, len(names))
	for name := range names {
		sorted = append(sorted, name)
	}

	sort.Strings(sorted)

	h := fnv.New64a()

	items := make([]string, 0, 16)

	for _, name := range sorted {
		_, _ = h.Write([]byte(name))

		items = items[:0]
		for _, e := range names[name] {
			items = append(items, e.key+"\x00"+e.target)
		}

		sort.Strings(items)

		for _, item := range items {
			_, _ = h.Write([]byte(item))
		}
	}

	return h.Sum64()
}
