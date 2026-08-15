package ecs_view

import (
	"context"
	"net/netip"
	"testing"

	"github.com/coredns/coredns/plugin/test"
	"github.com/miekg/dns"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// xfrRecorder keeps every message a transfer wrote. dnstest.Recorder keeps the
// last one, and a transfer is the stream rather than its final message.
type xfrRecorder struct {
	*test.ResponseWriter

	msgs []*dns.Msg
}

func (x *xfrRecorder) WriteMsg(m *dns.Msg) error {
	x.msgs = append(x.msgs, m)

	return nil
}

// records is everything the stream carried, in the order it went out.
func (x *xfrRecorder) records() []dns.RR {
	var out []dns.RR

	for _, m := range x.msgs {
		out = append(out, m.Answer...)
	}

	return out
}

// xfrPeer is a secondary on a network no instance sits on, which is the usual
// case: an off-fleet nameserver is placed by the allowlist and by nothing else.
func xfrPeer(tcp bool) *xfrRecorder {
	return &xfrRecorder{ResponseWriter: &test.ResponseWriter{RemoteIP: "192.0.2.53", TCP: tcp}}
}

// xfrQuery is one transfer request. serial > 0 makes it the IXFR a secondary
// sends when it already holds the zone.
func xfrQuery(zone string, serial uint32) *dns.Msg {
	m := new(dns.Msg)

	if serial == 0 {
		m.SetQuestion(zone, dns.TypeAXFR)

		return m
	}

	m.SetQuestion(zone, dns.TypeIXFR)
	m.Ns = []dns.RR{&dns.SOA{
		Hdr:    dns.RR_Header{Name: zone, Rrtype: dns.TypeSOA, Class: dns.ClassINET},
		Serial: serial,
	}}

	return m
}

// xfrView is an engine holding one two-network zone, opted in at both ends.
func xfrView(t *testing.T) *ECSView {
	t.Helper()

	snap := pieceOf("shop.incus.", testTTL, map[string][]map[string][]string{
		"gateway.shop.incus.": {{"api-net": {"10.0.1.10"}}},
		// Multi-homed, so the union a transfer carries differs from what any
		// one querier is answered with.
		"worker.shop.incus.": {{"api-net": {"10.0.1.20"}, "products-net": {"10.0.3.20"}}},
	}, []NetEntry{
		{Prefix: netip.MustParsePrefix("10.0.1.0/24"), Key: "api-net"},
		{Prefix: netip.MustParsePrefix("10.0.3.0/24"), Key: "products-net"},
	})

	snap.ByZone["shop.incus."].Serial = 7
	snap.ByZone["shop.incus."].Transfer = true

	v := New()
	v.AllowTransfer = []netip.Prefix{netip.MustParsePrefix("192.0.2.0/24")}
	v.Replace(snap)
	v.SetHealthy(true)

	return v
}

// TestTransferRefusesUnlessBothGatesSayYes pins the two independent opt-ins.
// The operator names who may ask; the project says which zones may go. Neither
// alone is enough, and a refusal writes nothing at all.
func TestTransferRefusesUnlessBothGatesSayYes(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string

		tcp      bool
		allow    []netip.Prefix
		opted    bool
		zone     string
		invented bool
	}{
		{
			name:  "over UDP, which cannot carry a stream",
			tcp:   false,
			allow: []netip.Prefix{netip.MustParsePrefix("192.0.2.0/24")},
			opted: true,
			zone:  "shop.incus.",
		},
		{
			name:  "from a peer the operator never named",
			tcp:   true,
			allow: nil,
			opted: true,
			zone:  "shop.incus.",
		},
		{
			name:  "of a zone whose project did not opt in",
			tcp:   true,
			allow: []netip.Prefix{netip.MustParsePrefix("192.0.2.0/24")},
			opted: false,
			zone:  "shop.incus.",
		},
		{
			name:  "of a zone this server does not serve at all",
			tcp:   true,
			allow: []netip.Prefix{netip.MustParsePrefix("192.0.2.0/24")},
			opted: true,
			zone:  "example.org.",
		},
		{
			// A name inside the zone is not an apex, and a transfer names one
			// or it names nothing.
			name:  "named at something below the apex",
			tcp:   true,
			allow: []netip.Prefix{netip.MustParsePrefix("192.0.2.0/24")},
			opted: true,
			zone:  "gateway.shop.incus.",
		},
		{
			// The zone invented to hold an absolute alias. Handing it over
			// would make the secondary authoritative for the whole domain.
			name:     "of a zone the source invented for one alias",
			tcp:      true,
			allow:    []netip.Prefix{netip.MustParsePrefix("192.0.2.0/24")},
			opted:    true,
			zone:     "example.com.",
			invented: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			v := xfrView(t)
			v.AllowTransfer = tc.allow
			v.current.Load().ByZone["shop.incus."].Transfer = tc.opted

			if tc.invented {
				// Exactly as aliasZones builds one: claiming its names, and
				// never carrying the opt-in.
				v.current.Load().ByZone["example.com."] = &Zone{
					Names:       map[string]map[string]RRSets{},
					Fallthrough: true,
				}
			}

			w := xfrPeer(tc.tcp)

			code, err := v.ServeDNS(context.Background(), w, xfrQuery(tc.zone, 0))
			require.NoError(t, err)

			assert.Equal(t, dns.RcodeRefused, code)
			assert.Empty(t, w.msgs, "a refusal is the caller's to write, so nothing went out here")
		})
	}
}

