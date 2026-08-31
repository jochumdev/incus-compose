// Package ecsviewbench benchmarks the engine on its own: a snapshot handed
// straight to ecs_view, with no source in the picture. It isolates what
// answering costs and what echo_subnet adds to it; the kubernetes module beside
// it is the yardstick. See ../benchmark.md.
package ecsviewbench

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"testing"

	"github.com/coredns/coredns/plugin/pkg/dnstest"
	"github.com/coredns/coredns/plugin/test"
	iradix "github.com/hashicorp/go-immutable-radix/v2"
	"github.com/miekg/dns"

	"github.com/lxc/incus-compose/ievent/dns/ecs_view"
)

// benchTTL is what the snapshot is rendered with. Any value does, as long as the
// engine is healthy: a clamped TTL copies records and would measure that instead.
const benchTTL = 5

// query builds a request carrying an EDNS0 Client Subnet address the way dnsmasq
// does with add-subnet=32,128. The engine needs one to identify the querier, so
// every benchmarked query has one - which is also what makes echo_subnet visible
// in these numbers.
func query(qname string, qtype uint16, from string) *dns.Msg {
	m := new(dns.Msg)
	m.SetQuestion(qname, qtype)

	subnet := &dns.EDNS0_SUBNET{
		Code:          dns.EDNS0SUBNET,
		Address:       net.ParseIP(from),
		Family:        1,
		SourceNetmask: 32,
	}

	opt := &dns.OPT{Hdr: dns.RR_Header{Name: ".", Rrtype: dns.TypeOPT}}
	opt.Option = append(opt.Option, subnet)
	m.Extra = append(m.Extra, opt)

	return m
}

// renderA is one name's A records on one network, as a source renders them.
func renderA(name string, addrs []netip.Addr) ecs_view.Records {
	out := make([]dns.RR, 0, len(addrs))

	for _, addr := range addrs {
		out = append(out, &dns.A{
			Hdr: dns.RR_Header{Name: name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: benchTTL},
			A:   addr.AsSlice(),
		})
	}

	return ecs_view.Records{dns.TypeA: out}
}

// benchSnapshot builds zones x replicas hosts as a source publishes them: one
// zone per project, nets networks each, every host on all of them and a service
// name fanning out to every replica behind it.
//
// nets no longer decides whether answering joins - that moved to build time with
// the per-view index, so both settings answer by reference. It still decides how
// many addresses a name holds, which is what the reply size follows.
func benchSnapshot(zones, replicas, nets int) *ecs_view.Snapshot {
	snap := &ecs_view.Snapshot{}

	names := iradix.New[ecs_view.ViewAnswer]().Txn()
	denial := iradix.New[*ecs_view.Zone]().Txn()

	for z := range zones {
		zoneName := fmt.Sprintf("proj%02d.incus.", z)

		keys := make([]string, 0, nets)
		for n := range nets {
			keys = append(keys, fmt.Sprintf("%snet%d", zoneName, n))
		}

		// Every host of a project sits on every one of its networks, so one view
		// covers the project.
		view := ecs_view.ViewOf(keys)

		// A /24 of its own per network. Overlapping them would collide in the
		// address index and silently turn these into NXDOMAIN measurements.
		subnet := func(n int) string {
			at := z*nets + n

			return fmt.Sprintf("10.%d.%d", at/256, at%256)
		}

		denial.Insert(ecs_view.NameKey(zoneName), &ecs_view.Zone{
			Name: zoneName,
			SOA: &dns.SOA{
				Hdr:    dns.RR_Header{Name: zoneName, Rrtype: dns.TypeSOA, Class: dns.ClassINET, Ttl: benchTTL},
				Ns:     "ns.dns." + zoneName,
				Mbox:   "hostmaster." + zoneName,
				Serial: 1,
				Minttl: benchTTL,
			},
			NS: []dns.RR{&dns.NS{
				Hdr: dns.RR_Header{Name: zoneName, Rrtype: dns.TypeNS, Class: dns.ClassINET, Ttl: benchTTL},
				Ns:  "ns.dns." + zoneName,
			}},
		})

		for n := range nets {
			prefix := netip.MustParsePrefix(subnet(n) + ".0/24")

			key, _ := ecs_view.AddrKey(prefix)
			snap.ByIPv4.Set(key, view)
		}

		behind := make([]netip.Addr, 0, replicas*nets)

		for r := range replicas {
			host := fmt.Sprintf("web-%d.%s", r, zoneName)

			addrs := make([]netip.Addr, 0, nets)

			for n := range nets {
				addr := netip.MustParseAddr(fmt.Sprintf("%s.%d", subnet(n), r+1))
				addrs = append(addrs, addr)
				behind = append(behind, addr)

				// A host asks from any of its own addresses and sees the same
				// thing, because the view is the set of networks it sits on.
				key, _ := ecs_view.AddrKey(netip.PrefixFrom(addr, 32))
				snap.ByIPv4.Set(key, view)
			}

			names.Insert(ecs_view.NameKey(host), ecs_view.ViewAnswer{view: renderA(host, addrs)})
		}

		service := "web." + zoneName
		names.Insert(ecs_view.NameKey(service), ecs_view.ViewAnswer{view: renderA(service, behind)})
	}

	snap.Tree = names.Commit()
	snap.Denial = denial.Commit()

	return snap
}

