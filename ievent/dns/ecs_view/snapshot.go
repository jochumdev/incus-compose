// Package ecs_view serves DNS records filtered by who is asking. A querier is
// placed on a set of networks and sees only the names reachable there.
package ecs_view

import (
	"net/netip"
	"slices"
	"sort"
	"strings"

	iradix "github.com/hashicorp/go-immutable-radix/v2"
	"github.com/koji-hirono/go-critbit"
	"github.com/miekg/dns"
)

// maxName is the longest a DNS name can be on the wire, which is what lets a key
// be reversed into a stack buffer rather than allocated per query.
const maxName = 255

// ViewID names a set of networks, which is all the answer to a query depends on.
type ViewID string

// AmbiguousView is stored where two things claim one address or one prefix.
// Refusing beats picking: the loser's records would answer the wrong querier.
const AmbiguousView ViewID = "\x00ambiguous"

// Records is one name's records for one view, by query type. The grouping is
// what keeps NXDOMAIN and NODATA apart.
type Records map[uint16][]dns.RR

// ViewAnswer is one name's records, by view then by query type. A view is the
// set of networks a querier sits on, so authorization is the outer key here
// rather than an intersection taken per query.
type ViewAnswer map[ViewID]Records

// Zone is what a denial needs and nothing else. SOA and NS are rendered once at
// build time, so answering synthesizes nothing.
type Zone struct {
	Name string
	SOA  dns.RR
	NS   []dns.RR

	// Shadowing marks a zone invented for an alias that landed outside every
	// served zone: it claims the names it holds and falls through for the rest.
	Shadowing bool

	// Transfer opts this zone into AXFR and IXFR. The serial a secondary keys
	// off is the one on SOA, so there is no second copy to disagree with it.
	Transfer bool
}

// Snapshot is an immutable view of every record. Updates build a new one.
type Snapshot struct {
	// Tree is every name, keyed root-first so a zone is a prefix of the names in
	// it.
	Tree *iradix.Tree[ViewAnswer]

	// Denial is the zone a name falls in, keyed the same way. One descent - zones
	// deep rather than names deep - gives a miss its authority section, or
	// nothing at all when the name is outside every zone.
	Denial *iradix.Tree[*Zone]

	// ByIPv4 and ByIPv6 place a querier. Addresses an instance claims go in at
	// /32 and /128, networks at their own prefix, so the longest match arbitrates
	// and an instance's own claim beats the subnet holding it.
	//
	// Two trees rather than one: sharing a key space, a v6 address beginning
	// 0a00:0100:... would match a stored 10.0.1.0/24 on the first 24 bits.
	ByIPv4 critbit.Tree[ViewID]
	ByIPv6 critbit.Tree[ViewID]
}

// ViewOf canonicalises a set of network keys, sorted so any order names the same
// view.
func ViewOf(keys []string) ViewID {
	if len(keys) == 0 {
		return ""
	}

	sorted := slices.Clone(keys)
	sort.Strings(sorted)

	// A separator no network key can contain, so two sets cannot collide.
	return ViewID(strings.Join(sorted, "\x00"))
}

// KeysOf is the networks a view spans. The ViewID is them joined, so nothing
// has to remember the set beside it.
func KeysOf(id ViewID) []string {
	if id == "" {
		return nil
	}

	return strings.Split(string(id), "\x00")
}

// ZoneOf returns the zone qname falls in, or nil when it falls outside every one.
func (s *Snapshot) ZoneOf(qname string) *Zone {
	var buf [maxName]byte

	_, z, ok := s.Denial.Root().LongestPrefix(nameKey(buf[:0], qname))
	if !ok {
		return nil
	}

	return z
}

// Answers returns every view's records for qname.
func (s *Snapshot) Answers(qname string) (ViewAnswer, bool) {
	var buf [maxName]byte

	return s.Tree.Root().Get(nameKey(buf[:0], qname))
}

// ViewFor places a querier on the narrowest network known to hold it, so an
// address no instance claims still lands on the subnet it sits in.
func (s *Snapshot) ViewFor(addr netip.Addr) (ViewID, bool) {
	if addr.Is4() {
		b := addr.As4()

		return s.ByIPv4.Longest(critbit.BitsKey(b[:], 32))
	}

	b := addr.As16()

	return s.ByIPv6.Longest(critbit.BitsKey(b[:], 128))
}

// AddrKey is how an address or a prefix is keyed, and which of the two trees it
// belongs in. A source indexes with this so both ends agree.
func AddrKey(prefix netip.Prefix) (key critbit.Key, v4 bool) {
	addr := prefix.Addr()

	if addr.Is4() {
		b := addr.As4()

		return critbit.BitsKey(b[:], prefix.Bits()), true
	}

	b := addr.As16()

	return critbit.BitsKey(b[:], prefix.Bits()), false
}

// NameKey is a name as the trees hold it. A source building one needs this;
// answering does not.
func NameKey(name string) []byte {
	return nameKey(make([]byte, 0, len(name)+1), name)
}

// nameKey appends name to dst with its labels reversed, root first, each still
// followed by a dot - so a zone is a byte-wise prefix of every name inside it.
// The trailing dot is load-bearing: without it "incus." would prefix "incusx.".
func nameKey(dst []byte, name string) []byte {
	end := len(name)
	if end > 0 && name[end-1] == '.' {
		end--
	}

	for end > 0 {
		start := strings.LastIndexByte(name[:end], '.') + 1

		dst = append(dst, name[start:end]...)
		dst = append(dst, '.')

		end = start - 1
	}

	return dst
}

// EmptySnapshot returns a usable snapshot with no records in it.
func EmptySnapshot() *Snapshot {
	return &Snapshot{
		Tree:   iradix.New[ViewAnswer](),
		Denial: iradix.New[*Zone](),
	}
}
