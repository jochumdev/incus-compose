package dns

import (
	"net/netip"
	"strings"

	"github.com/miekg/dns"
)

// v6HostLabels is how many nibble labels of an ip6.arpa name belong to the host
// rather than its zone: 16 nibbles are the low 64 bits, the /64 Incus gives a bridge.
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

// covered reports whether a network's own addressing reaches addr. A network
// with no prefixes covers nothing - what Incus reports for one it does not manage.
func covered(prefixes []netip.Prefix, addr netip.Addr) bool {
	for _, prefix := range prefixes {
		if prefix.Contains(addr) {
			return true
		}
	}

	return false
}
