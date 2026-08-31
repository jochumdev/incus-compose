package dns

import (
	"context"
	"log/slog"
	"net"
	"net/netip"
	"slices"
	"sort"
	"strings"
	"sync/atomic"

	"github.com/coredns/coredns/plugin"
	"github.com/coredns/coredns/request"
	"github.com/miekg/dns"

	"github.com/lxc/incus-compose/ievent/dns/ecs_view"
)

// xfrName is what this handler is called in the chain.
const xfrName = "xfr"

// maxEnvelope is how much of one transfer message is filled before another
// starts, leaving room under the 64 KiB wire limit for framing and a TSIG record.
const maxEnvelope = 63000

// xfr answers AXFR and IXFR, and holds a snapshot of its own to answer them
// from.
//
// The engine is handed the same snapshot separately rather than read through:
// a transfer is the one answer not filtered by who is asking, so it derives what
// the query path is built not to, and one atomic.Pointer with one writer and
// many readers is cheaper than a second reader of somebody else's.
type xfr struct {
	Next plugin.Handler

	// allow is who may ask. Empty allows nobody, so a transfer is opt-in at the
	// listener as well as at the zone.
	allow []netip.Prefix

	current atomic.Pointer[ecs_view.Snapshot]
}

// newXFR returns a handler already holding an empty snapshot, so a transfer
// arriving before the first publish is refused rather than answered from nil.
func newXFR(allow []netip.Prefix) *xfr {
	x := &xfr{allow: allow}
	x.current.Store(ecs_view.EmptySnapshot())

	return x
}

// Name implements plugin.Handler.
func (x *xfr) Name() string { return xfrName }

// Replace takes the published snapshot. The same value the engine is given, and
// neither reads the other's.
func (x *xfr) Replace(snap *ecs_view.Snapshot) { x.current.Store(snap) }

// ServeDNS answers a transfer and hands everything else on: the ordinary query
// path is the engine's, one position along.
func (x *xfr) ServeDNS(ctx context.Context, w dns.ResponseWriter, r *dns.Msg) (int, error) {
	state := request.Request{W: w, Req: r}

	if state.QType() != dns.TypeAXFR && state.QType() != dns.TypeIXFR {
		return plugin.NextOrFailure(x.Name(), x.Next, ctx, w, r)
	}

	return x.transfer(state)
}

// transfer answers from the current snapshot. A zone goes over whole or not at
// all, so every gate is checked before the snapshot is read.
func (x *xfr) transfer(state request.Request) (int, error) {
	w, r := state.W, state.Req

	// A transfer is a stream of messages and UDP carries one message.
	if state.Proto() != "tcp" {
		return dns.RcodeRefused, nil
	}

	peer, known := peerAddr(w.RemoteAddr())
	if !known || !x.allowed(peer) {
		return dns.RcodeRefused, nil
	}

	// Nil while no secrets are configured; once there are, this is the only
	// assertion of who the peer is, reported rather than dropped on failure.
	err := w.TsigStatus()
	if err != nil {
		return dns.RcodeRefused, err
	}

	snap := x.current.Load()

	zoneName := state.Name()

	// An apex or nothing: ZoneOf takes the longest, so a name inside a zone
	// answers with the zone holding it and is refused here rather than served.
	// A Shadowing zone was invented for one alias, and handing it over would
	// make the secondary authoritative for a domain we hold one name in.
	z := snap.ZoneOf(zoneName)
	if z == nil || z.Name != zoneName || !z.Transfer || z.Shadowing {
		return dns.RcodeRefused, nil
	}

	apex, rendered := z.SOA.(*dns.SOA)
	if !rendered {
		return dns.RcodeServerFailure, nil
	}

	var serial uint32

	if state.QType() == dns.TypeIXFR {
		if len(r.Ns) != 1 {
			return dns.RcodeServerFailure, nil
		}

		asked, isSOA := r.Ns[0].(*dns.SOA)
		if !isSOA {
			return dns.RcodeServerFailure, nil
		}

		serial = asked.Serial
	}

	slog.Debug("Transfer", "zone", zoneName, "qtype", state.QType(), "peer", peer,
		"serial", serial, "have", apex.Serial)

	// Already current. There is no journal here to cut a delta from, so an
	// older serial takes the whole zone instead.
	if serial != 0 && serial == apex.Serial {
		m := new(dns.Msg)
		m.SetReply(r)
		m.Authoritative = true
		m.Answer = []dns.RR{apex}

		err = w.WriteMsg(m)
		if err != nil {
			return dns.RcodeServerFailure, err
		}

		return dns.RcodeSuccess, nil
	}

	// Everything is rendered already, so the envelopes are cut and the channel
	// closed before Out reads any of it - no producer goroutine to hang on a peer.
	ch := envelopes(zoneRecords(snap, z, apex))

	err = new(dns.Transfer).Out(w, r, ch)
	if err != nil {
		return dns.RcodeServerFailure, err
	}

	return dns.RcodeSuccess, nil
}

