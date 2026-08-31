package dns

import (
	"net/netip"
	"slices"
	"sort"

	"github.com/miekg/dns"

	"github.com/lxc/incus-compose/ievent/dns/ecs_view"
)

// apply moves one instance from what the trees hold to what has arrived, taking
// back exactly what it put in. next nil is the instance gone.
func (s *state) apply(key string, next *instance, ttl uint32) {
	prev := s.held[key]

	// Dropped while its claim still stands, or the views it answered in are no
	// longer the views it is written under.
	if prev != nil {
		for name, answer := range s.contributions(prev, ttl) {
			s.dropName(name, answer)
		}

		s.moved(prev)

		for range prev.names {
			s.leave(prev.zone, prev.ns, ttl, true, prev.transfer)
		}

		for _, on := range prev.ptrs {
			s.leave(on.zone, prev.ns, ttl, true, prev.transfer)
		}

		for _, name := range prev.alias {
			s.leave(s.aliasZone(name), nil, ttl, false, false)
		}

		s.dropAddrs(prev)

		freed, gone := s.release(prev)
		s.forget(gone, ttl)
		s.dropPrefixes(prev, freed)
	}

	if next == nil {
		delete(s.held, key)

		return
	}

	own, fresh := s.view(next)

	s.held[key] = next

	for name, answer := range s.contributions(next, ttl) {
		s.addName(name, answer)
	}

	for range next.names {
		s.enter(next.zone, next.ns, ttl, true, next.transfer)
	}

	for _, on := range next.ptrs {
		s.enter(on.zone, next.ns, ttl, true, next.transfer)
	}

	// After the zones its own names made, so an alias inside one of them finds
	// it served rather than inventing a second.
	for _, name := range next.alias {
		s.enter(s.aliasZone(name), nil, ttl, false, false)
	}

	s.moved(next)
	s.addAddrs(next, own)
	s.backfill(fresh, ttl)
}

// moved marks every zone this instance has a name in, so publish knows which
// serials to step.
func (s *state) moved(inst *instance) {
	for _, zone := range append([]string{inst.zone}, s.aliasZones(inst)...) {
		held, known := s.serials[zone]
		if !known {
			continue
		}

		held.dirty = true
		s.serials[zone] = held
	}

	for _, on := range inst.ptrs {
		held, known := s.serials[on.zone]
		if !known {
			continue
		}

		held.dirty = true
		s.serials[on.zone] = held
	}
}

// aliasZones is the zone each of this instance's aliases lands in.
func (s *state) aliasZones(inst *instance) []string {
	out := make([]string, 0, len(inst.alias))
	for _, name := range inst.alias {
		out = append(out, s.aliasZone(name))
	}

	return out
}

// step moves the serial of every zone a patch touched, and renders it under the
// new one. Only while warm: a step is a claim the zone changed, and cold has
// confirmed nothing, so it has no standing to make one.
func (s *state) step(warm bool, ttl uint32) {
	for name, held := range s.serials {
		if held.names <= 0 {
			delete(s.serials, name)
			s.zones.Delete(ecs_view.NameKey(name))

			continue
		}

		if !held.dirty {
			continue
		}

		held.dirty = false

		switch {
		case held.Serial == 0:
			// Its first publish is a birth rather than a step, and a secondary
			// takes zero as "no zone here".
			held.Serial = 1
		case warm:
			held.Serial++
		}

		s.serials[name] = held
		s.render(name, ttl)
	}
}

// aliasZone is the zone one alias lands in: the longest served, otherwise its
// own parent. Decided on arrival, so a longer zone appearing later leaves this
// count stale - ZoneOf takes the longest, so the answers are not.
func (s *state) aliasZone(name string) string {
	zone, _ := aliasZone(name, func(zone string) bool {
		_, served := s.serials[zone]

		return served
	})

	return zone
}

