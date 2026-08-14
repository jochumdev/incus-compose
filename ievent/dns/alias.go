package dns

import (
	"slices"
	"sort"
	"strings"

	"github.com/miekg/dns"

	"github.com/lxc/incus-compose/ievent/dns/ecs_view"
)

// aliasSep separates the names in one instance's aliases label.
const aliasSep = ","

// aliasRecord is one extra name an instance answers to. A CNAME onto the
// instance's own name, so a host that changes address changes it in one place.
type aliasRecord struct {
	name string

	// target is the instance's own name and targetZone the zone holding it. Apart
	// because an absolute alias may live in a different zone from its host.
	target     string
	targetZone string

	// keys is every network the aliasing instance sits on, sorted, so the alias
	// is reachable exactly where the instance is.
	keys []string
}

// aliasNames reads one instance's aliases label. A name ending in a dot is
// absolute, anything else relative to the instance's zone; invalid ones go.
func aliasNames(meta map[string]string, zone string) []string {
	list := meta[metaAliases]
	if list == "" {
		return nil
	}

	var out []string

	for field := range strings.SplitSeq(list, aliasSep) {
		field = strings.TrimSpace(field)
		if field == "" {
			continue
		}

		name := dns.CanonicalName(field)
		if !strings.HasSuffix(field, ".") {
			name += zone
		}

		_, ok := dns.IsDomainName(name)
		if !ok {
			continue
		}

		out = append(out, name)
	}

	// Sorted so repeats are adjacent: one instance naming an alias twice claims
	// it once, which is not a collision with itself.
	sort.Strings(out)

	return slices.Compact(out)
}

// aliasRecords collects every alias the fleet declares, by zone and name. Called
// once hosts is whole, since a collision is decided against every name served.
//
// Three drop the alias rather than pick a winner: a name a host or service
// already answers to, a name two instances claim, and a zone apex.
func aliasRecords(
	held map[string]*instance,
	hosts map[string]map[string][]*instance,
) map[string]map[string]*aliasRecord {
	// Every name the forward zones already answer to, and every apex.
	taken := map[string]struct{}{}

	for zoneName, names := range hosts {
		taken[zoneName] = struct{}{}

		for name := range names {
			taken[name] = struct{}{}
		}
	}

	// Claimants per name, so a contested alias is dropped whole rather than
	// settled by whichever instance the map walked to first.
	claims := map[string][]*aliasRecord{}

	for key, inst := range held {
		_, keys := viewOf(inst)

		target := dns.CanonicalName(nameOf(key)) + inst.zone

		for _, name := range aliasNames(inst.meta, inst.zone) {
			_, clash := taken[name]
			if clash {
				continue
			}

			claims[name] = append(claims[name], &aliasRecord{
				name:       name,
				target:     target,
				targetZone: inst.zone,
				keys:       keys,
			})
		}
	}

	out := map[string]map[string]*aliasRecord{}

	for name, list := range claims {
		if len(list) > 1 {
			continue
		}

		zoneName, ok := aliasZone(name, hosts)
		if !ok {
			continue
		}

		names := out[zoneName]
		if names == nil {
			names = map[string]*aliasRecord{}
			out[zoneName] = names
		}

		names[name] = list[0]
	}

	return out
}

// aliasZone is the zone an alias belongs to: the longest one the fleet serves,
// otherwise the name's own parent. A single label is refused.
func aliasZone(name string, zones map[string]map[string][]*instance) (string, bool) {
	parent := ""

	for i := 0; i < len(name); {
		cut := strings.IndexByte(name[i:], '.')
		if cut < 0 {
			break
		}

		i += cut + 1
		if i >= len(name) {
			break
		}

		if parent == "" {
			parent = name[i:]
		}

		_, served := zones[name[i:]]
		if served {
			return name[i:], true
		}
	}

	if parent == "" {
		return "", false
	}

	return parent, true
}

// aliasZones adds a zone for every alias landing outside all of them, marked
// Fallthrough so it claims the aliased names and nothing else in that domain.
func aliasZones(snap, prev *ecs_view.Snapshot, aliases map[string]map[string]*aliasRecord) {
	for zoneName, names := range aliases {
		_, served := snap.ByZone[zoneName]
		if served {
			continue
		}

		z := &ecs_view.Zone{
			Names:       make(map[string]map[string]ecs_view.RRSets, len(names)),
			Hash:        hashHosts(nil, names),
			Fallthrough: true,
		}
		z.Serial = nextSerial(prev, zoneName, z.Hash)

		snap.ByZone[zoneName] = z
	}
}

// renderAliases writes every alias into the zone holding it, last, from the
// target's own rendered records so the two stay in step per network.
func renderAliases(snap *ecs_view.Snapshot, aliases map[string]map[string]*aliasRecord, ttl uint32) {
	for zoneName, names := range aliases {
		z := snap.ByZone[zoneName]

		for name, rec := range names {
			target := snap.ByZone[rec.targetZone]

			z.Names[name] = ecs_view.RenderCName(name, rec.target, target.Names[rec.target], rec.keys, ttl)
		}
	}
}
