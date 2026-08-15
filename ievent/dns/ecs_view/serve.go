package ecs_view

import (
	"context"
	"log/slog"
	"net"
	"net/netip"

	"github.com/coredns/coredns/request"
	"github.com/miekg/dns"

	"github.com/lxc/incus-compose/shared"
)

// staleTTL caps the TTL of answers served while a source is unhealthy, so stale
// records expire quickly once a client can reach a healthy server.
const staleTTL = 5

// Answer resolves one query against the current snapshot and writes the reply.
// handled=false is the caller's cue to hand the request to the next plugin.
func (v *ECSView) Answer(ctx context.Context, w dns.ResponseWriter, r *dns.Msg) (bool, int, error) {
	state := request.Request{W: w, Req: r}
	qname := state.Name()
	qtype := state.QType()

	// A transfer writes a stream of messages, so it cannot take the path below,
	// which ends at a single write by construction. Never falls through: a zone
	// this server does not serve is refused rather than forwarded.
	if qtype == dns.TypeAXFR || qtype == dns.TypeIXFR {
		code, err := v.transfer(state)

		return true, code, err
	}

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
			// into it below - the only signal a cache in front has.
			if v.EchoSubnet {
				echo = subnet
				echoOpt = replyOPT(opt, subnet)
			}

			break
		}
	}

	via := "client-subnet"

	// Nothing relayed the query, so the source address is the querier: what a
	// client naming this server in resolv.conf sends.
	if !haveClient {
		via = "source-address"
		client, haveClient = sourceAddr(w.RemoteAddr())
	}

	snap := v.current.Load()

	zoneName, z := snap.MatchZone(qname)
	if z == nil {
		slog.Debug("unknown, falling through", "zone", qname)

		return false, dns.RcodeSuccess, nil
	}

	// A zone claiming only the names in it. Held is the test, never visible: a
	// name this querier may not see is answered below under the ordinary rule.
	if z.Fallthrough {
		_, ours := z.Names[qname]
		if !ours {
			slog.Debug("not claimed, falling through", "name", qname, "zone", zoneName)

			return false, dns.RcodeSuccess, nil
		}
	}

	server := v.Server

	// What the snapshot was rendered with, so records reach a reply without
	// copying. Only the stale clamp wants a different one.
	ttl := snap.TTL
	if !v.healthy.Load() && ttl > staleTTL {
		ttl = staleTTL
	}

	m := new(dns.Msg)
	m.SetReply(r)
	m.Authoritative = true

	if echoOpt != nil {
		m.Extra = replyExtra(r, echoOpt)
	}

	if qname == zoneName {
		answerApex(m, qtype, zoneName, z.Serial, ttl)

		return write(w, m)
	}

	// Everything below turns on who is asking, so the answer is valid for that
	// address alone. The apex above leaves the scope at 0: shareable fleet-wide.
	if echo != nil {
		echo.SourceScope = answerScope(echo.Family)
	}

	// Fail closed. Without a client address there is no way to tell what the
	// querier may see, and answering everything would leak across networks.
	var (
		view  ViewID
		known bool
	)

	if haveClient {
		view, known = snap.ViewFor(client)
	}

	if !known {
		if v.Metrics {
			deniedCount.WithLabelValues(server).Inc()
			requestCount.WithLabelValues(server, "denied").Inc()
		}

		deny(m, zoneName, z.Serial, ttl)

		slog.Log(ctx, shared.LevelTrace, "Denied", "code", dns.RcodeToString[m.Rcode], "client", client, "type", dns.TypeToString[qtype], "query", qname, "via", via, "zone", zoneName, "known", known, "view", view)
		return write(w, m)
	}

	found := snap.Resolve(qname, qtype, view)

	switch found.Result {
	case NameError:
		deny(m, zoneName, z.Serial, ttl)
	case NoData:
		m.Ns = []dns.RR{soa(zoneName, z.Serial, ttl)}
	case Success:
		m.Answer = withTTL(found.RRs, snap.TTL, ttl)
	}

	// One counter for the three, keyed by the same name the line above logs, so
	// the switch is about the answer alone.
	if v.Metrics {
		requestCount.WithLabelValues(server, resultName(found.Result)).Inc()
	}

	slog.Log(ctx, shared.LevelTrace, "Answer", "code", dns.RcodeToString[m.Rcode], "client", client, "type", dns.TypeToString[qtype], "query", qname, "via", via, "zone", zoneName, "known", known, "view", view)
	return write(w, m)
}

// replyOPT is the OPT record the echoed client subnet rides back on. A query
// carrying nothing else lends its own record, allocating nothing.
//
// Anything else it brought - a cookie, padding, a keepalive - is the query's,
// so that case gets a record of its own with the subnet alone on it.
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

// withTTL hands the stored records to a reply. They are iutil by every query
// on the view, so the normal path reslices and only the stale clamp copies.
func withTTL(rrs []dns.RR, built, want uint32) []dns.RR {
	if want == built {
		// Capped, so a later append cannot write into the snapshot's array.
		return rrs[:len(rrs):len(rrs)]
	}

	out := make([]dns.RR, len(rrs))
	for i, rr := range rrs {
		out[i] = dns.Copy(rr)
		out[i].Header().Ttl = want
	}

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
func deny(m *dns.Msg, zoneName string, serial, ttl uint32) {
	m.Rcode = dns.RcodeNameError
	m.Ns = []dns.RR{soa(zoneName, serial, ttl)}
}

// answerApex answers queries for the zone name itself.
func answerApex(m *dns.Msg, qtype uint16, zoneName string, serial, ttl uint32) {
	switch qtype {
	case dns.TypeSOA:
		m.Answer = []dns.RR{soa(zoneName, serial, ttl)}
	case dns.TypeNS:
		m.Answer = []dns.RR{nsRecord(zoneName, ttl)}
	default:
		m.Ns = []dns.RR{soa(zoneName, serial, ttl)}
	}
}

// nsRecord is the zone's own NS. A transfer carries it too, and the name a
// secondary is handed may not differ from the one a query is answered with.
func nsRecord(zoneName string, ttl uint32) *dns.NS {
	return &dns.NS{
		Hdr: dns.RR_Header{Name: zoneName, Rrtype: dns.TypeNS, Class: dns.ClassINET, Ttl: ttl},
		Ns:  "ns.dns." + zoneName,
	}
}

// soa synthesizes the zone's SOA, with the zone's own serial.
func soa(zoneName string, serial, ttl uint32) *dns.SOA {
	return &dns.SOA{
		Hdr:     dns.RR_Header{Name: zoneName, Rrtype: dns.TypeSOA, Class: dns.ClassINET, Ttl: ttl},
		Ns:      "ns.dns." + zoneName,
		Mbox:    "hostmaster." + zoneName,
		Serial:  serial,
		Refresh: 7200,
		Retry:   1800,
		Expire:  86400,
		Minttl:  ttl,
	}
}

// resultName is a Result as it reads in a log line.
func resultName(r Result) string {
	switch r {
	case Success:
		return "success"
	case NoData:
		return "nodata"
	case NameError:
		return "nxdomain"
	}

	return "unknown"
}
