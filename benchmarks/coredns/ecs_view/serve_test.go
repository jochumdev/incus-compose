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

// benchSnapshot builds zones x replicas hosts as a source publishes them: one
// zone per project, two networks each, every host on both of them and a service
// name fanning out to every replica behind it.
//
// The exported helpers do the work - Render renders, ViewOf names the view,
// Gather assembles it - so this is the shape a source produces rather than a
// second implementation of one.
func benchSnapshot(zones, replicas int) *ecs_view.Snapshot {
	snap := &ecs_view.Snapshot{
		ByZone: make(map[string]*ecs_view.Zone, zones),
		ByAddr: make(map[netip.Addr]ecs_view.ViewID, 2*zones*replicas),
		Views:  make(map[ecs_view.ViewID]map[string]ecs_view.RRSets, zones),
		Nets:   make([]ecs_view.NetEntry, 0, 2*zones),
		TTL:    benchTTL,
	}

	for z := range zones {
		zoneName := fmt.Sprintf("proj%02d.incus.", z)

		// Every project gets its own pair of /24s. Overlapping them would collide
		// in the address index and silently turn these into NXDOMAIN measurements.
		api := fmt.Sprintf("10.%d.%d", z/256, z%256)
		data := fmt.Sprintf("172.%d.%d", 16+z/256, z%256)

		apiKey := zoneName + "api"
		dataKey := zoneName + "data"
		keys := []string{apiKey, dataKey}
		view := ecs_view.ViewOf(keys)

		snap.Nets = append(snap.Nets,
			ecs_view.NetEntry{Prefix: netip.MustParsePrefix(api + ".0/24"), Key: apiKey},
			ecs_view.NetEntry{Prefix: netip.MustParsePrefix(data + ".0/24"), Key: dataKey},
		)

		names := make(map[string]map[string]ecs_view.RRSets, replicas+1)

		onAPI := make([]netip.Addr, 0, replicas)
		onData := make([]netip.Addr, 0, replicas)

		for n := range replicas {
			first := netip.MustParseAddr(fmt.Sprintf("%s.%d", api, n+1))
			second := netip.MustParseAddr(fmt.Sprintf("%s.%d", data, n+1))

			host := fmt.Sprintf("web-%d.%s", n, zoneName)
			names[host] = map[string]ecs_view.RRSets{
				apiKey:  ecs_view.Render(host, []netip.Addr{first}, nil, benchTTL),
				dataKey: ecs_view.Render(host, []netip.Addr{second}, nil, benchTTL),
			}

			onAPI = append(onAPI, first)
			onData = append(onData, second)

			// A host asks from either of its own addresses and sees the same
			// thing, because the view is the set of networks it sits on.
			snap.ByAddr[first] = view
			snap.ByAddr[second] = view
		}

		service := "web." + zoneName
		names[service] = map[string]ecs_view.RRSets{
			apiKey:  ecs_view.Render(service, onAPI, nil, benchTTL),
			dataKey: ecs_view.Render(service, onData, nil, benchTTL),
		}

		snap.ByZone[zoneName] = &ecs_view.Zone{Names: names, Serial: 1}

		// Every host of a project sits on both of its networks, so one view covers
		// the project and reaches that project's names alone.
		gathered := make(map[string]ecs_view.RRSets, len(names))

		for name, perNet := range names {
			got, visible := ecs_view.Gather(perNet, keys)
			if !visible {
				continue
			}

			gathered[name] = got
		}

		snap.Views[view] = gathered
	}

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

				v.Replace(benchSnapshot(c.zones, c.replicas))
				v.SetHealthy(true)

				ctx := context.Background()
				w := dnstest.NewRecorder(&test.ResponseWriter{})

				// A service name in the last zone, asked by one of its own
				// replicas.
				qname := fmt.Sprintf("web.proj%02d.incus.", c.zones-1)
				client := fmt.Sprintf("10.%d.%d.1", (c.zones-1)/256, (c.zones-1)%256)
				req := query(qname, dns.TypeA, client)

				b.Run(fmt.Sprintf("zones=%d_replicas=%d", c.zones, c.replicas), func(b *testing.B) {
					// A run that measures NXDOMAIN measures nothing, and one
					// where the option never got attached measures the wrong
					// half of the pair, so both are checked before timing.
					_, err := v.ServeDNS(ctx, w, req)
					if err != nil {
						b.Fatal(err)
					}

					// The querier shares both networks with every replica, so it
					// sees each one twice: once per network.
					want := 2 * c.replicas
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

// cases is the shared shape every side is measured at, see ../benchmark.md.
var cases = []struct{ zones, replicas int }{
	{1, 1}, {1, 100}, {50, 1}, {50, 20}, {500, 1}, {500, 20},
}