// TestTransferCarriesTheWholeZoneUnfiltered pins the shape of an AXFR: bounded
// by its SOA at both ends, and holding every address of a multi-homed host
// rather than the ones one querier can reach.
func TestTransferCarriesTheWholeZoneUnfiltered(t *testing.T) {
	t.Parallel()

	v := xfrView(t)
	w := xfrPeer(true)

	code, err := v.ServeDNS(context.Background(), w, xfrQuery("shop.incus.", 0))
	require.NoError(t, err)
	require.Equal(t, dns.RcodeSuccess, code)

	rrs := w.records()
	require.GreaterOrEqual(t, len(rrs), 4)

	first, isSOA := rrs[0].(*dns.SOA)
	require.True(t, isSOA, "a transfer opens with the SOA")
	assert.EqualValues(t, 7, first.Serial, "carrying the zone's own serial")

	last, isSOA := rrs[len(rrs)-1].(*dns.SOA)
	require.True(t, isSOA, "and closes with it, which is what ends the stream")
	assert.EqualValues(t, 7, last.Serial)

	_, isNS := rrs[1].(*dns.NS)
	assert.True(t, isNS, "the zone's NS follows the apex")

	var addrs []string

	for _, rr := range rrs {
		a, isA := rr.(*dns.A)
		if isA && rr.Header().Name == "worker.shop.incus." {
			addrs = append(addrs, a.A.String())
		}
	}

	assert.ElementsMatch(t, []string{"10.0.1.20", "10.0.3.20"}, addrs,
		"both networks, which no single querier is ever answered with")
}

// TestTransferSendsAnAliasWithoutTheRecordsItWasChasedInto pins the one place
// the query shape and the zone shape disagree. RenderCName puts the CNAME at
// the head of the target's A set so a query through it is one lookup; a zone
// holding a CNAME beside other data at one name is rejected by a secondary.
func TestTransferSendsAnAliasWithoutTheRecordsItWasChasedInto(t *testing.T) {
	t.Parallel()

	v := xfrView(t)

	z := v.current.Load().ByZone["shop.incus."]

	// The real renderer, so this tracks whatever shape aliases actually take.
	z.Names["store.shop.incus."] = RenderCName(
		"store.shop.incus.", "gateway.shop.incus.",
		z.Names["gateway.shop.incus."], []string{"api-net"}, testTTL)

	require.Contains(t, z.Names["store.shop.incus."]["api-net"], dns.TypeA,
		"the chased set is there to answer from, which is what this is about")

	w := xfrPeer(true)

	code, err := v.ServeDNS(context.Background(), w, xfrQuery("shop.incus.", 0))
	require.NoError(t, err)
	require.Equal(t, dns.RcodeSuccess, code)

	var types []uint16

	for _, rr := range w.records() {
		if rr.Header().Name == "store.shop.incus." {
			types = append(types, rr.Header().Rrtype)
		}
	}

	assert.Equal(t, []uint16{dns.TypeCNAME}, types,
		"the alias goes over alone, or the secondary rejects the zone")
}

// TestTransferAnswersIXFRFromTheSerial pins both arms: a secondary already
// holding the zone is told so in one record, and an older one takes all of it
// because there is no journal here to cut a delta from.
func TestTransferAnswersIXFRFromTheSerial(t *testing.T) {
	t.Parallel()

	t.Run("a current secondary gets the SOA and nothing else", func(t *testing.T) {
		t.Parallel()

		v := xfrView(t)
		w := xfrPeer(true)

		code, err := v.ServeDNS(context.Background(), w, xfrQuery("shop.incus.", 7))
		require.NoError(t, err)
		require.Equal(t, dns.RcodeSuccess, code)

		rrs := w.records()
		require.Len(t, rrs, 1)

		soa, isSOA := rrs[0].(*dns.SOA)
		require.True(t, isSOA)
		assert.EqualValues(t, 7, soa.Serial)
	})

	t.Run("an older one falls back to the whole zone", func(t *testing.T) {
		t.Parallel()

		v := xfrView(t)
		w := xfrPeer(true)

		code, err := v.ServeDNS(context.Background(), w, xfrQuery("shop.incus.", 6))
		require.NoError(t, err)
		require.Equal(t, dns.RcodeSuccess, code)

		assert.Greater(t, len(w.records()), 1, "a delta it cannot build is the whole zone")
	})
}

// TestTransferCutsEnvelopesAtTheWireLimit pins the batching. One message may
// not exceed 64 KiB, so a zone larger than that has to arrive in several.
func TestTransferCutsEnvelopesAtTheWireLimit(t *testing.T) {
	t.Parallel()

	v := xfrView(t)

	z := v.current.Load().ByZone["shop.incus."]

	// Enough names that the records cannot fit one message.
	for i := range 3000 {
		name := dns.Fqdn(dns.CanonicalName(
			"host"+string(rune('a'+i%26))+string(rune('a'+i/26%26))+string(rune('a'+i/676))) + ".shop.incus")

		z.Names[name] = map[string]RRSets{
			"api-net": Render(name, []netip.Addr{netip.MustParseAddr("10.0.1.99")}, nil, testTTL),
		}
	}

	w := xfrPeer(true)

	code, err := v.ServeDNS(context.Background(), w, xfrQuery("shop.incus.", 0))
	require.NoError(t, err)
	require.Equal(t, dns.RcodeSuccess, code)

	require.Greater(t, len(w.msgs), 1, "a zone this size does not fit one message")

	for i, m := range w.msgs {
		assert.LessOrEqual(t, m.Len(), dns.MaxMsgSize, "message %d is over the wire limit", i)
	}
}
