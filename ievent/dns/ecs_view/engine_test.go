package ecs_view

import (
	"context"
	"net"
	"net/netip"
	"sort"
	"strings"
	"testing"

	"github.com/coredns/coredns/plugin/pkg/dnstest"
	"github.com/coredns/coredns/plugin/test"
	"github.com/miekg/dns"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testTTL = 5

// pieceOf builds one contributor's snapshot from a literal topology: a name, the
// hosts answering to it, and each host's addresses per network key.
//
// It wires the exported helpers rather than reimplementing them, so what a test
// exercises is the production code and not a second copy of it.
func pieceOf(zoneName string, ttl uint32, hosts map[string][]map[string][]string, nets []NetEntry) *Snapshot {
	z := &Zone{Names: map[string]map[string]RRSets{}}

	snap := &Snapshot{
		ByZone: map[string]*Zone{zoneName: z},
		ByAddr: map[netip.Addr]ViewID{},
		Views:  map[ViewID]map[string]RRSets{},
		Nets:   nets,
		TTL:    ttl,
	}

	// Every distinct set of networks a host sits on becomes a view.
	sets := map[ViewID][]string{}

	for name, replicas := range hosts {
		perNetV4 := map[string][]netip.Addr{}
		perNetV6 := map[string][]netip.Addr{}

		for _, replica := range replicas {
			keys := make([]string, 0, len(replica))
			for key := range replica {
				keys = append(keys, key)
			}

			sort.Strings(keys)

			// A host queries from the networks it sits on, so all of its own
			// addresses resolve to the same view.
			id := ViewOf(keys)
			sets[id] = keys

			for _, key := range keys {
				for _, raw := range replica[key] {
					addr := netip.MustParseAddr(raw)
					if addr.Is4() {
						perNetV4[key] = append(perNetV4[key], addr)
					} else {
						perNetV6[key] = append(perNetV6[key], addr)
					}

					snap.ByAddr[addr] = id
				}
			}
		}

		rendered := map[string]RRSets{}
		for key := range perNetV4 {
			rendered[key] = Render(name, perNetV4[key], perNetV6[key], ttl)
		}

		for key := range perNetV6 {
			_, done := rendered[key]
			if !done {
				rendered[key] = Render(name, nil, perNetV6[key], ttl)
			}
		}

		z.Names[name] = rendered
	}

	for id, keys := range sets {
		names := map[string]RRSets{}

		for name, perNet := range z.Names {
			got, visible := Gather(perNet, keys)
			if !visible {
				continue
			}

			names[name] = got
		}

		snap.Views[id] = names
	}

	return snap
}

// testTopology is the worked example:
//
//	gateway       api-net
//	user-api      api-net, user-net
//	user-db       user-net
//	products-api  api-net, products-net
//	products-db   products-net
//
// The three on api-net see each other; user-db and products-db never do.
func testTopology() *Snapshot { return testPiece() }

// testPiece is that topology as one source publishes it, before folding.
func testPiece() *Snapshot { return testPieceTTL(testTTL) }

// testPieceTTL is testPiece rendered with a given TTL, for the stale clamp.
func testPieceTTL(ttl uint32) *Snapshot {
	return pieceOf("shop.incus.", ttl, map[string][]map[string][]string{
		"gateway.shop.incus.":      {{"api-net": {"10.0.1.10"}}},
		"user-api.shop.incus.":     {{"api-net": {"10.0.1.20"}, "user-net": {"10.0.2.20"}}},
		"user-db.shop.incus.":      {{"user-net": {"10.0.2.30"}}},
		"products-api.shop.incus.": {{"api-net": {"10.0.1.40"}, "products-net": {"10.0.3.40"}}},
		"products-db.shop.incus.":  {{"products-net": {"10.0.3.50"}}},
	}, []NetEntry{
		{Prefix: netip.MustParsePrefix("10.0.1.0/24"), Key: "api-net"},
		{Prefix: netip.MustParsePrefix("10.0.2.0/24"), Key: "user-net"},
		{Prefix: netip.MustParsePrefix("10.0.3.0/24"), Key: "products-net"},
	})
}

// addZone folds a zone into every view that can already see something, the way a
// source's build does. A ViewID is its sorted keys joined, so they recover here.
func addZone(snap *Snapshot, zoneName string, z *Zone) {
	snap.ByZone[zoneName] = z

	for id, names := range snap.Views {
		keys := strings.Split(string(id), "\x00")

		for name, perNet := range z.Names {
			got, visible := Gather(perNet, keys)
			if !visible {
				continue
			}

			names[name] = got
		}
	}
}

// reversePiece is the worked topology plus the reverse of user-api's two
// addresses, each keyed by the network it is reachable on - the source's doing.
func reversePiece() *Snapshot {
	snap := testPiece()

	addZone(snap, "1.0.10.in-addr.arpa.", &Zone{Serial: 1, Names: map[string]map[string]RRSets{
		"20.1.0.10.in-addr.arpa.": {
			"api-net": RenderPTR("20.1.0.10.in-addr.arpa.", []string{"user-api.shop.incus."}, testTTL),
		},
	}})

	addZone(snap, "2.0.10.in-addr.arpa.", &Zone{Serial: 1, Names: map[string]map[string]RRSets{
		"20.2.0.10.in-addr.arpa.": {
			"user-net": RenderPTR("20.2.0.10.in-addr.arpa.", []string{"user-api.shop.incus."}, testTTL),
		},
	}})

	return snap
}

func TestRenderPTRSharesOneArray(t *testing.T) {
	sets := RenderPTR("20.1.0.10.in-addr.arpa.", []string{"a.shop.incus.", "b.shop.incus."}, testTTL)

	rrs := sets[dns.TypePTR]
	require.Len(t, rrs, 2)
	require.Len(t, sets, 1, "a reverse name has nothing but PTR to say")

	assert.Equal(t, "a.shop.incus.", rrs[0].(*dns.PTR).Ptr)
	assert.Equal(t, "b.shop.incus.", rrs[1].(*dns.PTR).Ptr)
	assert.Equal(t, "20.1.0.10.in-addr.arpa.", rrs[0].Header().Name)
	assert.EqualValues(t, testTTL, rrs[0].Header().Ttl)

	assert.Nil(t, RenderPTR("20.1.0.10.in-addr.arpa.", nil, testTTL),
		"no targets is a name that does not exist, not one with nothing to say")
}

// TestAnswerReverseObeysVisibility is the reverse half of the visibility rule: a
// PTR answers only a querier on the address's network, NXDOMAIN otherwise.
func TestAnswerReverseObeysVisibility(t *testing.T) {
	tests := []struct {
		name  string
		qname string
		from  string
		want  int
		ptr   string
	}{
		{
			name:  "user-db reverses the address it shares a network with",
			qname: "20.2.0.10.in-addr.arpa.",
			from:  "10.0.2.30",
			want:  dns.RcodeSuccess,
			ptr:   "user-api.shop.incus.",
		},
		{
			name:  "gateway reverses the same host on its own network",
			qname: "20.1.0.10.in-addr.arpa.",
			from:  "10.0.1.10",
			want:  dns.RcodeSuccess,
			ptr:   "user-api.shop.incus.",
		},
		{
			name:  "gateway cannot reverse an address on a network it is not on",
			qname: "20.2.0.10.in-addr.arpa.",
			from:  "10.0.1.10",
			want:  dns.RcodeNameError,
		},
		{
			name:  "user-db cannot reverse the same host's gateway address",
			qname: "20.1.0.10.in-addr.arpa.",
			from:  "10.0.2.30",
			want:  dns.RcodeNameError,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			v := New()
			v.Replace(reversePiece())
			v.SetHealthy(true)

			w := dnstest.NewRecorder(&test.ResponseWriter{})

			handled, _, err := v.Answer(context.Background(), w, query(tc.qname, dns.TypePTR, tc.from))
			require.NoError(t, err)
			require.True(t, handled, "a reverse zone that exists must be answered here")
			require.Equal(t, tc.want, w.Msg.Rcode)

			if tc.ptr == "" {
				assert.Empty(t, w.Msg.Answer, "leaked a name across networks")

				return
			}

			require.Len(t, w.Msg.Answer, 1)

			ptr, ok := w.Msg.Answer[0].(*dns.PTR)
			require.True(t, ok, "answer is %T, want *dns.PTR", w.Msg.Answer[0])
			assert.Equal(t, tc.ptr, ptr.Ptr)
		})
	}
}

