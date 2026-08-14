package dns

import (
	"net/netip"
	"testing"

	"github.com/miekg/dns"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lxc/incus-compose/ievent/dns/ecs_view"
	"github.com/lxc/incus-compose/ievent/shared"
)

// netSpec is one wire written out rather than derived, because `on` draws a /24
// around its address and these cases turn on prefixes that do not cover theirs.
type netSpec struct {
	key      string
	prefixes []string
	addrs    []string
}

// sitting builds one instance from those wires.
func sitting(zone string, specs ...netSpec) *instance {
	out := &instance{zone: zone, nets: map[string]*shared.Network{}}

	for _, spec := range specs {
		var (
			prefixes []netip.Prefix
			v4, v6   []netip.Addr
		)

		for _, p := range spec.prefixes {
			prefixes = append(prefixes, netip.MustParsePrefix(p))
		}

		for _, a := range spec.addrs {
			addr := netip.MustParseAddr(a)
			if addr.Is4() {
				v4 = append(v4, addr)
			} else {
				v6 = append(v6, addr)
			}
		}

		out.nets[spec.key] = shared.NewNetwork("net", "shop", len(prefixes) > 0, prefixes, v4, v6)
	}

	return out
}

func TestReverseNameSplitsHostFromZone(t *testing.T) {
	tests := []struct {
		addr string
		name string
		zone string
	}{
		{
			addr: "10.75.12.5",
			name: "5.12.75.10.in-addr.arpa.",
			zone: "12.75.10.in-addr.arpa.",
		},
		{
			// The zone is the /24 whatever the network's own prefix is, so two
			// bridges inside one /16 keep their reverses apart.
			addr: "10.75.13.5",
			name: "5.13.75.10.in-addr.arpa.",
			zone: "13.75.10.in-addr.arpa.",
		},
		{
			addr: "fd42::1:2",
			name: "2.0.0.0.1.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.2.4.d.f.ip6.arpa.",
			zone: "0.0.0.0.0.0.0.0.0.0.0.0.2.4.d.f.ip6.arpa.",
		},
		{
			// Mapped into v6, which dns.ReverseAddr reads as IPv4: the zone
			// follows the family the name came out in.
			addr: "::ffff:10.75.12.5",
			name: "5.12.75.10.in-addr.arpa.",
			zone: "12.75.10.in-addr.arpa.",
		},
	}

	for _, test := range tests {
		t.Run(test.addr, func(t *testing.T) {
			name, zone, ok := reverseName(netip.MustParseAddr(test.addr))
			require.True(t, ok)

			assert.Equal(t, test.name, name)
			assert.Equal(t, test.zone, zone)

			// The zone has to hold the name, or the query path cannot reach it.
			assert.True(t, dns.IsSubDomain(zone, name), "%s is not under %s", name, zone)
		})
	}
}

func TestHashPTRIgnoresOrder(t *testing.T) {
	one := map[string][]ptrEntry{
		"5.12.75.10.in-addr.arpa.": {
			{key: "default/ic-api", target: "web-1.shop.incus."},
			{key: "default/ic-users", target: "web-2.shop.incus."},
		},
	}

	other := map[string][]ptrEntry{
		"5.12.75.10.in-addr.arpa.": {
			{key: "default/ic-users", target: "web-2.shop.incus."},
			{key: "default/ic-api", target: "web-1.shop.incus."},
		},
	}

	assert.Equal(t, hashPTR(one), hashPTR(other))

	// The network a name is answered on is part of what the zone says, so
	// moving it has to move the serial.
	moved := map[string][]ptrEntry{
		"5.12.75.10.in-addr.arpa.": {
			{key: "default/elsewhere", target: "web-1.shop.incus."},
			{key: "default/ic-users", target: "web-2.shop.incus."},
		},
	}

	assert.NotEqual(t, hashPTR(one), hashPTR(moved))
}

