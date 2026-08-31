package dns

import (
	"context"
	"net"
	"testing"

	"github.com/coredns/coredns/plugin"
	"github.com/miekg/dns"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lxc/incus-compose/ievent/dns/ecs_view"
)

// recorder captures the reply a handler writes, so a test can assert on the
// message rather than on a network.
type recorder struct {
	dns.ResponseWriter

	msg *dns.Msg
}

func (r *recorder) WriteMsg(m *dns.Msg) error {
	r.msg = m

	return nil
}

func (r *recorder) LocalAddr() net.Addr  { return &net.UDPAddr{IP: net.IPv4zero, Port: 53} }
func (r *recorder) RemoteAddr() net.Addr { return &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 4242} }

// query is one A question with an EDNS0 buffer, the way a resolver sends it.
func query(name string) *dns.Msg {
	m := new(dns.Msg)
	m.SetQuestion(dns.Fqdn(name), dns.TypeA)
	m.SetEdns0(1232, false)

	return m
}

// TestWireWithNothingAfter pins the empty chain: the engine falls through to a
// refusal rather than a nil handler, which would panic on every query.
func TestWireWithNothingAfter(t *testing.T) {
	t.Parallel()

	view := ecs_view.New()

	wire(newXFR(nil), view, nil)

	require.NotNil(t, view.Next)

	w := &recorder{}

	rcode, err := view.Next.ServeDNS(t.Context(), w, query("example.com"))
	require.NoError(t, err)

	// Refused rather than an empty NOERROR, which would claim the name exists
	// here with no records of this type.
	assert.Equal(t, dns.RcodeRefused, rcode)

	// And it writes nothing: ClientWrite reports false for REFUSED, so ServeDNS
	// writes the reply on this plugin's behalf - pinned by TestAdapter's "a
	// refusal nobody wrote gets a reply written for it". Doing both put two
	// responses on the wire for every query that fell through.
	assert.Nil(t, w.msg, "refuse must not write; the adapter writes for it")
}

// TestWireOrdersTheChain pins that main lists the chain in the order a query
// travels it, and that the last one still reaches the refusal.
func TestWireOrdersTheChain(t *testing.T) {
	t.Parallel()

	var walked []string

	a := &marker{name: "a", walked: &walked}
	b := &marker{name: "b", walked: &walked}

	view := ecs_view.New()

	wire(newXFR(nil), view, []plugin.Plugin{
		func(next plugin.Handler) plugin.Handler { a.Next = next; return a },
		func(next plugin.Handler) plugin.Handler { b.Next = next; return b },
	})

	w := &recorder{}

	rcode, err := view.Next.ServeDNS(t.Context(), w, query("example.com"))
	require.NoError(t, err)

	assert.Equal(t, []string{"a", "b"}, walked)
	assert.Equal(t, dns.RcodeRefused, rcode, "the end of the chain still refuses")
}

// marker records that a query reached it and hands it on.
type marker struct {
	Next   plugin.Handler
	name   string
	walked *[]string
}

func (m *marker) Name() string { return m.name }

func (m *marker) ServeDNS(ctx context.Context, w dns.ResponseWriter, r *dns.Msg) (int, error) {
	if m.walked != nil {
		*m.walked = append(*m.walked, m.name)
	}

	return m.Next.ServeDNS(ctx, w, r)
}

func TestAdapter(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		msg  *dns.Msg

		chain     dns.HandlerFunc
		rcode     int
		wantRcode int
	}{
		{
			// miekg's own mux checks this; we are the mux. A question-less query
			// reaching a plugin is a panic further down.
			name:      "a query with no question is servfail, not a panic",
			msg:       new(dns.Msg),
			wantRcode: dns.RcodeServerFailure,
		},
		{
			// CoreDNS makes CHAOS configurable for version.bind; we serve no
			// such zone, so anything but IN is refused.
			name: "a class we do not serve is refused",
			msg: func() *dns.Msg {
				m := new(dns.Msg)
				m.Question = []dns.Question{{
					Name: "version.bind.", Qtype: dns.TypeTXT, Qclass: dns.ClassCHAOS,
				}}

				return m
			}(),
			wantRcode: dns.RcodeRefused,
		},
		{
			// A plugin panic is this query's problem and not the process's.
			name: "a panicking plugin is servfail and the process lives",
			msg:  query("web.incus.test"),
			chain: func(_ dns.ResponseWriter, _ *dns.Msg) {
				panic("plugins are ours, but they are still code")
			},
			wantRcode: dns.RcodeServerFailure,
		},
		{
			// The contract refuse{} relies on: return an rcode ClientWrite
			// calls unwritten, and a reply is written for you.
			name:      "a refusal nobody wrote gets a reply written for it",
			msg:       query("web.incus.test"),
			rcode:     dns.RcodeRefused,
			wantRcode: dns.RcodeRefused,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			a := &adapter{chain: &fakeChain{fn: tc.chain, rcode: tc.rcode}}

			w := &recorder{}

			assert.NotPanics(t, func() { a.ServeDNS(w, tc.msg) })

			require.NotNil(t, w.msg, "nothing was written")
			assert.Equal(t, tc.wantRcode, w.msg.Rcode)
		})
	}
}

// TestAdapterLeavesAnAnsweredQueryAlone pins the other half, an absence: an
// NXDOMAIN the plugin wrote itself must not be written over. ecs_view does that.
func TestAdapterLeavesAnAnsweredQueryAlone(t *testing.T) {
	t.Parallel()

	written := new(dns.Msg)
	written.SetRcode(query("web.incus.test"), dns.RcodeNameError)
	written.Ns = []dns.RR{&dns.SOA{Hdr: dns.RR_Header{
		Name: "incus.test.", Rrtype: dns.TypeSOA, Class: dns.ClassINET, Ttl: 30,
	}}}

	a := &adapter{chain: &fakeChain{
		fn:    func(w dns.ResponseWriter, _ *dns.Msg) { _ = w.WriteMsg(written) },
		rcode: dns.RcodeNameError,
	}}

	w := &recorder{}
	a.ServeDNS(w, query("web.incus.test"))

	require.NotNil(t, w.msg)
	assert.Equal(t, dns.RcodeNameError, w.msg.Rcode)

	// The authority section survives, so the adapter wrote no bare rcode of its
	// own over it.
	assert.Len(t, w.msg.Ns, 1)
}

// fakeChain stands in for the plugin chain, writing whatever fn writes and
// reporting rcode. A nil fn is the case the adapter has to finish for.
type fakeChain struct {
	fn    dns.HandlerFunc
	rcode int
}

func (f *fakeChain) Name() string { return "fake" }

func (f *fakeChain) ServeDNS(_ context.Context, w dns.ResponseWriter, r *dns.Msg) (int, error) {
	if f.fn != nil {
		f.fn(w, r)
	}

	return f.rcode, nil
}