// TestAnswerReverseNameHasOnlyPTR keeps NODATA and NXDOMAIN apart: the name
// exists for this querier, so asking it for an address is nothing to say.
func TestAnswerReverseNameHasOnlyPTR(t *testing.T) {
	v := New()
	v.Replace(reversePiece())
	v.SetHealthy(true)

	w := dnstest.NewRecorder(&test.ResponseWriter{})

	_, _, err := v.Answer(context.Background(), w, query("20.2.0.10.in-addr.arpa.", dns.TypeA, "10.0.2.30"))
	require.NoError(t, err)

	assert.Equal(t, dns.RcodeSuccess, w.Msg.Rcode)
	assert.Empty(t, w.Msg.Answer)
	require.Len(t, w.Msg.Ns, 1, "NODATA has to carry the zone's SOA")
	assert.IsType(t, &dns.SOA{}, w.Msg.Ns[0])
}

// TestAnswerReverseFallsThroughUnclaimedSubnet is why the reverse zone comes
// from the addresses served: a subnet nothing sits on is not ours to answer for.
func TestAnswerReverseFallsThroughUnclaimedSubnet(t *testing.T) {
	v := New()
	v.Replace(reversePiece())
	v.SetHealthy(true)

	w := dnstest.NewRecorder(&test.ResponseWriter{})

	handled, _, err := v.Answer(context.Background(), w, query("50.3.0.10.in-addr.arpa.", dns.TypePTR, "10.0.1.10"))
	require.NoError(t, err)

	assert.False(t, handled, "answered for a subnet no zone was built for")
	assert.Nil(t, w.Msg)
}