// contributions is every name this instance answers to and what it puts under
// each: its own and its service name, the aliases it claims, and the reverse
// names its addresses answer to.
func (s *state) contributions(inst *instance, ttl uint32) map[string]ecs_view.ViewAnswer {
	out := make(map[string]ecs_view.ViewAnswer, len(inst.names)+len(inst.alias)+len(inst.ptrs))

	for _, name := range inst.names {
		out[name] = s.records(inst, name, ttl)
	}

	for _, name := range inst.alias {
		out[name] = s.alias(inst, name, ttl)
	}

	for name, on := range inst.ptrs {
		out[name] = s.reverse(inst, name, on.key, ttl)
	}

	return out
}

// backfill writes every name already on these wires under a view that did not
// exist until now. The rare path, and the only one that is not one instance's:
// a view is a set of networks, so everything reachable in it answers there.
func (s *state) backfill(fresh []ecs_view.ViewID, ttl uint32) {
	for _, id := range fresh {
		for _, inst := range s.on(ecs_view.KeysOf(id)) {
			for name, answer := range s.contributions(inst, ttl) {
				records, answers := answer[id]
				if !answers {
					continue
				}

				s.addName(name, ecs_view.ViewAnswer{id: records})
			}
		}
	}
}

// forget takes a view nothing is left in off every name that answered there.
func (s *state) forget(gone []ecs_view.ViewID, ttl uint32) {
	for _, id := range gone {
		for _, inst := range s.on(ecs_view.KeysOf(id)) {
			for name := range s.contributions(inst, ttl) {
				s.dropView(name, id)
			}
		}
	}
}

// on is every instance held on any of these networks.
func (s *state) on(keys []string) []*instance {
	var out []*instance

	for _, inst := range s.held {
		for _, key := range keys {
			_, sits := inst.nets[key]
			if sits {
				out = append(out, inst)

				break
			}
		}
	}

	return out
}

// dropView takes one view off a name entirely, and the name with it where that
// was the only view answering.
func (s *state) dropView(name string, id ecs_view.ViewID) {
	key := ecs_view.NameKey(name)

	held, ok := s.names.Get(key)
	if !ok {
		return
	}

	_, answers := held[id]
	if !answers {
		return
	}

	out := ecs_view.ViewAnswer{}

	for other, records := range held {
		if other == id {
			continue
		}

		out[other] = records
	}

	if len(out) == 0 {
		s.names.Delete(key)

		return
	}

	s.names.Insert(key, out)
}

// enter counts one name into its zone, rendering the zone where it is the first.
func (s *state) enter(name string, ns []string, ttl uint32, host, transfer bool) {
	held := s.serials[name]

	held.names++

	if host {
		held.hosts++
	}

	if transfer {
		held.transfers++
	}

	if held.ns == nil {
		held.ns = map[string]int{}
	}

	for _, server := range ns {
		held.ns[server]++
	}

	s.serials[name] = held
	s.render(name, ttl)
}

// leave counts one name back out, taking the zone with it where that was the
// last name in it.
func (s *state) leave(name string, ns []string, ttl uint32, host, transfer bool) {
	held, known := s.serials[name]
	if !known {
		return
	}

	held.names--

	if host {
		held.hosts--
	}

	if transfer {
		held.transfers--
	}

	for _, server := range ns {
		held.ns[server]--
		if held.ns[server] <= 0 {
			delete(held.ns, server)
		}
	}

	// Not removed here: an update leaves its zone and enters it again, and
	// deleting between the two would lose the serial and republish it at 1.
	// Publish is what sweeps a zone nothing came back to.
	held.dirty = true

	s.serials[name] = held

	if held.names > 0 {
		s.render(name, ttl)
	}
}

// render writes one zone's SOA and NS, which answering a denial reads and never
// synthesizes.
func (s *state) render(name string, ttl uint32) {
	held := s.serials[name]

	servers := make([]string, 0, len(held.ns))
	for server := range held.ns {
		servers = append(servers, server)
	}

	sort.Strings(servers)

	s.zones.Insert(ecs_view.NameKey(name), zone(name, held, servers, ttl))
}