// allowed reports whether peer is one the operator named.
func (x *xfr) allowed(peer netip.Addr) bool {
	for _, prefix := range x.allow {
		if prefix.Contains(peer) {
			return true
		}
	}

	return false
}

// peerAddr is the address a transfer arrived from. There is no client-subnet
// case here: a transfer is answered for the peer itself or for nobody.
func peerAddr(a net.Addr) (netip.Addr, bool) {
	tcp, ok := a.(*net.TCPAddr)
	if !ok {
		return netip.Addr{}, false
	}

	addr, ok := netip.AddrFromSlice(tcp.IP)
	if !ok {
		return netip.Addr{}, false
	}

	return addr.Unmap(), true
}

// zoneRecords is the zone as a transfer carries it: apex SOA, NS, every name in
// it, then SOA again.
func zoneRecords(snap *ecs_view.Snapshot, z *ecs_view.Zone, apex dns.RR) []dns.RR {
	out := []dns.RR{apex}
	out = append(out, z.NS...)

	// Keyed root-first, so a zone is a byte-wise prefix of the names in it and
	// the walk is already in one deterministic order - two transfers of an
	// unchanged zone are the same byte stream without sorting anything here.
	snap.Tree.Root().WalkPrefix(ecs_view.NameKey(z.Name), func(key []byte, answer ecs_view.ViewAnswer) bool {
		name := keyName(key)

		// The apex is written above, and a child zone's names are a prefix match
		// too: without this the parent carries what it never delegated.
		inside := snap.ZoneOf(name)
		if name == z.Name || inside == nil || inside.Name != z.Name {
			return false
		}

		sets := gather(answer)

		// An alias shares its name with the records it was chased into, so a
		// zone may not hold both sets; on the wire the CNAME goes alone.
		cname, aliased := sets[dns.TypeCNAME]
		if aliased {
			out = append(out, cname...)

			return false
		}

		types := make([]uint16, 0, len(sets))
		for qtype := range sets {
			types = append(types, qtype)
		}

		slices.Sort(types)

		for _, qtype := range types {
			out = append(out, sets[qtype]...)
		}

		return false
	})

	return append(out, apex)
}

// gather folds every view's records for one name together. It is the one answer
// not filtered by who is asking: a serial is per zone rather than per view, and
// two secondaries handed different records under one could never tell.
func gather(answer ecs_view.ViewAnswer) ecs_view.Records {
	out := ecs_view.Records{}
	seen := map[string]bool{}

	for _, records := range answer {
		for qtype, rrs := range records {
			for _, rr := range rrs {
				text := rr.String()
				if seen[text] {
					continue
				}

				seen[text] = true
				out[qtype] = append(out[qtype], rr)
			}
		}
	}

	// A view is a map key, so what came out above is in no order at all.
	for _, rrs := range out {
		sort.Slice(rrs, func(i, j int) bool { return rrs[i].String() < rrs[j].String() })
	}

	return out
}

// keyName turns a tree key back into the name it was made from, which is what
// walking a zone hands back. NameKey reversed the labels; this reverses them again.
func keyName(key []byte) string {
	labels := strings.Split(strings.TrimSuffix(string(key), "."), ".")
	slices.Reverse(labels)

	return strings.Join(labels, ".") + "."
}

// envelopes cuts records into messages that fit the wire, on a channel already
// closed: everything here came from an immutable snapshot, so nothing is left to produce.
func envelopes(rrs []dns.RR) chan *dns.Envelope {
	var (
		out   []*dns.Envelope
		batch []dns.RR
		size  int
	)

	for _, rr := range rrs {
		n := dns.Len(rr)

		if len(batch) > 0 && size+n > maxEnvelope {
			out = append(out, &dns.Envelope{RR: batch})
			batch = nil
			size = 0
		}

		batch = append(batch, rr)
		size += n
	}

	if len(batch) > 0 {
		out = append(out, &dns.Envelope{RR: batch})
	}

	ch := make(chan *dns.Envelope, len(out))
	for _, e := range out {
		ch <- e
	}

	close(ch)

	return ch
}