func TestLookupVisibility(t *testing.T) {
	snap := testTopology()

	tests := []struct {
		name    string
		from    string
		qname   string
		want    []string
		wantRes Result
	}{
		{"gateway sees user-api over api-net", "10.0.1.10", "user-api.shop.incus.", []string{"10.0.1.20"}, Success},
		{"gateway sees products-api", "10.0.1.10", "products-api.shop.incus.", []string{"10.0.1.40"}, Success},
		{"gateway cannot see user-db", "10.0.1.10", "user-db.shop.incus.", nil, NameError},
		{"gateway cannot see products-db", "10.0.1.10", "products-db.shop.incus.", nil, NameError},

		// api-net siblings do see each other.
		{"user-api sees products-api", "10.0.1.20", "products-api.shop.incus.", []string{"10.0.1.40"}, Success},
		{"products-api sees user-api", "10.0.1.40", "user-api.shop.incus.", []string{"10.0.1.20"}, Success},

		{"user-api sees user-db over user-net", "10.0.2.20", "user-db.shop.incus.", []string{"10.0.2.30"}, Success},
		{"user-db sees user-api, user-net address only", "10.0.2.30", "user-api.shop.incus.", []string{"10.0.2.20"}, Success},
		{"user-db cannot see gateway", "10.0.2.30", "gateway.shop.incus.", nil, NameError},
		{"user-db cannot see products-db", "10.0.2.30", "products-db.shop.incus.", nil, NameError},
		{"products-db cannot see user-db", "10.0.3.50", "user-db.shop.incus.", nil, NameError},

		{"unknown name", "10.0.1.10", "nope.shop.incus.", nil, NameError},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			view, ok := snap.ViewFor(netip.MustParseAddr(tc.from))
			require.True(t, ok, "no querier for %s", tc.from)

			got := snap.Resolve(tc.qname, dns.TypeA, view)

			require.Equal(t, tc.wantRes, got.Result)
			assertAddrs(t, got.RRs, tc.want)
		})
	}
}

// TestLookupMultiHomedAnswersSharedNetworkOnly pins the rule that a querier is
// never handed an address on a network it is not itself attached to.
func TestLookupMultiHomedAnswersSharedNetworkOnly(t *testing.T) {
	snap := testTopology()

	// user-api holds 10.0.1.20 and 10.0.2.20; each querier gets only one.
	view, ok := snap.ViewFor(netip.MustParseAddr("10.0.2.30"))
	require.True(t, ok)

	got := snap.Resolve("user-api.shop.incus.", dns.TypeA, view)
	assertAddrs(t, got.RRs, []string{"10.0.2.20"})
}

// TestQuerierAnonymousOnKnownNetwork covers an address belonging to no host but
// on a known network: it resolves when some host sits on exactly that set.
func TestQuerierAnonymousOnKnownNetwork(t *testing.T) {
	snap := testTopology()

	// gateway sits on api-net alone, so an anonymous api-net address names the very
	// same view and sees what gateway sees.
	view, ok := snap.ViewFor(netip.MustParseAddr("10.0.1.99"))
	require.True(t, ok, "expected an anonymous querier on api-net")

	got := snap.Resolve("gateway.shop.incus.", dns.TypeA, view)
	assertAddrs(t, got.RRs, []string{"10.0.1.10"})

	// An address outside every known network has no identity at all.
	_, ok = snap.ViewFor(netip.MustParseAddr("192.0.2.1"))
	assert.False(t, ok, "expected no querier for an address outside all networks")
}

// TestResolveRefusesUnmaterializedView pins that a querier whose set of networks
// no host sits on is refused rather than gathered against the zone directly.
//
// empty-net is known to the snapshot with nothing attached, so an address there
// identifies a querier while naming a view that was never built.
func TestResolveRefusesUnmaterializedView(t *testing.T) {
	piece := pieceOf("shop.incus.", testTTL, map[string][]map[string][]string{
		"gateway.shop.incus.": {{"api-net": {"10.0.1.10"}}},
	}, []NetEntry{
		{Prefix: netip.MustParsePrefix("10.0.1.0/24"), Key: "api-net"},
		{Prefix: netip.MustParsePrefix("10.0.9.0/24"), Key: "empty-net"},
	})

	snap := piece

	view, ok := snap.ViewFor(netip.MustParseAddr("10.0.9.5"))
	require.True(t, ok, "an address on a known network still identifies a querier")
	require.NotContains(t, snap.Views, view, "nothing sits on empty-net, so its view was never built")

	got := snap.Resolve("gateway.shop.incus.", dns.TypeA, view)
	assert.Equal(t, NameError, got.Result, "a view nothing materialized must disclose nothing")
	assert.Empty(t, got.RRs)
}

