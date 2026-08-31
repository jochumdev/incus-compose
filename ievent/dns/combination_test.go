package dns

import (
	"testing"

	iradix "github.com/hashicorp/go-immutable-radix/v2"
	"github.com/miekg/dns"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lxc/incus-compose/ievent/dns/ecs_view"
)

// everyName is every name a tree answers to, so two derivations can be compared
// whole rather than at the names a test thought to ask about.
func everyName(tree *iradix.Tree[ecs_view.ViewAnswer]) map[string]ecs_view.ViewAnswer {
	out := map[string]ecs_view.ViewAnswer{}

	tree.Root().Walk(func(key []byte, answer ecs_view.ViewAnswer) bool {
		out[string(key)] = answer

		return false
	})

	return out
}

// fleets are the shapes a patch has to agree with a build about. Keyed by what
// makes each different, since that is what a failure has to name.
func fleets() map[string]map[string]*instance {
	return map[string]map[string]*instance{
		"one instance on one wire": {
			"shop/web": instanceOn("shop", "web", nil, map[string]string{"net-a": "10.0.0.5"}),
		},
		"two replicas under one service name": {
			"shop/web": instanceOn("shop", "web", map[string]string{metaService: "frontend"}, map[string]string{"net-a": "10.0.0.5"}),
			"shop/api": instanceOn("shop", "api", map[string]string{metaService: "frontend"}, map[string]string{"net-a": "10.0.0.6"}),
		},
		"an instance on two wires": {
			"shop/gate": instanceOn("shop", "gate", nil, map[string]string{"net-a": "10.0.0.7", "net-b": "10.1.0.7"}),
		},
		"a multi-homed instance beside a single-homed one": {
			"shop/web":  instanceOn("shop", "web", nil, map[string]string{"net-a": "10.0.0.5"}),
			"shop/gate": instanceOn("shop", "gate", nil, map[string]string{"net-a": "10.0.0.7", "net-b": "10.1.0.7"}),
		},
		"two projects sharing one zone": {
			"shop/web":  instanceOn("shop", "web", map[string]string{metaZone: "one.example."}, map[string]string{"net-a": "10.0.0.5"}),
			"other/api": instanceOn("other", "api", map[string]string{metaZone: "one.example."}, map[string]string{"net-a": "10.0.0.6"}),
		},
		"an instance answering to an alias": {
			"shop/web": instanceOn("shop", "web", map[string]string{metaAliases: "www"}, map[string]string{"net-a": "10.0.0.5"}),
		},
		"wires that do not reach each other": {
			"shop/web":  instanceOn("shop", "web", nil, map[string]string{"net-a": "10.0.0.5"}),
			"other/api": instanceOn("other", "api", nil, map[string]string{"net-b": "10.1.0.6"}),
		},
		"a service split across two wires": {
			"shop/web":  instanceOn("shop", "web", map[string]string{metaService: "frontend"}, map[string]string{"net-a": "10.0.0.5"}),
			"shop/api":  instanceOn("shop", "api", map[string]string{metaService: "frontend"}, map[string]string{"net-b": "10.1.0.6"}),
			"shop/gate": instanceOn("shop", "gate", nil, map[string]string{"net-a": "10.0.0.7", "net-b": "10.1.0.7"}),
		},
	}
}

// TestOneFleetIsOneTreeHoweverItArrived compares every arrival order against
// the first, which needs no second derivation to compare with: a patch that
// depends on the order events came in agrees with itself by luck.
func TestOneFleetIsOneTreeHoweverItArrived(t *testing.T) {
	for name, held := range fleets() {
		t.Run(name, func(t *testing.T) {
			var want map[string]ecs_view.ViewAnswer

			var first []string

			for _, keys := range orders(held) {
				s := newState(map[string]zoneSerial{})
				for _, key := range keys {
					s.apply(key, held[key], 5)
				}

				got := everyName(s.snapshot().Tree)

				if want == nil {
					want, first = got, keys

					continue
				}

				require.Len(t, got, len(want), "%v holds a different set of names than %v", keys, first)

				for key, answer := range want {
					mine, held := got[key]
					require.True(t, held, "%q is answered as %v but not as %v", key, first, keys)

					sameAnswers(t, answer, mine, key)
				}
			}
		})
	}
}

// TestPatchRendersTheConfiguredTTL pins that the window a resolver caches for
// is the one configured, not one baked into a render.
func TestPatchRendersTheConfiguredTTL(t *testing.T) {
	held := map[string]*instance{
		"shop/web": instanceOn("shop", "web", nil, map[string]string{"net-a": "10.0.0.5"}),
	}

	const ttl = 30

	s := newState(map[string]zoneSerial{})
	s.apply("shop/web", held["shop/web"], ttl)

	answers, ok := s.snapshot().Answers("web.shop.example.")
	require.True(t, ok)

	for _, records := range answers {
		for _, rr := range records[dns.TypeA] {
			assert.Equal(t, uint32(ttl), rr.Header().Ttl)
		}
	}
}

