package ecs_view

import (
	"context"
	"log/slog"
	"net"
	"net/netip"

	"github.com/coredns/coredns/request"
	"github.com/miekg/dns"
)

// staleTTL caps the TTL of answers served while a source is unhealthy, so stale
// records expire quickly once a client can reach a healthy server.
const staleTTL = 5

// levelTrace is shared.LevelTrace by value, which this may not import: the
// engine depends on nothing of ours. TestLevelTraceMatchesShared pins them.
const levelTrace = slog.LevelDebug - 4

// Answer resolves one query against the current snapshot and writes the reply.
// handled=false is the caller's cue to hand the request to the next plugin.
func (v *ECSView) Answer(ctx context.Context, w dns.ResponseWriter, r *dns.Msg) (bool, int, error) {
	state := request.Request{W: w, Req: r}
	qname := state.Name()
	qtype := state.QType()

	// The one place holding the option: everything downstream wants an address
	// to look up rather than a wire encoding.
	var (
		client     netip.Addr
		haveClient bool
		echo       *dns.EDNS0_SUBNET
		echoOpt    *dns.OPT
	)

	opt := r.IsEdns0()
	if opt != nil {
		for _, o := range opt.Option {
			subnet, ok := o.(*dns.EDNS0_SUBNET)
			if !ok {
				continue
			}

			// A resolver forwarding with add-subnet=32,128 puts the client's own
			// address here rather than only its network.
			client, haveClient = netip.AddrFromSlice(subnet.Address)
			client = client.Unmap()

			// RFC 7871: the option is echoed as it arrived, with SCOPE written
			// into it below - the only signal a cache before this one has.
			if v.EchoSubnet {
				echo = subnet
				echoOpt = replyOPT(opt, subnet)
			}

			break
		}
	}

	// Nothing relayed the query, so the source address is the querier: what a
	// client naming this server in resolv.conf sends.
	if !haveClient {
		client, haveClient = sourceAddr(w.RemoteAddr())
	}

	snap := v.current.Load()

	// Zones first, and the zone tree is zones deep rather than names deep: a name
	// outside every zone is the common case and never touches the name tree.
	z := snap.ZoneOf(qname)
	if z == nil {
		slog.Debug("unknown, falling through", "zone", qname)

		return false, dns.RcodeSuccess, nil
	}

	answers, held := snap.Answers(qname)

	// A zone claiming only the names in it, the apex included: naming a server
	// for a domain we hold one name in would claim the rest of that domain too.
	if z.Shadowing && !held {
		slog.Debug("not claimed, falling through", "name", qname, "zone", z.Name)

		return false, dns.RcodeSuccess, nil
	}

	server := v.Server

	// Records are shared by every query on the view, so only the clamp copies.
	clamp := !v.healthy.Load()

	m := new(dns.Msg)
	m.SetReply(r)
	m.Authoritative = true

	if echoOpt != nil {
		m.Extra = replyExtra(r, echoOpt)
	}

	if qname == z.Name {
		answerApex(m, qtype, z, clamp)

		return write(w, m)
	}

	// Everything below turns on who is asking, so the answer is valid for that
	// address alone. The apex above leaves the scope at 0: shareable fleet-wide.
	if echo != nil {
		echo.SourceScope = answerScope(echo.Family)
	}

	if !held {
		deny(m, z, clamp)

		if v.Metrics {
			requestCount.WithLabelValues(server, "nxdomain").Inc()
		}

		return write(w, m)
	}

	// Fail closed. Without an address there is no network to answer from, and any
	// view picked here would hand out records it cannot route to.
	var (
		view  ViewID
		known bool
	)

	if haveClient {
		view, known = snap.ViewFor(client)
	}

	if !known || view == AmbiguousView {
		if v.Metrics {
			deniedCount.WithLabelValues(server).Inc()
			requestCount.WithLabelValues(server, "denied").Inc()
		}

		deny(m, z, clamp)

		return write(w, m)
	}

	byType, visible := answers[view]

	// Two instances claiming one alias each wrote their own, and a name may hold
	// only one CNAME. Refusing beats picking, and it reads as missing: whichever
	// claimant leaves first frees the name for the other with nothing tracking
	// them.
	if len(byType[dns.TypeCNAME]) > 1 {
		visible = false
	}

	// Unreachable reads exactly like missing.
	if !visible {
		deny(m, z, clamp)

		if v.Metrics {
			requestCount.WithLabelValues(server, "nxdomain").Inc()
		}

		return write(w, m)
	}

	rrs, has := byType[qtype]
	if !has {
		// The name is here, this type is not.
		m.Ns = []dns.RR{authority(z.SOA, clamp)}

		if v.Metrics {
			requestCount.WithLabelValues(server, "nodata").Inc()
		}

		return write(w, m)
	}

	m.Answer = withTTL(rrs, clamp)

	if v.Metrics {
		requestCount.WithLabelValues(server, "success").Inc()
	}

	return write(w, m)
}