// TestLookupServiceFansOutToReplicas checks that one name carrying several hosts
// answers with every one the querier can reach, and no others.
func TestLookupServiceFansOutToReplicas(t *testing.T) {
	piece := pieceOf("shop.incus.", testTTL, map[string][]map[string][]string{
		// Two replicas on api-net, a third reachable only over products-net.
		"worker.shop.incus.": {
			{"api-net": {"10.0.1.61"}},
			{"api-net": {"10.0.1.62"}},
			{"products-net": {"10.0.3.63"}},
		},
	}, []NetEntry{
		{Prefix: netip.MustParsePrefix("10.0.1.0/24"), Key: "api-net"},
		{Prefix: netip.MustParsePrefix("10.0.3.0/24"), Key: "products-net"},
	})

	snap := piece

	// An api-net querier sees the api-net replicas alone.
	view, ok := snap.ViewFor(netip.MustParseAddr("10.0.1.61"))
	require.True(t, ok)

	got := snap.Resolve("worker.shop.incus.", dns.TypeA, view)
	assertAddrs(t, got.RRs, []string{"10.0.1.61", "10.0.1.62"})
}

func TestMatchZoneLongestWins(t *testing.T) {
	snap := &Snapshot{ByZone: map[string]*Zone{
		"incus.":      {},
		"shop.incus.": {},
	}}

	name, z := snap.MatchZone("gateway.shop.incus.")
	assert.Equal(t, "shop.incus.", name)
	assert.NotNil(t, z)

	name, _ = snap.MatchZone("gateway.other.incus.")
	assert.Equal(t, "incus.", name, "a shorter zone still matches")

	name, z = snap.MatchZone("gateway.example.org.")
	assert.Empty(t, name)
	assert.Nil(t, z, "a name outside every zone matches nothing")
}

// TestAmbiguousAddressIdentifiesNobody pins the refusal: an address claimed by
// more than one host makes the querier unknowable. Detecting it is the source's.
func TestAmbiguousAddressIdentifiesNobody(t *testing.T) {
	shared := netip.MustParseAddr("10.0.1.10")

	snap := &Snapshot{
		ByZone: map[string]*Zone{"a.incus.": {Names: map[string]map[string]RRSets{}}},
		ByAddr: map[netip.Addr]ViewID{shared: AmbiguousView},
		Views:  map[ViewID]map[string]RRSets{},
		Nets:   []NetEntry{{Prefix: netip.MustParsePrefix("10.0.1.0/24"), Key: "a/net"}},
	}

	_, ok := snap.ViewFor(shared)
	assert.False(t, ok, "an ambiguous address must identify nobody")

	// And it must not fall through to the anonymous path, which would hand it the
	// view of everyone on that subnet.
	other, ok := snap.ViewFor(netip.MustParseAddr("10.0.1.99"))
	assert.True(t, ok, "an unclaimed address on a known network is still anonymous")
	assert.Equal(t, ViewOf([]string{"a/net"}), other)
}

// TestWithTTLSharesUnlessClamped is why answering is allocation-free: the normal
// path reslices the snapshot's own records, and only a different TTL copies.
func TestWithTTLSharesUnlessClamped(t *testing.T) {
	rrs := Render("gateway.shop.incus.", []netip.Addr{netip.MustParseAddr("10.0.1.10")}, nil, testTTL)[dns.TypeA]

	same := withTTL(rrs, testTTL, testTTL)
	require.Len(t, same, 1)
	assert.Same(t, rrs[0], same[0], "an unchanged TTL must not copy")

	clamped := withTTL(rrs, testTTL, 1)
	require.Len(t, clamped, 1)
	assert.NotSame(t, rrs[0], clamped[0], "a different TTL must copy")
	assert.EqualValues(t, 1, clamped[0].Header().Ttl)
	assert.EqualValues(t, testTTL, rrs[0].Header().Ttl, "and must not retune the original")
}

// engineWith returns an ECSView serving the worked topology, healthy, published
// the way a source does so the fold and TTL derivation are exercised too.
func engineWith(t *testing.T) *ECSView {
	t.Helper()

	v := New()
	v.Replace(testPiece())
	v.SetHealthy(true)

	return v
}

// query builds a request, optionally carrying an EDNS0 Client Subnet address the
// way a forwarding resolver does with add-subnet=32,128.
func query(qname string, qtype uint16, from string) *dns.Msg {
	m := new(dns.Msg)
	m.SetQuestion(qname, qtype)

	if from == "" {
		return m
	}

	ip := net.ParseIP(from)
	subnet := &dns.EDNS0_SUBNET{Code: dns.EDNS0SUBNET, Address: ip}

	if ip.To4() != nil {
		subnet.Family = 1
		subnet.SourceNetmask = 32
	} else {
		subnet.Family = 2
		subnet.SourceNetmask = 128
	}

	opt := &dns.OPT{Hdr: dns.RR_Header{Name: ".", Rrtype: dns.TypeOPT}}
	opt.Option = append(opt.Option, subnet)
	m.Extra = append(m.Extra, opt)

	return m
}