// reverse is one instance's PTR for one reverse name. Scoped to the network the
// address sits on, so a PTR obeys the same reachability its forward name does.
func (s *state) reverse(inst *instance, name, on string, ttl uint32) ecs_view.ViewAnswer {
	out := ecs_view.ViewAnswer{}

	for _, id := range s.views(on) {
		out[id] = renderPTR(name, []string{inst.names[0]}, ttl)
	}

	return out
}

// alias is one instance's CNAME for a name it claims, chased into the records
// its own name answers with so answering through one is the same lookup.
func (s *state) alias(inst *instance, name string, ttl uint32) ecs_view.ViewAnswer {
	cname := dns.RR(&dns.CNAME{
		Hdr:    dns.RR_Header{Name: name, Rrtype: dns.TypeCNAME, Class: dns.ClassINET, Ttl: ttl},
		Target: inst.names[0],
	})

	out := ecs_view.ViewAnswer{}

	for id, records := range s.records(inst, inst.names[0], ttl) {
		chased := make(ecs_view.Records, len(records)+1)
		chased[dns.TypeCNAME] = []dns.RR{cname}

		for qtype, rrs := range records {
			chased[qtype] = append([]dns.RR{cname}, rrs...)
		}

		out[id] = chased
	}

	return out
}

// addAddrs files every address this instance claims under the view it queries
// from, and every prefix of its networks under that network's own view.
func (s *state) addAddrs(inst *instance, own ecs_view.ViewID) {
	for key, net := range inst.nets {
		for _, addr := range net.ipv4 {
			s.index(netip.PrefixFrom(addr, addr.BitLen()), own)
		}

		for _, addr := range net.ipv6 {
			s.index(netip.PrefixFrom(addr, addr.BitLen()), own)
		}

		anon := ecs_view.ViewOf([]string{key})
		for _, prefix := range net.prefixes {
			s.index(prefix, anon)
		}
	}
}

// dropAddrs takes this instance's own addresses back out. The network prefixes
// are not its own to remove; dropPrefixes does those when the wire empties.
func (s *state) dropAddrs(inst *instance) {
	for _, net := range inst.nets {
		for _, addr := range net.ipv4 {
			s.unindex(netip.PrefixFrom(addr, addr.BitLen()))
		}

		for _, addr := range net.ipv6 {
			s.unindex(netip.PrefixFrom(addr, addr.BitLen()))
		}
	}
}

// dropPrefixes takes a wire's own prefixes out once the last instance on it has
// gone.
func (s *state) dropPrefixes(inst *instance, freed []string) {
	for _, key := range freed {
		net, sits := inst.nets[key]
		if !sits {
			continue
		}

		for _, prefix := range net.prefixes {
			s.unindex(prefix)
		}
	}
}

// index files one prefix under the view a querier inside it belongs to. Two
// claiming one refuse rather than let the last writer decide.
func (s *state) index(prefix netip.Prefix, view ecs_view.ViewID) {
	key, v4 := ecs_view.AddrKey(prefix)

	tree := &s.v6
	if v4 {
		tree = &s.v4
	}

	held, taken := tree.Get(key)
	if taken && held != view {
		tree.Set(key, ecs_view.AmbiguousView)

		return
	}

	tree.Set(key, view)
}

// unindex takes one prefix out. An ambiguous one stays ambiguous: the value is
// one view, so what is left after a claimant goes is not in it.
func (s *state) unindex(prefix netip.Prefix) {
	key, v4 := ecs_view.AddrKey(prefix)

	tree := &s.v6
	if v4 {
		tree = &s.v4
	}

	tree.Delete(key)
}

