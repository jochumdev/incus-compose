package dns

import (
	"slices"
	"sort"
	"strings"

	"github.com/miekg/dns"
)

// labelSep separates the names in a comma-separated label: aliases, ns.
const labelSep = ","

// relativeNames splits a comma-separated label the way a zone file resolves
// names: a trailing dot is absolute, anything else relative to zone. Invalid
// names go, and repeats collapse - naming one twice is not a collision with
// itself.
func relativeNames(list, zone string) []string {
	if list == "" {
		return nil
	}

	var out []string

	for field := range strings.SplitSeq(list, labelSep) {
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

	sort.Strings(out)

	return slices.Compact(out)
}

// aliasNames reads one instance's aliases label.
func aliasNames(meta map[string]string, zone string) []string {
	return relativeNames(meta[metaAliases], zone)
}

// aliasZone is the zone an alias belongs to: the longest one served, otherwise
// the name's own parent, which is then a zone nobody asked for. A single label
// is refused.
func aliasZone(name string, served func(string) bool) (zone string, ok bool) {
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

		if served(name[i:]) {
			return name[i:], true
		}
	}

	if parent == "" {
		return "", false
	}

	return parent, true
}
