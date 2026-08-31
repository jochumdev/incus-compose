package dns

import (
	"net"
	"testing"

	incusapi "github.com/lxc/incus/v7/shared/api"
	"github.com/miekg/dns"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lxc/incus-compose/ievent/iutil"
)

// answering builds a plugin serving one instance in one project, after the
// adapter every query really arrives through - unlike calling Answer directly.
func answering(t *testing.T, echo bool) *adapter {
	t.Helper()

	p := New(EchoSubnet(echo), TTL(30), Suffix("incus"))

	// Answering is all this needs, so it never runs: a successor to hand each
	// event to, and no chain after it.
	p.next = func(_ *iutil.Event) {}

	p.fold(enriched(incusapi.EventLifecycleInstanceStarted, "shop", "web", "10.0.0.2"))
	p.fold(event(iutil.ActionSweepEnd, "", ""))

	// Nothing after it, which is what a deployment with no --forward runs.
	wire(p.xfr, p.view, nil)

	return &adapter{chain: p.view}
}

// subnetQuery is one A question carrying a client subnet, plus whatever else the
// resolver before it would have put on the same OPT record.
func subnetQuery(name, from string, also ...dns.EDNS0) *dns.Msg {
	m := new(dns.Msg)
	m.SetQuestion(dns.Fqdn(name), dns.TypeA)
	m.SetEdns0(1232, false)

	opt := m.IsEdns0()
	opt.Option = append([]dns.EDNS0{&dns.EDNS0_SUBNET{
		Code:          dns.EDNS0SUBNET,
		Family:        1,
		SourceNetmask: 32,
		Address:       net.ParseIP(from),
	}}, also...)

	return m
}

// replySubnet is the client subnet a reply carries, or nil when it carries none.
func replySubnet(m *dns.Msg) *dns.EDNS0_SUBNET {
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

// TestEchoReachesTheWire pins the engine's scope through the adapter: upstream's
// ScrubWriter rewrites every OPT record, so what ecs_view builds and a client gets can differ.
func TestEchoReachesTheWire(t *testing.T) {
	t.Parallel()

	a := answering(t, true)
	w := &recorder{}

	a.ServeDNS(w, subnetQuery("web.shop.incus.", "10.0.0.2"))

	require.NotNil(t, w.msg)
	require.Equal(t, dns.RcodeSuccess, w.msg.Rcode)
	require.Len(t, w.msg.Answer, 1, "the client subnet decides who is asking")

	got := replySubnet(w.msg)
	require.NotNil(t, got, "the echo did not survive the writer")

	assert.EqualValues(t, 32, got.SourceScope, "this answer is valid for one address")
	assert.EqualValues(t, 32, got.SourceNetmask)
	assert.Equal(t, "10.0.0.2", got.Address.String())
}

// TestNoEchoWithoutTheFlag pins the default: the reply still carries an OPT
// record via ScrubWriter, but never a subnet.
func TestNoEchoWithoutTheFlag(t *testing.T) {
	t.Parallel()

	a := answering(t, false)
	w := &recorder{}

	a.ServeDNS(w, subnetQuery("web.shop.incus.", "10.0.0.2"))

	require.NotNil(t, w.msg)
	require.Equal(t, dns.RcodeSuccess, w.msg.Rcode)
	require.Len(t, w.msg.Answer, 1, "the client subnet still decides who is asking")

	assert.Nil(t, replySubnet(w.msg), "echoed without --echo-subnet")
	assert.NotNil(t, w.msg.IsEdns0(), "the query's own OPT record comes back regardless")
}

// TestRefusalCarriesNoScope pins that a name outside every zone is refused
// identically for every querier; the reply says nothing about who asked.
func TestRefusalCarriesNoScope(t *testing.T) {
	t.Parallel()

	a := answering(t, true)
	w := &recorder{}

	a.ServeDNS(w, subnetQuery("www.example.org.", "10.0.0.2"))

	require.NotNil(t, w.msg)
	require.Equal(t, dns.RcodeRefused, w.msg.Rcode)

	assert.Nil(t, replySubnet(w.msg),
		"a refusal is not an answer this querier was given")
}

// TestEchoDecidesTheQuerysOtherOptions pins that echoing replaces the query's
// OPT record with one holding only the subnet, dropping other options like a cookie.
func TestEchoDecidesTheQuerysOtherOptions(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		echo bool
		want bool
	}{
		{"the query's own OPT comes back whole when nothing echoes", false, true},
		{"an echoing reply answers about the subnet alone", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			a := answering(t, tc.echo)
			w := &recorder{}

			cookie := &dns.EDNS0_COOKIE{Code: dns.EDNS0COOKIE, Cookie: "24616263646566"}

			a.ServeDNS(w, subnetQuery("web.shop.incus.", "10.0.0.2", cookie))

			require.NotNil(t, w.msg)
			require.Equal(t, dns.RcodeSuccess, w.msg.Rcode)

			opt := w.msg.IsEdns0()
			require.NotNil(t, opt)

			var kept bool

			for _, o := range opt.Option {
				_, ok := o.(*dns.EDNS0_COOKIE)
				if ok {
					kept = true
				}
			}

			assert.Equal(t, tc.want, kept, "the cookie decided the reply's other options")
		})
	}
}