// records is one instance's records for one name, by the view each is reachable
// in. A view spanning several networks answers with what it can see of all of
// them, which is why this is not per network.
func (s *state) records(inst *instance, name string, ttl uint32) ecs_view.ViewAnswer {
	out := ecs_view.ViewAnswer{}

	for key := range inst.nets {
		for _, id := range s.views(key) {
			_, done := out[id]
			if done {
				continue
			}

			var v4, v6 []netip.Addr

			for _, on := range ecs_view.KeysOf(id) {
				net, reachable := inst.nets[on]
				if !reachable {
					continue
				}

				v4 = append(v4, net.ipv4...)
				v6 = append(v6, net.ipv6...)
			}

			// Sorted, so one fleet renders one way however its events arrived.
			slices.SortFunc(v4, netip.Addr.Compare)
			slices.SortFunc(v6, netip.Addr.Compare)

			out[id] = render(name, v4, v6, ttl)
		}
	}

	return out
}

// addName folds one instance's records for a name into what the tree holds.
func (s *state) addName(name string, add ecs_view.ViewAnswer) {
	key := ecs_view.NameKey(name)

	out := ecs_view.ViewAnswer{}

	held, ok := s.names.Get(key)
	if ok {
		for id, records := range held {
			out[id] = records
		}
	}

	for id, records := range add {
		out[id] = union(out[id], records)
	}

	// An instance that lost its addresses contributes nothing, and a name with
	// nothing under it is NODATA where it should be gone.
	if len(out) == 0 {
		s.names.Delete(key)

		return
	}

	s.names.Insert(key, out)
}

// dropName takes one instance's records back out, and the name with them where
// nothing else was answering there.
func (s *state) dropName(name string, drop ecs_view.ViewAnswer) {
	key := ecs_view.NameKey(name)

	held, ok := s.names.Get(key)
	if !ok {
		return
	}

	out := ecs_view.ViewAnswer{}

	for id, records := range held {
		gone, touched := drop[id]
		if !touched {
			out[id] = records

			continue
		}

		left := without(records, gone)
		if len(left) > 0 {
			out[id] = left
		}
	}

	if len(out) == 0 {
		s.names.Delete(key)

		return
	}

	s.names.Insert(key, out)
}

// union is one view's records with another's folded in, built fresh so a map
// already published is never written through.
func union(held, add ecs_view.Records) ecs_view.Records {
	out := make(ecs_view.Records, len(held)+len(add))

	for qtype, rrs := range held {
		out[qtype] = slices.Clone(rrs)
	}

	for qtype, rrs := range add {
		for _, rr := range rrs {
			if slices.ContainsFunc(out[qtype], func(have dns.RR) bool { return sameRR(have, rr) }) {
				continue
			}

			out[qtype] = append(out[qtype], rr)
		}

		sortRRs(out[qtype])
	}

	return out
}

// without is one view's records with another's taken out, dropping a type
// nothing is left under so a name with none at all can be told.
func without(held, gone ecs_view.Records) ecs_view.Records {
	out := make(ecs_view.Records, len(held))

	for qtype, rrs := range held {
		var left []dns.RR

		for _, rr := range rrs {
			if slices.ContainsFunc(gone[qtype], func(have dns.RR) bool { return sameRR(have, rr) }) {
				continue
			}

			left = append(left, rr)
		}

		if len(left) > 0 {
			out[qtype] = left
		}
	}

	return out
}

// sameRR reports whether two records say the same thing. By value, because a
// patch renders its own: contains compares by identity and would miss.
func sameRR(a, b dns.RR) bool {
	if a.Header().Rrtype != b.Header().Rrtype || a.Header().Name != b.Header().Name {
		return false
	}

	switch x := a.(type) {
	case *dns.A:
		y, ok := b.(*dns.A)

		return ok && x.A.Equal(y.A)
	case *dns.AAAA:
		y, ok := b.(*dns.AAAA)

		return ok && x.AAAA.Equal(y.AAAA)
	case *dns.PTR:
		y, ok := b.(*dns.PTR)

		return ok && x.Ptr == y.Ptr
	case *dns.CNAME:
		y, ok := b.(*dns.CNAME)

		return ok && x.Target == y.Target
	}

	return false
}

// sortRRs orders one type's records, so a name reads the same however the
// events that built it arrived.
func sortRRs(rrs []dns.RR) {
	sort.Slice(rrs, func(i, j int) bool { return rrs[i].String() < rrs[j].String() })
}