// TestBuildDerivesReverseRecords is the forward derivation read backwards: zone
// from the address, name from the instance, keyed by the network it is on.
func TestBuildDerivesReverseRecords(t *testing.T) {
	held := map[string]*instance{
		"shop/user-api-1": sitting("shop.incus.",
			netSpec{
				key:      "default/ic-api",
				prefixes: []string{"10.0.1.0/24", "fd42::/64"},
				addrs:    []string{"10.0.1.20", "fd42::1:2"},
			},
			netSpec{
				key:      "default/ic-users",
				prefixes: []string{"10.0.2.0/24"},
				addrs:    []string{"10.0.2.20"},
			},
		),
	}

	held["shop/user-api-1"].meta = map[string]string{metaService: "user-api"}

	snap := build(held, nil, 5)

	z := snap.ByZone["1.0.10.in-addr.arpa."]
	require.NotNil(t, z, "no reverse zone for 10.0.1.0/24")

	perNet := z.Names["20.1.0.10.in-addr.arpa."]
	require.Contains(t, perNet, "default/ic-api",
		"the reverse is not keyed by the network the address is on")
	require.NotContains(t, perNet, "default/ic-users",
		"an address answers only on its own network")

	rrs := perNet["default/ic-api"][dns.TypePTR]
	require.Len(t, rrs, 1)

	// The instance name, never the service one: a reverse lookup wants the name
	// that names this host alone.
	assert.Equal(t, "user-api-1.shop.incus.", rrs[0].(*dns.PTR).Ptr)

	// The address on the other network answers under its own zone.
	other := snap.ByZone["2.0.10.in-addr.arpa."]
	require.NotNil(t, other)
	assert.Len(t, other.Names["20.2.0.10.in-addr.arpa."]["default/ic-users"][dns.TypePTR], 1)

	// IPv6 gets a /64 zone of its own.
	v6 := snap.ByZone["0.0.0.0.0.0.0.0.0.0.0.0.2.4.d.f.ip6.arpa."]
	require.NotNil(t, v6, "no reverse zone for the /64")
	assert.Len(t,
		v6.Names["2.0.0.0.1.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.2.4.d.f.ip6.arpa."]["default/ic-api"][dns.TypePTR],
		1)
}

// TestReverseClaimsNothingEmpty is why the zone comes from the address: a
// subnet Incus manages but nothing sits on falls through rather than NXDOMAIN.
func TestReverseClaimsNothingEmpty(t *testing.T) {
	held := map[string]*instance{
		// The network addresses two /24s and the instance is on one of them.
		"shop/gateway-1": sitting("shop.incus.", netSpec{
			key:      "default/ic-api",
			prefixes: []string{"10.0.1.0/24", "10.0.9.0/24"},
			addrs:    []string{"10.0.1.10"},
		}),
	}

	snap := build(held, nil, 5)

	assert.Contains(t, snap.ByZone, "1.0.10.in-addr.arpa.")
	assert.NotContains(t, snap.ByZone, "9.0.10.in-addr.arpa.",
		"claimed a subnet holding nothing")

	// An address inside a zone that is claimed is a name that does not exist,
	// which is not the same as one that falls through.
	name, zone, ok := reverseName(netip.MustParseAddr("10.0.1.99"))
	require.True(t, ok)
	require.Equal(t, "1.0.10.in-addr.arpa.", zone)

	claimed := snap.ByZone[zone]
	require.NotNil(t, claimed)

	assert.NotContains(t, claimed.Names, name)
}

// TestReverseSkipsSubnetsIncusDoesNotOwn is the same rule further: an instance
// bridged onto somebody else's wire is served forward and not backward.
func TestReverseSkipsSubnetsIncusDoesNotOwn(t *testing.T) {
	held := map[string]*instance{
		"shop/edge-1": sitting("shop.incus.", netSpec{
			key:   "default/ic-lan",
			addrs: []string{"203.0.113.5"},
		}),
	}

	snap := build(held, nil, 5)

	// The address still resolves forward: sharing an unmanaged wire is still
	// sharing a network.
	assert.Len(t, snap.ByZone["shop.incus."].Names["edge-1.shop.incus."]["default/ic-lan"][dns.TypeA], 1)

	assert.NotContains(t, snap.ByZone, "113.0.203.in-addr.arpa.",
		"claimed the reverse of a subnet Incus does not address")

	// An address inside its network's prefix keeps its reverse, so the rule is
	// coverage rather than "no reverse for anything".
	managed := build(map[string]*instance{
		"shop/edge-1": sitting("shop.incus.", netSpec{
			key:      "default/ic-api",
			prefixes: []string{"10.0.1.0/24"},
			addrs:    []string{"10.0.1.5"},
		}),
	}, nil, 5)

	assert.Contains(t, managed.ByZone, "1.0.10.in-addr.arpa.")
}

