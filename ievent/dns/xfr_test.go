package dns

import (
	"context"
	"net/netip"
	"strings"
	"testing"

	"github.com/coredns/coredns/plugin/test"
	incusapi "github.com/lxc/incus/v7/shared/api"
	"github.com/miekg/dns"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lxc/incus-compose/ievent/iutil"
)

// xfrTTL is what these fixtures render under.
const xfrTTL = 300

// xfrRecorder keeps every message a transfer wrote. A transfer is the stream
// rather than its final message, so the last one alone says nothing.
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
// case: an off-fleet name server is placed by the allow-list and by nothing else.
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

// xfrInstance is one instance as the enricher hands it over, carrying its
// project's own configuration: transfer and ns are the zone's to set, so they
// arrive from there and an instance naming them reaches nothing.
func xfrInstance(project, name string, own map[string]string, meta map[string]string, nets map[string]string) *instance {
	ev := eventOn(incusapi.EventLifecycleInstanceStarted, project, name, meta, nets).
		WithProject(iutil.NewProject(own))

	return patchInstance(ev, "example.")
}

// optedIn is a project that opted its zone into transfer and named a server.
func optedIn() map[string]string {
	return map[string]string{
		labelPrefix + metaTransfer: "true",
		labelPrefix + metaNS:       "ns1.example.org.",
	}
}

// xfrServed folds a fleet and hands back a handler holding what it published.
func xfrServed(t *testing.T, allow []netip.Prefix, held map[string]*instance) *xfr {
	t.Helper()

	s := newState(nil)
	for key, inst := range held {
		s.apply(key, inst, xfrTTL)
	}

	s.step(true, xfrTTL)

	x := newXFR(allow)
	x.Replace(s.snapshot())

	return x
}

// xfrFleet is one zone opted in, with a multi-homed instance in it so the union
// a transfer carries differs from what any one querier is answered with.
func xfrFleet(t *testing.T) *xfr {
	t.Helper()

	return xfrServed(t, []netip.Prefix{netip.MustParsePrefix("192.0.2.0/24")}, map[string]*instance{
		"shop/gateway": xfrInstance("shop", "gateway", optedIn(), nil,
			map[string]string{"api-net": "10.0.1.10"}),
		"shop/worker": xfrInstance("shop", "worker", optedIn(), nil,
			map[string]string{"api-net": "10.0.1.20", "products-net": "10.0.3.20"}),
	})
}

// serve runs one transfer and hands back what went on the wire.
func serve(t *testing.T, x *xfr, w *xfrRecorder, m *dns.Msg) int {
	t.Helper()

	code, err := x.ServeDNS(context.Background(), w, m)
	require.NoError(t, err)

	return code
}

// TestTransferRefusesUnlessBothGatesSayYes pins the two independent opt-ins: the
// operator names who may ask, the project names which zones may go, and neither
// alone is enough.
func TestTransferRefusesUnlessBothGatesSayYes(t *testing.T) {
	t.Parallel()

	allow := []netip.Prefix{netip.MustParsePrefix("192.0.2.0/24")}

	cases := []struct {
		name string

		tcp   bool
		allow []netip.Prefix
		own   map[string]string
		zone  string
	}{
		{
			name:  "over UDP, which cannot carry a stream",
			tcp:   false,
			allow: allow,
			own:   optedIn(),
			zone:  "shop.example.",
		},
		{
			name:  "from a peer the operator never named",
			tcp:   true,
			allow: nil,
			own:   optedIn(),
			zone:  "shop.example.",
		},
		{
			name:  "of a zone whose project did not opt in",
			tcp:   true,
			allow: allow,
			own:   nil,
			zone:  "shop.example.",
		},
		{
			name:  "of a name inside the zone rather than its apex",
			tcp:   true,
			allow: allow,
			own:   optedIn(),
			zone:  "gateway.shop.example.",
		},
		{
			name:  "of a zone this server does not hold at all",
			tcp:   true,
			allow: allow,
			own:   optedIn(),
			zone:  "elsewhere.test.",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			x := xfrServed(t, tc.allow, map[string]*instance{
				"shop/gateway": xfrInstance("shop", "gateway", tc.own, nil,
					map[string]string{"api-net": "10.0.1.10"}),
			})

			w := xfrPeer(tc.tcp)

			assert.Equal(t, dns.RcodeRefused, serve(t, x, w, xfrQuery(tc.zone, 0)))
			assert.Empty(t, w.msgs, "a refusal put something on the wire")
		})
	}
}

