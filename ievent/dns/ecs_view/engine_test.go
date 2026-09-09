package ecs_view

import (
	"context"
	"log/slog"
	"net/netip"
	"testing"

	"github.com/coredns/coredns/plugin/pkg/dnstest"
	"github.com/coredns/coredns/plugin/test"
	iradix "github.com/hashicorp/go-immutable-radix/v2"
	"github.com/miekg/dns"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// testTTL is what the worked topology renders with, well above staleTTL so the
// clamp is visible when it applies.
const testTTL = 300

// The worked example, one project on two wires:
//
//	gateway   api-net
//	user-api  api-net, user-net
//	user-db   user-net
//
// Every querier is placed by address, so the view a name is answered from is the
// one its own addresses were indexed under.
var (
	apiView  = ViewOf([]string{"api-net"})
	userView = ViewOf([]string{"user-net"})
	bothView = ViewOf([]string{"api-net", "user-net"})
)

// builder collects a snapshot the way a source does: zones and names into two
// radix trees, addresses and prefixes into the two critbit ones.
type builder struct {
	snap  *Snapshot
	zones *iradix.Txn[*Zone]
	names *iradix.Txn[ViewAnswer]
}

func newBuilder() *builder {
	return &builder{
		snap:  &Snapshot{},
		zones: iradix.New[*Zone]().Txn(),
		names: iradix.New[ViewAnswer]().Txn(),
	}
}

func (b *builder) zone(name string, shadowing bool) *builder {
	b.zones.Insert(NameKey(name), &Zone{
		Name:      name,
		SOA:       testSOA(name),
		NS:        []dns.RR{testNS(name)},
		Shadowing: shadowing,
	})

	return b
}

func (b *builder) name(name string, answer ViewAnswer) *builder {
	b.names.Insert(NameKey(name), answer)

	return b
}

// at places a querier: a bare address is one an instance claims, a prefix is the
// network itself and answers whoever sits on it unclaimed.
func (b *builder) at(cidr string, view ViewID) *builder {
	prefix := netip.MustParsePrefix(cidr)

	key, v4 := AddrKey(prefix)
	if v4 {
		b.snap.ByIPv4.Set(key, view)
	} else {
		b.snap.ByIPv6.Set(key, view)
	}

	return b
}

func (b *builder) done() *Snapshot {
	b.snap.Denial = b.zones.Commit()
	b.snap.Tree = b.names.Commit()

	return b.snap
}

func testSOA(zone string) dns.RR {
	return &dns.SOA{
		Hdr:    dns.RR_Header{Name: zone, Rrtype: dns.TypeSOA, Class: dns.ClassINET, Ttl: testTTL},
		Ns:     "ns.dns." + zone,
		Mbox:   "hostmaster." + zone,
		Serial: 1,
		Minttl: testTTL,
	}
}

func testNS(zone string) dns.RR {
	return &dns.NS{
		Hdr: dns.RR_Header{Name: zone, Rrtype: dns.TypeNS, Class: dns.ClassINET, Ttl: testTTL},
		Ns:  "ns.dns." + zone,
	}
}

// a is one name's A records, as a source renders them per view.
func a(name string, addrs ...string) Records {
	out := make([]dns.RR, 0, len(addrs))

	for _, addr := range addrs {
		out = append(out, &dns.A{
			Hdr: dns.RR_Header{Name: name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: testTTL},
			A:   netip.MustParseAddr(addr).AsSlice(),
		})
	}

	return Records{dns.TypeA: out}
}

// testPiece is the worked topology as one source publishes it.
func testPiece() *Snapshot {
	return newBuilder().
		zone("shop.incus.", false).
		name("gateway.shop.incus.", ViewAnswer{
			apiView:  a("gateway.shop.incus.", "10.0.1.10"),
			bothView: a("gateway.shop.incus.", "10.0.1.10"),
		}).
		name("user-api.shop.incus.", ViewAnswer{
			apiView:  a("user-api.shop.incus.", "10.0.1.20"),
			userView: a("user-api.shop.incus.", "10.0.2.20"),
			bothView: a("user-api.shop.incus.", "10.0.1.20", "10.0.2.20"),
		}).
		name("user-db.shop.incus.", ViewAnswer{
			userView: a("user-db.shop.incus.", "10.0.2.30"),
			bothView: a("user-db.shop.incus.", "10.0.2.30"),
		}).
		at("10.0.1.10/32", apiView).
		at("10.0.1.20/32", bothView).
		at("10.0.2.20/32", bothView).
		at("10.0.2.30/32", userView).
		at("10.0.1.0/24", apiView).
		at("10.0.2.0/24", userView).
		done()
}

// engineWith is an engine serving the worked topology, healthy.
func engineWith(t *testing.T) *ECSView {
	t.Helper()

	v := New(slog.Default())
	v.Replace(testPiece())
	v.SetHealthy(true)

	return v
}

// query is one question, asked as client - the address decides the view.
func query(qname string, qtype uint16, client string) *dns.Msg {
	m := new(dns.Msg)
	m.SetQuestion(qname, qtype)

	addr := netip.MustParseAddr(client)

	subnet := &dns.EDNS0_SUBNET{Code: dns.EDNS0SUBNET, Address: addr.AsSlice()}
	if addr.Is4() {
		subnet.Family = 1
		subnet.SourceNetmask = 32
	} else {
		subnet.Family = 2
		subnet.SourceNetmask = 128
	}

	m.Extra = append(m.Extra, &dns.OPT{
		Hdr:    dns.RR_Header{Name: ".", Rrtype: dns.TypeOPT},
		Option: []dns.EDNS0{subnet},
	})

	return m
}

// ask runs one query against v and hands back what went on the wire.
func ask(t *testing.T, v *ECSView, qname string, qtype uint16, client string) (bool, *dns.Msg) {
	t.Helper()

	w := dnstest.NewRecorder(&test.ResponseWriter{})

	handled, _, err := v.Answer(context.Background(), w, query(qname, qtype, client))
	require.NoError(t, err)

	return handled, w.Msg
}

func TestAnswerServesWhatTheQuerierShares(t *testing.T) {
	t.Parallel()

	v := engineWith(t)

	cases := []struct {
		name   string
		qname  string
		client string
		want   []string
	}{
		{
			name:   "a querier on one wire sees the address on that wire",
			qname:  "user-api.shop.incus.",
			client: "10.0.1.10",
			want:   []string{"10.0.1.20"},
		},
		{
			name:   "and on the other wire, the other address of the same name",
			qname:  "user-api.shop.incus.",
			client: "10.0.2.30",
			want:   []string{"10.0.2.20"},
		},
		{
			name:   "a querier on both is answered both, joined at build time",
			qname:  "user-api.shop.incus.",
			client: "10.0.1.20",
			want:   []string{"10.0.1.20", "10.0.2.20"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			handled, m := ask(t, v, tc.qname, dns.TypeA, tc.client)

			require.True(t, handled)
			require.Equal(t, dns.RcodeSuccess, m.Rcode)

			got := make([]string, 0, len(m.Answer))
			for _, rr := range m.Answer {
				got = append(got, rr.(*dns.A).A.String())
			}

			assert.ElementsMatch(t, tc.want, got)
		})
	}
}

// TestAnswerHidesWhatTheQuerierCannotReach pins the rule the whole engine is
// for: unreachable reads exactly like missing, never like a refusal.
func TestAnswerHidesWhatTheQuerierCannotReach(t *testing.T) {
	t.Parallel()

	v := engineWith(t)

	// user-db is on user-net alone, and this querier is on api-net alone.
	handled, m := ask(t, v, "user-db.shop.incus.", dns.TypeA, "10.0.1.10")

	require.True(t, handled)
	assert.Equal(t, dns.RcodeNameError, m.Rcode)
	assert.Empty(t, m.Answer)
	require.Len(t, m.Ns, 1, "a denial carries the zone's SOA")
	assert.IsType(t, &dns.SOA{}, m.Ns[0])
}

func TestAnswerDeniesAndFallsThrough(t *testing.T) {
	t.Parallel()

	v := engineWith(t)

	cases := []struct {
		name    string
		qname   string
		qtype   uint16
		handled bool
		rcode   int
		soa     bool
	}{
		{
			name:    "a name that does not exist in a zone we serve is NXDOMAIN",
			qname:   "nope.shop.incus.",
			qtype:   dns.TypeA,
			handled: true,
			rcode:   dns.RcodeNameError,
			soa:     true,
		},
		{
			name:    "a type the name does not hold is NODATA, not NXDOMAIN",
			qname:   "user-api.shop.incus.",
			qtype:   dns.TypeMX,
			handled: true,
			rcode:   dns.RcodeSuccess,
			soa:     true,
		},
		{
			name:    "a name outside every zone falls through",
			qname:   "github.com.",
			qtype:   dns.TypeA,
			handled: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			handled, m := ask(t, v, tc.qname, tc.qtype, "10.0.1.10")

			require.Equal(t, tc.handled, handled)

			if !tc.handled {
				return
			}

			assert.Equal(t, tc.rcode, m.Rcode)
			assert.Empty(t, m.Answer)

			if tc.soa {
				require.Len(t, m.Ns, 1)
				assert.IsType(t, &dns.SOA{}, m.Ns[0])
			}
		})
	}
}