func TestAnswerVisibleHost(t *testing.T) {
	v := engineWith(t)
	w := dnstest.NewRecorder(&test.ResponseWriter{})

	handled, _, err := v.Answer(context.Background(), w, query("user-api.shop.incus.", dns.TypeA, "10.0.1.10"))
	require.NoError(t, err)
	require.True(t, handled, "an in-zone name must be answered here")
	require.Equal(t, dns.RcodeSuccess, w.Msg.Rcode)
	assert.True(t, w.Msg.Authoritative)
	require.Len(t, w.Msg.Answer, 1)

	a, ok := w.Msg.Answer[0].(*dns.A)
	require.True(t, ok, "answer is %T, want *dns.A", w.Msg.Answer[0])
	assert.Equal(t, "10.0.1.20", a.A.String())
}

func TestAnswerFallsThroughForeignZone(t *testing.T) {
	v := engineWith(t)
	w := dnstest.NewRecorder(&test.ResponseWriter{})

	handled, _, err := v.Answer(context.Background(), w, query("example.org.", dns.TypeA, "10.0.1.10"))
	require.NoError(t, err)
	assert.False(t, handled, "a name outside every zone must fall through")
	assert.Nil(t, w.Msg, "and nothing may be written")
}

// TestAnswerWithoutAnyIdentityFailsClosed pins the security property: with no
// way to place the querier, nothing is disclosed.
func TestAnswerWithoutAnyIdentityFailsClosed(t *testing.T) {
	v := engineWith(t)
	w := dnstest.NewRecorder(&test.ResponseWriter{})

	_, _, err := v.Answer(context.Background(), w, query("gateway.shop.incus.", dns.TypeA, ""))
	require.NoError(t, err)

	assert.Equal(t, dns.RcodeNameError, w.Msg.Rcode)
	assert.Empty(t, w.Msg.Answer, "leaked answers with no identifiable querier")
}

// TestAnswerIdentifiesQuerierBySourceAddress covers the direct-resolver
// deployment: nothing relays the query, so no client subnet is attached, but the
// source address is the querier.
func TestAnswerIdentifiesQuerierBySourceAddress(t *testing.T) {
	for _, tc := range []struct {
		name string
		tcp  bool
	}{
		{name: "udp"},
		{name: "tcp", tcp: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v := engineWith(t)
			w := dnstest.NewRecorder(&test.ResponseWriter{RemoteIP: "10.0.1.10", TCP: tc.tcp})

			_, _, err := v.Answer(context.Background(), w, query("user-api.shop.incus.", dns.TypeA, ""))
			require.NoError(t, err)
			require.Equal(t, dns.RcodeSuccess, w.Msg.Rcode)
			require.Len(t, w.Msg.Answer, 1)

			a, ok := w.Msg.Answer[0].(*dns.A)
			require.True(t, ok, "answer is %T, want *dns.A", w.Msg.Answer[0])
			assert.Equal(t, "10.0.1.20", a.A.String())
		})
	}
}

// TestAnswerSourceAddressObeysVisibility checks the fallback is filtered like any
// other querier rather than being trusted with the whole zone.
func TestAnswerSourceAddressObeysVisibility(t *testing.T) {
	v := engineWith(t)
	w := dnstest.NewRecorder(&test.ResponseWriter{RemoteIP: "10.0.1.10"})

	_, _, err := v.Answer(context.Background(), w, query("user-db.shop.incus.", dns.TypeA, ""))
	require.NoError(t, err)

	assert.Equal(t, dns.RcodeNameError, w.Msg.Rcode)
	assert.Empty(t, w.Msg.Answer, "leaked an invisible host to a source-identified querier")
}

// TestAnswerClientSubnetWinsOverSourceAddress pins the precedence. A relaying
// resolver puts its own address on the packet, so the client subnet is the more
// specific answer whenever both are present.
func TestAnswerClientSubnetWinsOverSourceAddress(t *testing.T) {
	v := engineWith(t)

	// The packet comes from user-db, the subnet says gateway. Only gateway may
	// see products-api, so the subnet is what decided if the answer arrives.
	w := dnstest.NewRecorder(&test.ResponseWriter{RemoteIP: "10.0.2.30"})

	_, _, err := v.Answer(context.Background(), w, query("products-api.shop.incus.", dns.TypeA, "10.0.1.10"))
	require.NoError(t, err)
	require.Equal(t, dns.RcodeSuccess, w.Msg.Rcode)
	require.Len(t, w.Msg.Answer, 1)

	a, ok := w.Msg.Answer[0].(*dns.A)
	require.True(t, ok)
	assert.Equal(t, "10.0.1.40", a.A.String())
}