// BenchmarkServeDNS is the hot path, at each of the engine's two optional
// costs. Every mode runs over identical data and answers identically, and both
// of the others share echo=off's baseline, so a difference against it is what
// that one option costs and nothing else.
//
// The zone count matters on its own because the query path has to find the zone
// a name belongs to, and the replica count matters because a service name fans
// out to every host behind it.
func BenchmarkServeDNS(b *testing.B) {
	for _, mode := range []struct {
		name    string
		echo    bool
		metrics bool
	}{
		{"echo=off", false, false},
		{"echo=on", true, false},
		{"metrics=on", false, true},
	} {
		b.Run(mode.name, func(b *testing.B) {
			for _, c := range cases {
				v := ecs_view.New()
				v.EchoSubnet = mode.echo
				v.Metrics = mode.metrics

				// What main labels the counters with, so the label lookup this
				// times is the one a deployment pays for.
				v.Server = ":53"

				v.Replace(benchSnapshot(c.zones, c.replicas, c.nets))
				v.SetHealthy(true)

				ctx := context.Background()
				w := dnstest.NewRecorder(&test.ResponseWriter{})

				// A service name in the last zone, asked by one of its own
				// replicas.
				qname := fmt.Sprintf("web.proj%02d.incus.", c.zones-1)
				at := (c.zones - 1) * c.nets
				client := fmt.Sprintf("10.%d.%d.1", at/256, at%256)
				req := query(qname, dns.TypeA, client)

				b.Run(fmt.Sprintf("nets=%d_zones=%d_replicas=%d", c.nets, c.zones, c.replicas), func(b *testing.B) {
					// A run that measures NXDOMAIN measures nothing, and one
					// where the option never got attached measures the wrong
					// half of the pair, so both are checked before timing.
					_, err := v.ServeDNS(ctx, w, req)
					if err != nil {
						b.Fatal(err)
					}

					// The querier shares every network with every replica, so it
					// sees each one once per network.
					want := c.nets * c.replicas
					if len(w.Msg.Answer) != want {
						b.Fatalf("%s answered %d records, want %d", qname, len(w.Msg.Answer), want)
					}

					if (w.Msg.IsEdns0() != nil) != mode.echo {
						b.Fatalf("%s: reply carries an OPT record %v, want %v", qname, w.Msg.IsEdns0() != nil, mode.echo)
					}

					b.ResetTimer()
					b.ReportAllocs()

					for range b.N {
						_, _ = v.ServeDNS(ctx, w, req)
					}
				})
			}
		})
	}
}

// cases is the shared shape every side is measured at, see ../benchmark.md. The
// nets=1 rows are what both standing fleets produce; nets=2 is the join.
var cases = []struct{ zones, replicas, nets int }{
	{1, 1, 1}, {1, 100, 1}, {50, 1, 1}, {50, 20, 1}, {500, 1, 1}, {500, 20, 1},
	{1, 1, 2}, {1, 100, 2}, {50, 20, 2}, {500, 20, 2},
}