// TestTransferRefusesAnInventedZone: a zone conjured for one absolute alias is
// Shadowing, and handing it over would make the secondary authoritative for a
// domain this server holds a single name in.
func TestTransferRefusesAnInventedZone(t *testing.T) {
	t.Parallel()

	x := xfrServed(t, []netip.Prefix{netip.MustParsePrefix("192.0.2.0/24")}, map[string]*instance{
		"shop/gateway": xfrInstance("shop", "gateway", optedIn(),
			map[string]string{metaAliases: "www.elsewhere.test."},
			map[string]string{"api-net": "10.0.1.10"}),
	})

	w := xfrPeer(true)

	assert.Equal(t, dns.RcodeRefused, serve(t, x, w, xfrQuery("elsewhere.test.", 0)))
}

// TestTransferCarriesTheWholeZoneUnfiltered is the one answer here not filtered
// by who is asking: a multi-homed instance goes over with every address it has,
// where a querier on one of those networks is answered with that one alone.
func TestTransferCarriesTheWholeZoneUnfiltered(t *testing.T) {
	t.Parallel()

	w := xfrPeer(true)

	assert.Equal(t, dns.RcodeSuccess, serve(t, xfrFleet(t), w, xfrQuery("shop.example.", 0)))

	var addrs []string

	for _, rr := range w.records() {
		a, ok := rr.(*dns.A)
		if !ok {
			continue
		}

		addrs = append(addrs, a.Hdr.Name+" "+a.A.String())
	}

	assert.ElementsMatch(t, []string{
		"gateway.shop.example. 10.0.1.10",
		"worker.shop.example. 10.0.1.20",
		"worker.shop.example. 10.0.3.20",
	}, addrs)
}

// TestTransferBracketsTheZoneWithItsApex is what a secondary reads the stream
// by: SOA first, the same SOA last, and its NS set matching what the apex answers.
func TestTransferBracketsTheZoneWithItsApex(t *testing.T) {
	t.Parallel()

	w := xfrPeer(true)

	require.Equal(t, dns.RcodeSuccess, serve(t, xfrFleet(t), w, xfrQuery("shop.example.", 0)))

	rrs := w.records()
	require.Greater(t, len(rrs), 2)

	first, ok := rrs[0].(*dns.SOA)
	require.True(t, ok, "the stream did not open with a SOA")

	last, ok := rrs[len(rrs)-1].(*dns.SOA)
	require.True(t, ok, "the stream did not close with a SOA")

	assert.Equal(t, first.Serial, last.Serial, "the brackets carried different serials")

	var servers []string

	for _, rr := range rrs {
		ns, isNS := rr.(*dns.NS)
		if isNS && ns.Hdr.Name == "shop.example." {
			servers = append(servers, ns.Ns)
		}
	}

	assert.Equal(t, []string{"ns1.example.org."}, servers)
}

// TestTransferSendsAnAliasWithoutTheRecordsItWasChasedInto: answering chases a
// CNAME into the addresses it points at, so one name holds both. A zone file may
// not, and a secondary loading one that does rejects it.
func TestTransferSendsAnAliasWithoutTheRecordsItWasChasedInto(t *testing.T) {
	t.Parallel()

	x := xfrServed(t, []netip.Prefix{netip.MustParsePrefix("192.0.2.0/24")}, map[string]*instance{
		"shop/gateway": xfrInstance("shop", "gateway", optedIn(),
			map[string]string{metaAliases: "www"},
			map[string]string{"api-net": "10.0.1.10"}),
	})

	w := xfrPeer(true)

	require.Equal(t, dns.RcodeSuccess, serve(t, x, w, xfrQuery("shop.example.", 0)))

	var types []string

	for _, rr := range w.records() {
		if rr.Header().Name == "www.shop.example." {
			types = append(types, dns.TypeToString[rr.Header().Rrtype])
		}
	}

	assert.Equal(t, []string{"CNAME"}, types, "the alias traveled with the records it was chased into")
}