func TestAnswerApex(t *testing.T) {
	for _, tc := range []struct {
		qtype   uint16
		name    string
		inAnswr bool
	}{
		{dns.TypeSOA, "SOA", true},
		{dns.TypeNS, "NS", true},
		{dns.TypeA, "A", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v := engineWith(t)
			w := dnstest.NewRecorder(&test.ResponseWriter{})

			// The apex needs no querier at all: it discloses only that the zone
			// exists, so it answers before the identity check.
			_, _, err := v.Answer(context.Background(), w, query("shop.incus.", tc.qtype, ""))
			require.NoError(t, err)
			assert.Equal(t, dns.RcodeSuccess, w.Msg.Rcode)

			if tc.inAnswr {
				assert.NotEmpty(t, w.Msg.Answer)
			} else {
				assert.Empty(t, w.Msg.Answer)
				assert.Len(t, w.Msg.Ns, 1, "NODATA carries an SOA")
			}
		})
	}
}

// echoingEngine is engineWith with the RFC 7871 reply option turned on, which is
// what a Corefile does with `echo_subnet`.
func echoingEngine(t *testing.T) *ECSView {
	t.Helper()

	v := engineWith(t)
	v.EchoSubnet = true

	return v
}

// replyECS is the client subnet a reply carries, or nil when it carries none.
func replyECS(t *testing.T, m *dns.Msg) *dns.EDNS0_SUBNET {
	t.Helper()

	opt := m.IsEdns0()
	if opt == nil {
		return nil
	}

	for _, o := range opt.Option {
		subnet, ok := o.(*dns.EDNS0_SUBNET)
		if ok {
			return subnet
		}
	}

	return nil
}

// TestAnswerEchoesClientSubnetScope pins RFC 7871's SCOPE PREFIX-LENGTH. It is
// the only signal a cache in front has for how widely an answer may be shared,
// and no option at all reads as "not tailored" - which is the cross-network leak
// the views exist to prevent, arriving in front of the plugin chain.
func TestAnswerEchoesClientSubnetScope(t *testing.T) {
	for _, tc := range []struct {
		name  string
		qname string
		qtype uint16
		from  string
		rcode int
		scope uint8
	}{
		{"visible name", "user-api.shop.incus.", dns.TypeA, "10.0.1.10", dns.RcodeSuccess, 32},
		{"invisible name", "user-db.shop.incus.", dns.TypeA, "10.0.1.10", dns.RcodeNameError, 32},
		{"unplaceable querier", "gateway.shop.incus.", dns.TypeA, "192.0.2.1", dns.RcodeNameError, 32},
		{"nodata", "user-api.shop.incus.", dns.TypeMX, "10.0.1.10", dns.RcodeSuccess, 32},
		{"apex is the same for every querier", "shop.incus.", dns.TypeSOA, "10.0.1.10", dns.RcodeSuccess, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v := echoingEngine(t)
			w := dnstest.NewRecorder(&test.ResponseWriter{})

			r := query(tc.qname, tc.qtype, tc.from)

			_, _, err := v.Answer(context.Background(), w, r)
			require.NoError(t, err)
			require.Equal(t, tc.rcode, w.Msg.Rcode)

			got := replyECS(t, w.Msg)
			require.NotNil(t, got, "an answer to a client subnet must echo one")

			assert.Equal(t, tc.scope, got.SourceScope)
			assert.EqualValues(t, 1, got.Family)
			assert.EqualValues(t, 32, got.SourceNetmask, "source prefix length is copied, not chosen")
			assert.Equal(t, tc.from, got.Address.String(), "the address is copied too")

			// The reply hands the request's own option back, scope written into
			// it. Copying it would allocate on the query path for nothing.
			assert.Same(t, replyECS(t, r), got)
			assert.Equal(t, 1, cap(w.Msg.Extra),
				"capped, so an append cannot write into the request's own array")

			// Packing applies the source prefix length to the address and rejects
			// a family that disagrees with it, and a reply that cannot be packed
			// is a SERVFAIL no recorder would show.
			_, err = w.Msg.Pack()
			require.NoError(t, err)
		})
	}
}

// TestAnswerEchoesIPv6Scope is the same rule for an IPv6 querier, whose whole
// address is 128 bits.
func TestAnswerEchoesIPv6Scope(t *testing.T) {
	v := New()
	v.Replace(pieceOf("shop.incus.", testTTL, map[string][]map[string][]string{
		"gateway.shop.incus.":  {{"api-net": {"fd00:1::10"}}},
		"user-api.shop.incus.": {{"api-net": {"fd00:1::20"}}},
	}, []NetEntry{
		{Prefix: netip.MustParsePrefix("fd00:1::/64"), Key: "api-net"},
	}))
	v.SetHealthy(true)
	v.EchoSubnet = true

	w := dnstest.NewRecorder(&test.ResponseWriter{})

	_, _, err := v.Answer(context.Background(), w, query("user-api.shop.incus.", dns.TypeAAAA, "fd00:1::10"))
	require.NoError(t, err)
	require.Equal(t, dns.RcodeSuccess, w.Msg.Rcode)
	require.NotEmpty(t, w.Msg.Answer)

	got := replyECS(t, w.Msg)
	require.NotNil(t, got)

	assert.EqualValues(t, 2, got.Family)
	assert.EqualValues(t, 128, got.SourceNetmask)
	assert.EqualValues(t, 128, got.SourceScope)
}