// TestReverseSerialOnlyMovesOnChange is the forward serial rule for a reverse
// zone: a rebuild that changed nothing must not make a secondary re-transfer.
func TestReverseSerialOnlyMovesOnChange(t *testing.T) {
	wire := netSpec{
		key:      "default/ic-api",
		prefixes: []string{"10.0.1.0/24"},
		addrs:    []string{"10.0.1.10"},
	}

	first := build(map[string]*instance{
		"shop/gateway-1": sitting("shop.incus.", wire),
	}, nil, 5)
	require.NotNil(t, first.ByZone["1.0.10.in-addr.arpa."], "no reverse zone to carry a serial")
	require.Equal(t, uint32(1), first.ByZone["1.0.10.in-addr.arpa."].Serial)

	again := build(map[string]*instance{
		"shop/gateway-1": sitting("shop.incus.", wire),
	}, first, 5)
	require.NotNil(t, again.ByZone["1.0.10.in-addr.arpa."])
	assert.Equal(t, uint32(1), again.ByZone["1.0.10.in-addr.arpa."].Serial,
		"an unchanged reverse zone stepped its serial")

	// The same address, answering with a different name.
	moved := build(map[string]*instance{
		"shop/gateway-2": sitting("shop.incus.", wire),
	}, again, 5)
	require.NotNil(t, moved.ByZone["1.0.10.in-addr.arpa."])
	assert.Equal(t, uint32(2), moved.ByZone["1.0.10.in-addr.arpa."].Serial,
		"the name a reverse zone answers with changed and the serial did not")
}

// TestReverseOfSharedAddressNamesBoth is the reverse of an ambiguous address:
// overlapping subnets in two projects, and the reverse names both claimants.
func TestReverseOfSharedAddressNamesBoth(t *testing.T) {
	held := map[string]*instance{
		"shop/web-1": sitting("shop.incus.", netSpec{
			key:      "shop/ic-shop",
			prefixes: []string{"10.0.1.0/24"},
			addrs:    []string{"10.0.1.10"},
		}),
		"blog/web-1": sitting("blog.incus.", netSpec{
			key:      "blog/ic-blog",
			prefixes: []string{"10.0.1.0/24"},
			addrs:    []string{"10.0.1.10"},
		}),
	}

	snap := build(held, nil, 5)

	// The address identifies no querier, which is the forward half of the same
	// clash.
	assert.Equal(t, ecs_view.AmbiguousView, snap.ByAddr[netip.MustParseAddr("10.0.1.10")])

	// Each claim is answered on its own network, so a querier on one of them
	// sees one name rather than both.
	z := snap.ByZone["1.0.10.in-addr.arpa."]
	require.NotNil(t, z)

	perNet := z.Names["10.1.0.10.in-addr.arpa."]
	require.Len(t, perNet, 2)

	assert.Equal(t, "web-1.shop.incus.", perNet["shop/ic-shop"][dns.TypePTR][0].(*dns.PTR).Ptr)
	assert.Equal(t, "web-1.blog.incus.", perNet["blog/ic-blog"][dns.TypePTR][0].(*dns.PTR).Ptr)
}

// TestReverseReachesViewsLikeAForwardName pins that a PTR goes through the same
// gather, so it is visible exactly where the address's network is.
func TestReverseReachesViewsLikeAForwardName(t *testing.T) {
	held := map[string]*instance{
		// Multi-homed: on the front wire and the back one.
		"shop/api-1": sitting("shop.incus.",
			netSpec{key: "shop/front", prefixes: []string{"10.0.1.0/24"}, addrs: []string{"10.0.1.20"}},
			netSpec{key: "shop/back", prefixes: []string{"10.0.2.0/24"}, addrs: []string{"10.0.2.20"}},
		),
		// On the back wire alone.
		"shop/db-1": sitting("shop.incus.",
			netSpec{key: "shop/back", prefixes: []string{"10.0.2.0/24"}, addrs: []string{"10.0.2.30"}},
		),
	}

	snap := build(held, nil, 5)

	back := seenFrom(snap, "shop/back")

	// db shares the back wire with api, so it may name that address.
	assert.Contains(t, back, "20.2.0.10.in-addr.arpa.")

	// api's front address is on a wire db is not on. Absent is what the query
	// path answers NXDOMAIN from.
	assert.NotContains(t, back, "20.1.0.10.in-addr.arpa.",
		"a reverse was shown to a querier that cannot reach the address")

	// And it is there for a querier that is on that wire.
	assert.Contains(t, seenFrom(snap, "shop/front", "shop/back"), "20.1.0.10.in-addr.arpa.")
}