// TestTheSerialStepsOnlyWhenTheZoneMoved pins what a secondary keys off: a
// serial step is a claim the zone changed, so a publish that changed nothing
// makes none, and a cold one has confirmed nothing so it may not either.
func TestTheSerialStepsOnlyWhenTheZoneMoved(t *testing.T) {
	const zone = "shop.example."

	s := newState(nil)

	web := instanceOn("shop", "web", nil, map[string]string{"net-a": "10.0.0.5"})

	s.apply("shop/web", web, 5)
	s.step(true, 5)

	assert.Equal(t, uint32(1), s.serials[zone].Serial, "a zone nothing published before starts at 1")

	// Published again with nothing having moved.
	s.step(true, 5)
	assert.Equal(t, uint32(1), s.serials[zone].Serial, "a publish that changed nothing claims nothing")

	s.apply("shop/web",
		instanceOn("shop", "web", nil, map[string]string{"net-a": "10.0.0.9"}), 5)
	s.step(true, 5)

	assert.Equal(t, uint32(2), s.serials[zone].Serial, "the address moved, so the zone did")

	// Cold has read nothing, so it has no standing to say the zone changed.
	s.apply("shop/web",
		instanceOn("shop", "web", nil, map[string]string{"net-a": "10.0.0.11"}), 5)
	s.step(false, 5)

	assert.Equal(t, uint32(2), s.serials[zone].Serial, "cold confirmed nothing")

	// And what cold left dirty is not owed a step once warm says so.
	s.step(true, 5)
	assert.Equal(t, uint32(2), s.serials[zone].Serial)
}

// TestEverythingLeavingEmptiesTheTrees is the invariant a subtracting patcher
// lives or dies by: a contribution taken back imprecisely leaves a residue, and
// nothing else notices until a name answers an address that is gone.
func TestEverythingLeavingEmptiesTheTrees(t *testing.T) {
	for name, held := range fleets() {
		t.Run(name, func(t *testing.T) {
			for _, keys := range orders(held) {
				s := newState(map[string]zoneSerial{})
				for _, key := range keys {
					s.apply(key, held[key], 5)
				}

				// Removed in the same order, which is not the order they went in
				// for every permutation.
				for _, key := range keys {
					s.apply(key, nil, 5)
				}

				// Publish is what sweeps a zone nothing came back to.
				s.step(true, 5)

				snap := s.snapshot()

				assert.Empty(t, everyName(snap.Tree), "a name outlived every instance, applied as %v", keys)
				assert.Empty(t, s.nets, "a wire outlived every instance on it, applied as %v", keys)
				assert.Empty(t, s.serials, "a zone outlived every name in it, applied as %v", keys)
				assert.Empty(t, s.held, "applied as %v", keys)
			}
		})
	}
}

// TestAContestedAliasIsAnsweredForNobody pins what two claimants get: a name
// may hold one CNAME, so both writing theirs is a name nobody is answered for.
// Whichever leaves first frees it for the other, with nothing tracking them.
func TestAContestedAliasIsAnsweredForNobody(t *testing.T) {
	s := newState(nil)

	s.apply("shop/web",
		instanceOn("shop", "web", map[string]string{metaAliases: "www"}, map[string]string{"net-a": "10.0.0.5"}), 5)
	s.apply("shop/api",
		instanceOn("shop", "api", map[string]string{metaAliases: "www"}, map[string]string{"net-a": "10.0.0.6"}), 5)

	view := ecs_view.New()
	wire(newXFR(nil), view, nil)

	view.Replace(s.snapshot())
	view.SetHealthy(true)

	a := &adapter{chain: view}

	w := &recorder{}
	a.ServeDNS(w, subnetQuery("www.shop.example.", "10.0.0.5"))
	assert.Equal(t, dns.RcodeNameError, w.msg.Rcode, "two claimants, so the name is nobody's")

	// Its own name is untouched by the alias it could not have.
	w = &recorder{}
	a.ServeDNS(w, subnetQuery("web.shop.example.", "10.0.0.5"))
	require.Equal(t, dns.RcodeSuccess, w.msg.Rcode)

	s.apply("shop/api", nil, 5)
	view.Replace(s.snapshot())

	w = &recorder{}
	a.ServeDNS(w, subnetQuery("www.shop.example.", "10.0.0.5"))
	assert.Equal(t, dns.RcodeSuccess, w.msg.Rcode,
		"one claimant leaving frees the name for the one that is left")
}

// TestPatchInventsAShadowingZoneForAnAbsoluteAlias pins that a name aliased out
// of every served zone gets one of its own, claiming that name and nothing else.
func TestPatchInventsAShadowingZoneForAnAbsoluteAlias(t *testing.T) {
	s := newState(nil)

	s.apply("shop/gateway",
		instanceOn("shop", "gateway",
			map[string]string{metaAliases: "gw.other.com."},
			map[string]string{"net-a": "10.0.0.5"}), 5)

	snap := s.snapshot()

	z := snap.ZoneOf("gw.other.com.")
	require.NotNil(t, z, "the aliased name fell outside every zone and got none")
	assert.True(t, z.Shadowing, "a domain we hold one name in is not ours to deny for")

	_, answers := snap.Answers("gw.other.com.")
	assert.True(t, answers, "the aliased name itself is served")

	// Its own zone is not shadowing: the instance's own name is in it.
	own := snap.ZoneOf("gateway.shop.example.")
	require.NotNil(t, own)
	assert.False(t, own.Shadowing)

	// And the invented zone goes when the alias that made it does, swept by the
	// publish rather than the patch.
	s.apply("shop/gateway", nil, 5)
	s.step(true, 5)

	assert.Nil(t, s.snapshot().ZoneOf("gw.other.com."),
		"a zone nobody asked for outlived the one name it was invented for")
}

// orders is every order a fleet's events could have arrived in.
func orders(held map[string]*instance) [][]string {
	keys := make([]string, 0, len(held))
	for key := range held {
		keys = append(keys, key)
	}

	var out [][]string

	var walk func(taken, left []string)

	walk = func(taken, left []string) {
		if len(left) == 0 {
			out = append(out, append([]string{}, taken...))

			return
		}

		for i, key := range left {
			rest := append(append([]string{}, left[:i]...), left[i+1:]...)
			walk(append(taken, key), rest)
		}
	}

	walk(nil, keys)

	return out
}