// TestAnswerWithoutClientSubnetEchoesNone covers the source-address path: nothing
// asserted a client subnet, so there is nothing to echo and nothing in front to
// mislead.
func TestAnswerWithoutClientSubnetEchoesNone(t *testing.T) {
	v := echoingEngine(t)
	w := dnstest.NewRecorder(&test.ResponseWriter{RemoteIP: "10.0.1.10"})

	_, _, err := v.Answer(context.Background(), w, query("user-api.shop.incus.", dns.TypeA, ""))
	require.NoError(t, err)
	require.Equal(t, dns.RcodeSuccess, w.Msg.Rcode)

	assert.Nil(t, replyECS(t, w.Msg), "echoed a client subnet nothing sent")
}

// TestAnswerEchoKeepsOtherOptionsOffTheReply covers the query that brought more
// than a client subnet. A cookie or a keepalive is the query's, and answering it
// unread would be answering a question this plugin does not understand - so the
// reply gets a record of its own with the subnet alone on it.
func TestAnswerEchoKeepsOtherOptionsOffTheReply(t *testing.T) {
	v := echoingEngine(t)
	w := dnstest.NewRecorder(&test.ResponseWriter{})

	r := query("user-api.shop.incus.", dns.TypeA, "10.0.1.10")
	r.IsEdns0().Option = append(r.IsEdns0().Option, &dns.EDNS0_NSID{Code: dns.EDNS0NSID})

	_, _, err := v.Answer(context.Background(), w, r)
	require.NoError(t, err)
	require.Equal(t, dns.RcodeSuccess, w.Msg.Rcode)

	opt := w.Msg.IsEdns0()
	require.NotNil(t, opt)
	require.Len(t, opt.Option, 1, "the reply answers about the subnet and nothing else")

	got := replyECS(t, w.Msg)
	require.NotNil(t, got)
	assert.EqualValues(t, 32, got.SourceScope)
	assert.NotSame(t, r.IsEdns0(), opt, "the query's own record carries options this reply must not")

	_, err = w.Msg.Pack()
	require.NoError(t, err)
}

// TestAnswerEchoIsOptIn pins the default. Building and packing the option costs
// the query path allocations per query, so it is off until a Corefile asks for
// it - and then an answer is the same answer either way.
func TestAnswerEchoIsOptIn(t *testing.T) {
	v := engineWith(t)
	w := dnstest.NewRecorder(&test.ResponseWriter{})

	_, _, err := v.Answer(context.Background(), w, query("user-api.shop.incus.", dns.TypeA, "10.0.1.10"))
	require.NoError(t, err)
	require.Equal(t, dns.RcodeSuccess, w.Msg.Rcode)
	require.Len(t, w.Msg.Answer, 1, "the client subnet still decides who is asking")

	assert.Nil(t, replyECS(t, w.Msg), "echoed without echo_subnet")
	assert.Empty(t, w.Msg.Extra, "and put no OPT record on the reply at all")
}

// TestAnswerClampsTTLWhileUnhealthy checks the stale clamp: while a source says
// its data is not fresh, answers expire fast whatever the configured TTL.
func TestAnswerClampsTTLWhileUnhealthy(t *testing.T) {
	v := New()
	v.Replace(testPieceTTL(3600))
	v.SetHealthy(false)

	w := dnstest.NewRecorder(&test.ResponseWriter{})

	_, _, err := v.Answer(context.Background(), w, query("user-api.shop.incus.", dns.TypeA, "10.0.1.10"))
	require.NoError(t, err)
	require.NotEmpty(t, w.Msg.Answer)

	assert.EqualValues(t, staleTTL, w.Msg.Answer[0].Header().Ttl,
		"an unhealthy source must clamp the TTL")
}

// assertAddrs compares an answer's records to the expected addresses, order
// independent.
func assertAddrs(t *testing.T, rrs []dns.RR, want []string) {
	t.Helper()

	got := make([]string, 0, len(rrs))

	for _, rr := range rrs {
		switch v := rr.(type) {
		case *dns.A:
			got = append(got, v.A.String())
		case *dns.AAAA:
			got = append(got, v.AAAA.String())
		}
	}

	// ElementsMatch rather than Equal: order is not part of the contract, and it
	// treats a nil expectation and an empty answer as the same thing.
	assert.ElementsMatch(t, want, got)
}

// cnamePiece is the worked topology plus one name in a zone nobody asked us to
// serve, pointing at gateway. That is what a source builds for an absolute alias
// it cannot fit into any zone the fleet already has.
func cnamePiece() *Snapshot {
	snap := testPiece()

	base := snap.ByZone["shop.incus."].Names["gateway.shop.incus."]

	addZone(snap, "example.com.", &Zone{
		Serial:      1,
		Fallthrough: true,
		Names: map[string]map[string]RRSets{
			"me.example.com.": RenderCName("me.example.com.", "gateway.shop.incus.",
				base, []string{"api-net"}, testTTL),
		},
	})

	return snap
}

