package dns

import (
	"testing"

	"github.com/miekg/dns"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lxc/incus-compose/ievent/dns/ecs_view"
)

// aliased is an instance carrying an aliases label, on one network.
func aliased(zone, list string, nets map[string]string) *instance {
	return on(zone, map[string]string{metaAliases: list}, nets)
}

// answerFor is what one name answers with, of one type, seen from these
// networks.
func answerFor(snap *ecs_view.Snapshot, name string, qtype uint16, keys ...string) []dns.RR {
	return seenFrom(snap, keys...)[name][qtype]
}

// TestAliasNames pins the one rule an operator has to hold in their head: a
// trailing dot is absolute, anything else relative to the instance's own zone.
func TestAliasNames(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		list string
		want []string
	}{
		{
			name: "no label at all claims nothing",
			list: "",
		},
		{
			name: "a bare name is relative to the instance's zone",
			list: "alias1",
			want: []string{"alias1.shop.incus."},
		},
		{
			name: "a trailing dot is absolute and taken as written",
			list: "me.example.com.",
			want: []string{"me.example.com."},
		},
		{
			name: "both at once, which is the documented example",
			list: "alias1,me.example.com.",
			want: []string{"alias1.shop.incus.", "me.example.com."},
		},
		{
			name: "dots without a trailing one are still relative",
			list: "www.eu",
			want: []string{"www.eu.shop.incus."},
		},
		{
			name: "space around a name is not part of it",
			list: " alias1 , alias2 ",
			want: []string{"alias1.shop.incus.", "alias2.shop.incus."},
		},
		{
			name: "an empty field is dropped rather than claiming the zone",
			list: "alias1,,",
			want: []string{"alias1.shop.incus."},
		},
		{
			name: "naming one alias twice claims it once",
			list: "alias1,alias1",
			want: []string{"alias1.shop.incus."},
		},
		{
			name: "case is not part of a name",
			list: "Alias1,ME.example.com.",
			want: []string{"alias1.shop.incus.", "me.example.com."},
		},
		{
			name: "an empty label is not a domain name",
			list: "a..b",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := aliasNames(map[string]string{metaAliases: tc.list}, "shop.incus.")

			assert.Equal(t, tc.want, got)
		})
	}
}

// TestAliasAnswersWithACName pins the shape of the answer: the canonical name
// and what it resolves to in one reply, CNAME first as a resolver reads it.
func TestAliasAnswersWithACName(t *testing.T) {
	t.Parallel()

	held := map[string]*instance{
		"shop/web": aliased("shop.incus.", "alias1", map[string]string{"shop/net0": "10.0.0.2"}),
	}

	snap := build(held, nil, 5)

	rrs := answerFor(snap, "alias1.shop.incus.", dns.TypeA, "shop/net0")
	require.Len(t, rrs, 2, "an aliased A answers with the CNAME and the address behind it")

	cname, ok := rrs[0].(*dns.CNAME)
	require.True(t, ok, "the canonical name has to come first, got %T", rrs[0])
	assert.Equal(t, "alias1.shop.incus.", cname.Hdr.Name)
	assert.Equal(t, "web.shop.incus.", cname.Target)

	a, ok := rrs[1].(*dns.A)
	require.True(t, ok, "the target's address is missing, got %T", rrs[1])
	assert.Equal(t, "10.0.0.2", a.A.String())

	// And asked for the CNAME itself, it answers with that alone.
	only := answerFor(snap, "alias1.shop.incus.", dns.TypeCNAME, "shop/net0")
	require.Len(t, only, 1)
	assert.Equal(t, cname, only[0])

	// The host keeps its own name, unaliased and uncnamed.
	host := answerFor(snap, "web.shop.incus.", dns.TypeA, "shop/net0")
	require.Len(t, host, 1)
	assert.IsType(t, &dns.A{}, host[0])
}