func TestAnswerApex(t *testing.T) {
	t.Parallel()

	v := engineWith(t)

	t.Run("SOA is answered", func(t *testing.T) {
		t.Parallel()

		_, m := ask(t, v, "shop.incus.", dns.TypeSOA, "10.0.1.10")

		require.Len(t, m.Answer, 1)
		assert.IsType(t, &dns.SOA{}, m.Answer[0])
	})

	t.Run("NS is answered", func(t *testing.T) {
		t.Parallel()

		_, m := ask(t, v, "shop.incus.", dns.TypeNS, "10.0.1.10")

		require.Len(t, m.Answer, 1)
		assert.IsType(t, &dns.NS{}, m.Answer[0])
	})
}

// TestShadowingClaimsItsNamesAndNothingElse pins the invented zone: it answers
// what it holds, and hands everything else on rather than denying for a domain
// it does not own - the apex included.
func TestShadowingClaimsItsNamesAndNothingElse(t *testing.T) {
	t.Parallel()

	snap := newBuilder().
		zone("shop.incus.", false).
		zone("example.com.", true).
		name("alias.example.com.", ViewAnswer{apiView: a("alias.example.com.", "10.0.1.10")}).
		at("10.0.1.0/24", apiView).
		done()

	v := New(slog.Default())
	v.Replace(snap)
	v.SetHealthy(true)

	t.Run("a name it holds is answered", func(t *testing.T) {
		t.Parallel()

		handled, m := ask(t, v, "alias.example.com.", dns.TypeA, "10.0.1.50")

		require.True(t, handled)
		assert.Equal(t, dns.RcodeSuccess, m.Rcode)
		assert.Len(t, m.Answer, 1)
	})

	t.Run("one it does not falls through rather than denying", func(t *testing.T) {
		t.Parallel()

		handled, _ := ask(t, v, "www.example.com.", dns.TypeA, "10.0.1.50")

		assert.False(t, handled)
	})

	t.Run("and its apex falls through too", func(t *testing.T) {
		t.Parallel()

		handled, _ := ask(t, v, "example.com.", dns.TypeSOA, "10.0.1.50")

		assert.False(t, handled, "naming a server here would claim the whole domain")
	})
}