// TestRenderCNameChasesAtBuildTime pins what makes answering through a CNAME the
// same three map lookups as answering anything else: the target's records are
// already in the set the query will ask for, canonical name first.
func TestRenderCNameChasesAtBuildTime(t *testing.T) {
	base := map[string]RRSets{
		"api-net": Render("gateway.shop.incus.", []netip.Addr{netip.MustParseAddr("10.0.1.10")}, nil, testTTL),
	}

	got := RenderCName("me.example.com.", "gateway.shop.incus.", base, []string{"api-net"}, testTTL)

	sets := got["api-net"]
	require.NotNil(t, sets)

	a := sets[dns.TypeA]
	require.Len(t, a, 2, "an A query has to be told the canonical name and what it resolves to")
	assert.IsType(t, &dns.CNAME{}, a[0])
	assert.IsType(t, &dns.A{}, a[1])

	cname := sets[dns.TypeCNAME]
	require.Len(t, cname, 1)
	assert.Same(t, cname[0], a[0], "the same CNAME value serves both sets")
	assert.Equal(t, "gateway.shop.incus.", cname[0].(*dns.CNAME).Target)

	// The target's own records are still shared rather than copied per name.
	assert.Same(t, base["api-net"][dns.TypeA][0], a[1])
}

// TestRenderCNameSkipsNetworksTheTargetIsNotOn pins that pointing at nothing is
// not a name. A bare CNAME on a wire the target has no records on would send a
// querier after something it cannot resolve.
func TestRenderCNameSkipsNetworksTheTargetIsNotOn(t *testing.T) {
	base := map[string]RRSets{
		"api-net": Render("gateway.shop.incus.", []netip.Addr{netip.MustParseAddr("10.0.1.10")}, nil, testTTL),
	}

	got := RenderCName("me.example.com.", "gateway.shop.incus.", base,
		[]string{"api-net", "user-net"}, testTTL)

	assert.Contains(t, got, "api-net")
	assert.NotContains(t, got, "user-net")
}

// TestAnswerFallthroughZoneClaimsOnlyItsNames pins the whole point of the flag:
// a zone invented to hold a handful of names may not blackhole the rest of the
// domain, which the fleet still has to resolve through the forwarder.
func TestAnswerFallthroughZoneClaimsOnlyItsNames(t *testing.T) {
	v := New()
	v.Replace(cnamePiece())
	v.SetHealthy(true)

	t.Run("a name it holds is answered here", func(t *testing.T) {
		w := dnstest.NewRecorder(&test.ResponseWriter{})

		handled, _, err := v.Answer(context.Background(), w, query("me.example.com.", dns.TypeA, "10.0.1.10"))
		require.NoError(t, err)
		require.True(t, handled)
		require.Equal(t, dns.RcodeSuccess, w.Msg.Rcode)
		require.Len(t, w.Msg.Answer, 2)

		assert.Equal(t, "gateway.shop.incus.", w.Msg.Answer[0].(*dns.CNAME).Target)
		assert.Equal(t, "10.0.1.10", w.Msg.Answer[1].(*dns.A).A.String())
	})

	t.Run("a name it does not hold goes to the forwarder", func(t *testing.T) {
		w := dnstest.NewRecorder(&test.ResponseWriter{})

		handled, _, err := v.Answer(context.Background(), w, query("www.example.com.", dns.TypeA, "10.0.1.10"))
		require.NoError(t, err)
		assert.False(t, handled, "an unclaimed name in an invented zone must fall through")
		assert.Nil(t, w.Msg, "and nothing may be written")
	})

	t.Run("the apex is not ours to answer either", func(t *testing.T) {
		w := dnstest.NewRecorder(&test.ResponseWriter{})

		handled, _, err := v.Answer(context.Background(), w, query("example.com.", dns.TypeSOA, "10.0.1.10"))
		require.NoError(t, err)
		assert.False(t, handled, "we synthesized an SOA for a domain we do not serve")
		assert.Nil(t, w.Msg)
	})
}

// TestAnswerFallthroughZoneStillHidesInvisibleNames pins the half that keeps the
// flag from becoming a disclosure. Held is the test for falling through, not
// visible - otherwise the forwarder's answer for a name this querier may not see
// would tell it the name exists here.
func TestAnswerFallthroughZoneStillHidesInvisibleNames(t *testing.T) {
	v := New()
	v.Replace(cnamePiece())
	v.SetHealthy(true)

	// user-db sits on user-net alone, and the name points at a host on api-net.
	w := dnstest.NewRecorder(&test.ResponseWriter{})

	handled, _, err := v.Answer(context.Background(), w, query("me.example.com.", dns.TypeA, "10.0.2.30"))
	require.NoError(t, err)
	require.True(t, handled, "an invisible name must be answered here, not handed on")

	assert.Equal(t, dns.RcodeNameError, w.Msg.Rcode)
	assert.Empty(t, w.Msg.Answer)
}