// TestAliasFollowsItsInstanceVisibility pins that an alias is no way around the
// visibility rule: it is reachable exactly where the instance it names is.
func TestAliasFollowsItsInstanceVisibility(t *testing.T) {
	t.Parallel()

	held := map[string]*instance{
		"shop/web": aliased("shop.incus.", "alias1,me.example.com.",
			map[string]string{"shop/net0": "10.0.0.2"}),
		"blog/api": on("blog.incus.", nil, map[string]string{"blog/net0": "10.1.0.2"}),
	}

	snap := build(held, nil, 5)

	assert.Contains(t, seenFrom(snap, "shop/net0"), "alias1.shop.incus.")
	assert.Contains(t, seenFrom(snap, "shop/net0"), "me.example.com.")

	assert.NotContains(t, seenFrom(snap, "blog/net0"), "alias1.shop.incus.",
		"an alias was shown to a querier that shares no network with its instance")
	assert.NotContains(t, seenFrom(snap, "blog/net0"), "me.example.com.",
		"an absolute alias escaped the visibility rule")
}

// TestAbsoluteAliasInventsAFallthroughZone pins what an alias outside every
// zone gets: one claiming that name and leaving the domain to the forwarder.
func TestAbsoluteAliasInventsAFallthroughZone(t *testing.T) {
	t.Parallel()

	held := map[string]*instance{
		"shop/web": aliased("shop.incus.", "me.example.com.",
			map[string]string{"shop/net0": "10.0.0.2"}),
	}

	snap := build(held, nil, 5)

	z := snap.ByZone["example.com."]
	require.NotNil(t, z, "an absolute alias outside every zone needs one inventing")
	assert.True(t, z.Fallthrough, "an invented zone must not claim the whole domain")
	assert.Contains(t, z.Names, "me.example.com.")
	assert.NotContains(t, z.Names, "www.example.com.")

	// The zone a project actually asked for is authoritative as it always was.
	assert.False(t, snap.ByZone["shop.incus."].Fallthrough)
}

// TestAliasLandsInTheLongestZoneServingIt pins that an absolute alias inside a
// served zone joins it rather than inventing a second one that would shadow it.
func TestAliasLandsInTheLongestZoneServingIt(t *testing.T) {
	t.Parallel()

	held := map[string]*instance{
		"shop/web": aliased("shop.incus.", "www.shop.incus.",
			map[string]string{"shop/net0": "10.0.0.2"}),
	}

	snap := build(held, nil, 5)

	assert.Contains(t, snap.ByZone["shop.incus."].Names, "www.shop.incus.")
	assert.NotContains(t, snap.ByZone, "shop.incus.shop.incus.")
	assert.Len(t, snap.ByZone, 2, "the forward zone and its reverse, and no third")
}

// TestAliasCollisions pins the three ways an alias is refused, each a record set
// that would be invalid on the wire or decided by map order.
func TestAliasCollisions(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		held  map[string]*instance
		alias string
	}{
		{
			name:  "a name two instances both claim belongs to neither",
			alias: "alias1.shop.incus.",
			held: map[string]*instance{
				"shop/web": aliased("shop.incus.", "alias1", map[string]string{"shop/net0": "10.0.0.2"}),
				"shop/api": aliased("shop.incus.", "alias1", map[string]string{"shop/net0": "10.0.0.3"}),
			},
		},
		{
			name:  "a name a host already answers to keeps its addresses",
			alias: "api.shop.incus.",
			held: map[string]*instance{
				"shop/web": aliased("shop.incus.", "api", map[string]string{"shop/net0": "10.0.0.2"}),
				"shop/api": on("shop.incus.", nil, map[string]string{"shop/net0": "10.0.0.3"}),
			},
		},
		{
			name:  "a name a service already answers to keeps its addresses",
			alias: "store.shop.incus.",
			held: map[string]*instance{
				"shop/web": aliased("shop.incus.", "store", map[string]string{"shop/net0": "10.0.0.2"}),
				"shop/api": on("shop.incus.", map[string]string{metaService: "store"},
					map[string]string{"shop/net0": "10.0.0.3"}),
			},
		},
		{
			name:  "a zone apex is refused, since a CNAME there displaces the SOA",
			alias: "shop.incus.",
			held: map[string]*instance{
				"shop/web": aliased("shop.incus.", "shop.incus.", map[string]string{"shop/net0": "10.0.0.2"}),
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			snap := build(tc.held, nil, 5)

			rrs := answerFor(snap, tc.alias, dns.TypeA, "shop/net0")

			for _, rr := range rrs {
				assert.IsType(t, &dns.A{}, rr, "a refused alias rendered a CNAME anyway")
			}

			assert.Empty(t, seenFrom(snap, "shop/net0")[tc.alias][dns.TypeCNAME],
				"a refused alias is answerable as a CNAME")
		})
	}
}