// replyOPT is the OPT record the echoed client subnet rides back on. A query
// carrying nothing else lends its own record; otherwise a fresh one holds the subnet alone.
func replyOPT(req *dns.OPT, echo *dns.EDNS0_SUBNET) *dns.OPT {
	if len(req.Option) == 1 {
		do := req.Do()

		req.Hdr.Name = "."
		req.Hdr.Ttl = 0

		if do {
			req.SetDo()
		}

		return req
	}

	out := &dns.OPT{
		Hdr:    dns.RR_Header{Name: ".", Rrtype: dns.TypeOPT},
		Option: []dns.EDNS0{echo},
	}

	out.SetUDPSize(req.UDPSize())
	out.SetDo(req.Do())

	return out
}

// replyExtra is the reply's additional section, the OPT record and nothing else.
// A request holding only that lends its slice, capped so an append reallocates.
func replyExtra(r *dns.Msg, echoOpt *dns.OPT) []dns.RR {
	if len(r.Extra) == 1 && r.Extra[0] == dns.RR(echoOpt) {
		return r.Extra[:1:1]
	}

	return []dns.RR{echoOpt}
}

// answerScope is the querier's whole address, because a different address is a
// different view. RFC 7871 has FAMILY 0 stay at 0.
func answerScope(family uint16) uint8 {
	switch family {
	case 1:
		return 32
	case 2:
		return 128
	}

	return 0
}

// sourceAddr is the address a query arrived from. A client subnet wins over it,
// because a forwarding resolver puts its own address here for every client.
func sourceAddr(a net.Addr) (netip.Addr, bool) {
	var ip net.IP

	switch v := a.(type) {
	case *net.UDPAddr:
		ip = v.IP
	case *net.TCPAddr:
		ip = v.IP
	default:
		return netip.Addr{}, false
	}

	addr, ok := netip.AddrFromSlice(ip)
	if !ok {
		return netip.Addr{}, false
	}

	return addr.Unmap(), true
}

// withTTL hands the stored records to a reply. They are shared by every query on
// the view, so the normal path reslices and only the stale clamp copies.
func withTTL(rrs []dns.RR, clamp bool) []dns.RR {
	if !clamp {
		// Capped, so a later append cannot write into the snapshot's array.
		return rrs[:len(rrs):len(rrs)]
	}

	out := make([]dns.RR, len(rrs))
	for i, rr := range rrs {
		out[i] = authority(rr, true)
	}

	return out
}

// authority clamps one shared record, copying only when it has to: a record
// already under the cap is handed back as it stands.
func authority(rr dns.RR, clamp bool) dns.RR {
	if !clamp || rr.Header().Ttl <= staleTTL {
		return rr
	}

	out := dns.Copy(rr)
	out.Header().Ttl = staleTTL

	return out
}

// write is the single point that puts a message on the wire, returning the
// triple Answer reports so every answering branch ends the same way.
func write(w dns.ResponseWriter, m *dns.Msg) (bool, int, error) {
	err := w.WriteMsg(m)
	if err != nil {
		return true, dns.RcodeServerFailure, err
	}

	return true, dns.RcodeSuccess, nil
}

// deny turns a lookup miss into NXDOMAIN, so a name the querier cannot reach
// reads exactly like one that does not exist.
func deny(m *dns.Msg, z *Zone, clamp bool) {
	m.Rcode = dns.RcodeNameError
	m.Ns = []dns.RR{authority(z.SOA, clamp)}
}

// answerApex answers queries for the zone name itself.
func answerApex(m *dns.Msg, qtype uint16, z *Zone, clamp bool) {
	switch qtype {
	case dns.TypeSOA:
		m.Answer = []dns.RR{authority(z.SOA, clamp)}
	case dns.TypeNS:
		m.Answer = withTTL(z.NS, clamp)
	default:
		m.Ns = []dns.RR{authority(z.SOA, clamp)}
	}
}