// TestViewForPlacesTheQuerier pins that the longest match arbitrates: an address
// an instance claims beats the subnet holding it.
func TestViewForPlacesTheQuerier(t *testing.T) {
	t.Parallel()

	snap := testPiece()

	cases := []struct {
		name  string
		addr  string
		want  ViewID
		found bool
	}{
		{
			name:  "an address an instance claims answers from that instance's view",
			addr:  "10.0.1.20",
			want:  bothView,
			found: true,
		},
		{
			name:  "one nothing claims falls back to the network it sits in",
			addr:  "10.0.1.99",
			want:  apiView,
			found: true,
		},
		{
			name:  "and one on no known network is placed nowhere",
			addr:  "192.0.2.1",
			found: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, ok := snap.ViewFor(netip.MustParseAddr(tc.addr))

			require.Equal(t, tc.found, ok)

			if tc.found {
				assert.Equal(t, tc.want, got)
			}
		})
	}
}

// TestUnplaceableQuerierIsDenied pins failing closed: without a network there is
// no view, and any view picked would hand out records it cannot route to.
func TestUnplaceableQuerierIsDenied(t *testing.T) {
	t.Parallel()

	v := engineWith(t)

	handled, m := ask(t, v, "user-api.shop.incus.", dns.TypeA, "192.0.2.1")

	require.True(t, handled)
	assert.Equal(t, dns.RcodeNameError, m.Rcode)
	assert.Empty(t, m.Answer)
}