// TestMultiHomedAliasAnswersOneCName pins the join: one record under several
// keys, so a querier sharing two of them gets both addresses and one CNAME.
func TestMultiHomedAliasAnswersOneCName(t *testing.T) {
	t.Parallel()

	held := map[string]*instance{
		"shop/web": aliased("shop.incus.", "alias1", map[string]string{
			"shop/front": "10.0.0.2",
			"shop/back":  "10.1.0.2",
		}),
	}

	snap := build(held, nil, 5)

	rrs := answerFor(snap, "alias1.shop.incus.", dns.TypeA, "shop/back", "shop/front")

	var cnames, addrs int

	for _, rr := range rrs {
		switch rr.(type) {
		case *dns.CNAME:
			cnames++
		case *dns.A:
			addrs++
		}
	}

	assert.Equal(t, 1, cnames, "the querier was handed the canonical name twice")
	assert.Equal(t, 2, addrs, "an address the querier can reach went missing")
	assert.IsType(t, &dns.CNAME{}, rrs[0], "the canonical name has to come first")
}

// TestAliasStepsTheSerial pins that an alias is part of what a zone says. A
// secondary that does not re-transfer on one never learns the name exists.
func TestAliasStepsTheSerial(t *testing.T) {
	t.Parallel()

	held := map[string]*instance{
		"shop/web": on("shop.incus.", nil, map[string]string{"shop/net0": "10.0.0.2"}),
	}

	first := build(held, nil, 5)

	again := build(held, first, 5)
	require.Equal(t, first.ByZone["shop.incus."].Serial, again.ByZone["shop.incus."].Serial,
		"the serial moved without the records changing")

	held["shop/web"] = aliased("shop.incus.", "alias1", map[string]string{"shop/net0": "10.0.0.2"})

	added := build(held, again, 5)
	assert.Equal(t, again.ByZone["shop.incus."].Serial+1, added.ByZone["shop.incus."].Serial,
		"a new alias did not step the serial")
}

// TestAliasZoneSerialFollowsItsInstance pins why an alias carries its networks
// into the digest: an invented zone holds nothing else that notices a rewire.
func TestAliasZoneSerialFollowsItsInstance(t *testing.T) {
	t.Parallel()

	held := map[string]*instance{
		"shop/web": aliased("shop.incus.", "me.example.com.",
			map[string]string{"shop/net0": "10.0.0.2"}),
	}

	first := build(held, nil, 5)
	require.Equal(t, uint32(1), first.ByZone["example.com."].Serial)

	again := build(held, first, 5)
	require.Equal(t, uint32(1), again.ByZone["example.com."].Serial,
		"the serial moved without anything changing")

	// Same name, same target, same address - a different wire, so a different
	// set of queriers can resolve it.
	held["shop/web"] = aliased("shop.incus.", "me.example.com.",
		map[string]string{"shop/other": "10.0.0.2"})

	moved := build(held, again, 5)
	assert.Equal(t, uint32(2), moved.ByZone["example.com."].Serial,
		"an alias changing network did not step its zone's serial")
}

// TestAliasToAnotherZone pins what an absolute alias is really for: a name under
// one project's zone answering for an instance in another, address included.
func TestAliasToAnotherZone(t *testing.T) {
	t.Parallel()

	held := map[string]*instance{
		"shop/web": aliased("shop.incus.", "shop.blog.incus.",
			map[string]string{"default/iutil": "10.0.0.2"}),
		"blog/api": on("blog.incus.", nil, map[string]string{"default/iutil": "10.0.0.3"}),
	}

	snap := build(held, nil, 5)

	rrs := answerFor(snap, "shop.blog.incus.", dns.TypeA, "default/iutil")
	require.Len(t, rrs, 2)

	cname, ok := rrs[0].(*dns.CNAME)
	require.True(t, ok, "got %T", rrs[0])
	assert.Equal(t, "web.shop.incus.", cname.Target, "the alias points out of its own zone")

	a, ok := rrs[1].(*dns.A)
	require.True(t, ok, "got %T", rrs[1])
	assert.Equal(t, "10.0.0.2", a.A.String())

	// It went into the zone that already existed rather than inventing one.
	assert.False(t, snap.ByZone["blog.incus."].Fallthrough)
}