// TestTransferAnswersIXFRFromTheSerial: there is no journal to cut a delta from,
// so a secondary already on the serial is told so and everything else takes the
// whole zone.
func TestTransferAnswersIXFRFromTheSerial(t *testing.T) {
	t.Parallel()

	x := xfrFleet(t)

	current := xfrPeer(true)
	require.Equal(t, dns.RcodeSuccess, serve(t, x, current, xfrQuery("shop.example.", 0)))

	serial := current.records()[0].(*dns.SOA).Serial

	t.Run("already on the serial", func(t *testing.T) {
		w := xfrPeer(true)

		require.Equal(t, dns.RcodeSuccess, serve(t, x, w, xfrQuery("shop.example.", serial)))
		assert.Len(t, w.records(), 1, "a secondary that was current was sent the zone anyway")
	})

	t.Run("behind it", func(t *testing.T) {
		w := xfrPeer(true)

		require.Equal(t, dns.RcodeSuccess, serve(t, x, w, xfrQuery("shop.example.", serial-1)))
		assert.Greater(t, len(w.records()), 1, "an older serial was not sent the whole zone")
	})
}

// TestTransferCutsEnvelopesAtTheWireLimit: one message cannot carry a zone, and
// a peer reading one that overran would drop the transfer.
func TestTransferCutsEnvelopesAtTheWireLimit(t *testing.T) {
	t.Parallel()

	rrs := make([]dns.RR, 0, 4000)
	for range 4000 {
		rrs = append(rrs, &dns.A{
			Hdr: dns.RR_Header{
				Name: "filler.shop.example.", Rrtype: dns.TypeA,
				Class: dns.ClassINET, Ttl: xfrTTL,
			},
			A: []byte{10, 0, 1, 1},
		})
	}

	var seen int

	for envelope := range envelopes(rrs) {
		size := 0
		for _, rr := range envelope.RR {
			size += dns.Len(rr)
		}

		assert.LessOrEqual(t, size, maxEnvelope, "an envelope overran the wire limit")

		seen += len(envelope.RR)
	}

	assert.Equal(t, len(rrs), seen, "the cut lost records")
}

// TestTransferLeavesAChildZoneAlone is what the name tree's keying invites: a
// zone is a byte-wise prefix of the names in it, and so is every name of a zone
// underneath it. Carrying those would hand a secondary a delegation nobody made.
func TestTransferLeavesAChildZoneAlone(t *testing.T) {
	t.Parallel()

	// Two projects, one nested inside the other's zone by label.
	parent := optedIn()
	parent[labelPrefix+metaZone] = "example."

	child := optedIn()
	child[labelPrefix+metaZone] = "shop.example."

	x := xfrServed(t, []netip.Prefix{netip.MustParsePrefix("192.0.2.0/24")}, map[string]*instance{
		"top/edge": xfrInstance("top", "edge", parent, nil,
			map[string]string{"api-net": "10.0.1.10"}),
		"shop/gateway": xfrInstance("shop", "gateway", child, nil,
			map[string]string{"api-net": "10.0.1.20"}),
	})

	w := xfrPeer(true)

	require.Equal(t, dns.RcodeSuccess, serve(t, x, w, xfrQuery("example.", 0)))

	for _, rr := range w.records() {
		assert.False(t, strings.HasSuffix(rr.Header().Name, ".shop.example."),
			"the parent's transfer carried a name from the zone below it: %s", rr.Header().Name)
	}
}