// TestAmbiguousViewRefuses pins that two claimants on one address answer nobody,
// rather than whichever the last write happened to be.
func TestAmbiguousViewRefuses(t *testing.T) {
	t.Parallel()

	snap := newBuilder().
		zone("shop.incus.", false).
		name("user-api.shop.incus.", ViewAnswer{apiView: a("user-api.shop.incus.", "10.0.1.20")}).
		at("10.0.1.10/32", AmbiguousView).
		at("10.0.1.0/24", apiView).
		done()

	v := New(slog.Default())
	v.Replace(snap)
	v.SetHealthy(true)

	handled, m := ask(t, v, "user-api.shop.incus.", dns.TypeA, "10.0.1.10")

	require.True(t, handled)
	assert.Equal(t, dns.RcodeNameError, m.Rcode, "a claimed-twice address identifies nobody")
}

// TestZoneOfMatchesTheLongestZone pins the key encoding, whose trap is that a
// zone must not prefix a longer sibling label.
func TestZoneOfMatchesTheLongestZone(t *testing.T) {
	t.Parallel()

	snap := newBuilder().
		zone("incus.", false).
		zone("shop.incus.", false).
		done()

	cases := []struct {
		name  string
		qname string
		want  string
	}{
		{name: "the longest zone wins", qname: "gateway.shop.incus.", want: "shop.incus."},
		{name: "a shorter one still matches", qname: "gateway.other.incus.", want: "incus."},
		{name: "the apex matches itself", qname: "shop.incus.", want: "shop.incus."},
		{name: "a name outside every zone matches nothing", qname: "github.com.", want: ""},
		{
			// Compared byte-wise, so without a separator after every label
			// "incus." would claim this too.
			name:  "a longer sibling label is not inside the zone",
			qname: "gateway.incusx.",
			want:  "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			z := snap.ZoneOf(tc.qname)

			if tc.want == "" {
				assert.Nil(t, z)

				return
			}

			require.NotNil(t, z)
			assert.Equal(t, tc.want, z.Name)
		})
	}
}

// TestUnhealthyClampsTheTTL pins that a stale answer expires fast, and that the
// snapshot's own records are not rewritten to do it.
func TestUnhealthyClampsTheTTL(t *testing.T) {
	t.Parallel()

	v := New(slog.Default())
	v.Replace(testPiece())
	v.SetHealthy(false)

	_, m := ask(t, v, "user-api.shop.incus.", dns.TypeA, "10.0.1.10")

	require.Len(t, m.Answer, 1)
	assert.EqualValues(t, staleTTL, m.Answer[0].Header().Ttl)

	// The shared record is untouched: the clamp copies rather than retunes.
	v.SetHealthy(true)

	_, again := ask(t, v, "user-api.shop.incus.", dns.TypeA, "10.0.1.10")

	require.Len(t, again.Answer, 1)
	assert.EqualValues(t, testTTL, again.Answer[0].Header().Ttl)
}
