package dns

import (
	"net/netip"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lxc/incus-compose/ievent/dns/ecs_view"
	"github.com/lxc/incus-compose/ievent/shared"
)

// on builds an instance sitting on the given networks, with one address each.
func on(zone string, meta map[string]string, nets map[string]string) *instance {
	out := &instance{zone: zone, meta: meta, nets: map[string]*shared.Network{}}

	for key, addr := range nets {
		prefix := netip.MustParsePrefix(netip.MustParseAddr(addr).String() + "/24").Masked()

		out.nets[key] = shared.NewNetwork("net", "shop", true,
			[]netip.Prefix{prefix}, []netip.Addr{netip.MustParseAddr(addr)}, nil)
	}

	return out
}

// seenFrom is what a querier on these networks can resolve.
func seenFrom(snap *ecs_view.Snapshot, keys ...string) map[string]ecs_view.RRSets {
	return snap.Views[ecs_view.ViewOf(keys)]
}

// TestBuildViewsSpanProjects pins why the build is fleet-wide: two instances on
// one shared bridge sit in different zones and must still see each other.
func TestBuildViewsSpanProjects(t *testing.T) {
	held := map[string]*instance{
		"shop/web": on("shop.incus.", nil, map[string]string{"default/shared": "10.0.0.2"}),
		"blog/api": on("blog.incus.", nil, map[string]string{"default/shared": "10.0.0.3"}),
	}

	snap := build(held, nil, 5)

	visible := seenFrom(snap, "default/shared")

	assert.Contains(t, visible, "web.shop.incus.")
	assert.Contains(t, visible, "api.blog.incus.", "a name across the project boundary went missing")
}

// TestBuildIsolatesUnsharedNetworks pins the other half: instances sharing no
// wire are absent from each other's view, which the query path reads as NXDOMAIN.
func TestBuildIsolatesUnsharedNetworks(t *testing.T) {
	held := map[string]*instance{
		"shop/web": on("shop.incus.", nil, map[string]string{"shop/net0": "10.0.0.2"}),
		"blog/api": on("blog.incus.", nil, map[string]string{"blog/net0": "10.1.0.2"}),
	}

	snap := build(held, nil, 5)

	assert.Contains(t, seenFrom(snap, "shop/net0"), "web.shop.incus.")
	assert.NotContains(t, seenFrom(snap, "shop/net0"), "api.blog.incus.",
		"an instance was shown a host it shares no network with")
}

// TestBuildAnswersOnTheSharedWireOnly pins that a multi-homed host is answered
// with the addresses the querier can reach and not its others.
func TestBuildAnswersOnTheSharedWireOnly(t *testing.T) {
	held := map[string]*instance{
		"shop/web": on("shop.incus.", nil, map[string]string{
			"shop/front": "10.0.0.2",
			"shop/back":  "10.1.0.2",
		}),
		"shop/db": on("shop.incus.", nil, map[string]string{"shop/back": "10.1.0.3"}),
	}

	snap := build(held, nil, 5)

	// A querier on the back wire alone sees web's back address and not its
	// front one.
	rrs := seenFrom(snap, "shop/back")["web.shop.incus."]
	require.NotEmpty(t, rrs)

	var got []string
	for _, rr := range rrs[1] { // dns.TypeA
		got = append(got, rr.String())
	}

	require.Len(t, got, 1, "the querier was handed an address it cannot reach")
	assert.Contains(t, got[0], "10.1.0.2")
}

// TestBuildServiceNameGathersReplicas pins the round-robin: replicas labeled
// with one service answer under one name.
func TestBuildServiceNameGathersReplicas(t *testing.T) {
	svc := map[string]string{metaService: "api"}

	held := map[string]*instance{
		"shop/api-1": on("shop.incus.", svc, map[string]string{"shop/net0": "10.0.0.2"}),
		"shop/api-2": on("shop.incus.", svc, map[string]string{"shop/net0": "10.0.0.3"}),
	}

	snap := build(held, nil, 5)

	visible := seenFrom(snap, "shop/net0")

	// Both replicas under the service name, and each still under its own.
	assert.Len(t, visible["api.shop.incus."][1], 2)
	assert.Len(t, visible["api-1.shop.incus."][1], 1)
}

// TestBuildSerialsStepOnlyOnChange pins what a secondary depends on: a serial
// that moves for nothing re-transfers, one that stands still misses an update.
func TestBuildSerialsStepOnlyOnChange(t *testing.T) {
	held := map[string]*instance{
		"shop/web": on("shop.incus.", nil, map[string]string{"shop/net0": "10.0.0.2"}),
	}

	first := build(held, nil, 5)
	require.Equal(t, uint32(1), first.ByZone["shop.incus."].Serial, "a new zone starts at 1")

	// Built again from the same fleet.
	again := build(held, first, 5)
	assert.Equal(t, first.ByZone["shop.incus."].Serial, again.ByZone["shop.incus."].Serial,
		"the serial moved without the records changing")

	// An address changes, so the zone does.
	held["shop/web"] = on("shop.incus.", nil, map[string]string{"shop/net0": "10.0.0.9"})

	moved := build(held, again, 5)
	assert.Equal(t, uint32(2), moved.ByZone["shop.incus."].Serial)

	// And so does moving between wires without changing address: who can see it
	// changed even though what it answers did not.
	held["shop/web"] = on("shop.incus.", nil, map[string]string{"shop/other": "10.0.0.9"})

	rewired := build(held, moved, 5)
	assert.Equal(t, uint32(3), rewired.ByZone["shop.incus."].Serial,
		"a change in reachability did not step the serial")
}

// TestBuildAmbiguousAddress pins the fail-closed rule for an address two
// instances claim, which overlapping subnets in two projects really produce.
func TestBuildAmbiguousAddress(t *testing.T) {
	held := map[string]*instance{
		"shop/web": on("shop.incus.", nil, map[string]string{"shop/net0": "10.0.0.2"}),
		"blog/api": on("blog.incus.", nil, map[string]string{"blog/net0": "10.0.0.2"}),
	}

	snap := build(held, nil, 5)

	assert.Equal(t, ecs_view.AmbiguousView, snap.ByAddr[netip.MustParseAddr("10.0.0.2")],
		"a contested address was attributed to one of its claimants")
}

// TestZoneFor pins the naming, including the override a project sets on itself.
func TestZoneFor(t *testing.T) {
	assert.Equal(t, "shop.incus.", zoneFor("shop", nil, "incus"))
	assert.Equal(t, "example.test.", zoneFor("shop", map[string]string{metaZone: "example.test"}, "incus"))

	// Canonical either way, so a label written without the dot still matches a
	// query, which always carries one.
	assert.Equal(t, "example.test.", zoneFor("shop", map[string]string{metaZone: "example.test."}, "incus"))
}
