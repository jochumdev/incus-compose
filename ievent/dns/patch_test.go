package dns

import (
	"net/netip"
	"sort"
	"testing"

	incusapi "github.com/lxc/incus/v7/shared/api"
	"github.com/miekg/dns"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lxc/incus-compose/ievent/dns/ecs_view"
	"github.com/lxc/incus-compose/ievent/iutil"
)

// eventOn is one instance event as the enricher hands it over: read, running,
// on the given networks with one address each, carrying its own labels.
func eventOn(action, project, name string, meta map[string]string, nets map[string]string) *iutil.Event {
	config := map[string]string{}
	for key, value := range meta {
		config[userLabel(labelPrefix+key)] = value
	}

	return event(action, project, name).WithInstance(read(project, true, config, netsOf(nets)), true)
}

// netsOf is one network per key, each with one address on it and a /24 around
// that address, which is what makes the address reverse-resolvable.
//
// The wires belong to the default project, the way a bridge does unless a
// project has networks of its own: two projects naming one here are on one wire.
func netsOf(nets map[string]string) map[string]onNet {
	out := map[string]onNet{}

	for key, addr := range nets {
		sits := addressed(addr)
		sits.project = iutil.DefaultProject

		out[key] = sits
	}

	return out
}

// onWire is how netsOf keys one network, which is what a view is named by.
func onWire(name string) string { return iutil.NetworkKey(iutil.DefaultProject, name) }

// instanceOn is one instance as the enricher would have handed it over, derived
// rather than built by hand so it carries the names patchInstance works out.
func instanceOn(project, name string, meta map[string]string, nets map[string]string) *instance {
	return patchInstance(
		eventOn(incusapi.EventLifecycleInstanceStarted, project, name, meta, nets),
		"example.")
}

// patched applies a whole fleet one instance at a time and hands back the names
// tree, so a test can ask it what a build would have been asked.
func patched(held map[string]*instance, ttl uint32) *state {
	s := newState(map[string]zoneSerial{})

	for key, inst := range held {
		s.apply(key, inst, ttl)
	}

	return s
}

// sameAnswers reports whether two views of one name hold the same records,
// comparing by value since the two sides rendered their own.
func sameAnswers(t *testing.T, want, got ecs_view.ViewAnswer, name string) {
	t.Helper()

	require.Len(t, got, len(want), "%s is answered in a different set of views", name)

	for id, records := range want {
		mine, held := got[id]
		require.True(t, held, "%s is not answered in view %q", name, id)

		for qtype, rrs := range records {
			assert.Len(t, mine[qtype], len(rrs), "%s type %d in view %q", name, qtype, id)

			for _, rr := range rrs {
				assert.True(t, containsRR(mine[qtype], rr), "%s is missing %s", name, rr)
			}
		}
	}
}

func containsRR(held []dns.RR, want dns.RR) bool {
	for _, rr := range held {
		if sameRR(rr, want) {
			return true
		}
	}

	return false
}

// answered is what a querier at addr resolves for qname, as a query reaches it:
// the name, then the view the address places the querier in.
func answered(t *testing.T, snap *ecs_view.Snapshot, qname, addr string) []string {
	t.Helper()

	answers, held := snap.Answers(qname)
	if !held {
		return nil
	}

	view, placed := snap.ViewFor(netip.MustParseAddr(addr))
	require.True(t, placed, "%s is on no known network", addr)

	var out []string

	for _, rr := range answers[view][dns.TypeA] {
		out = append(out, rr.(*dns.A).A.String())
	}

	sort.Strings(out)

	return out
}

func TestPatch(t *testing.T) {
	zone := "shop.example."

	fleet := func() map[string]*instance {
		return map[string]*instance{
			"shop/web":  instanceOn("shop", "web", map[string]string{metaService: "frontend"}, map[string]string{"net-a": "10.0.0.5"}),
			"shop/api":  instanceOn("shop", "api", map[string]string{metaService: "frontend"}, map[string]string{"net-a": "10.0.0.6"}),
			"shop/gate": instanceOn("shop", "gate", nil, map[string]string{"net-a": "10.0.0.7", "net-b": "10.1.0.7"}),
		}
	}

	t.Run("a service name answers with every replica", func(t *testing.T) {
		snap := patched(fleet(), 5).snapshot()

		assert.Equal(t, []string{"10.0.0.5"}, answered(t, snap, "web."+zone, "10.0.0.5"))
		assert.Equal(t, []string{"10.0.0.5", "10.0.0.6"}, answered(t, snap, "frontend."+zone, "10.0.0.5"))
		assert.Equal(t, []string{"10.0.0.7"}, answered(t, snap, "gate."+zone, "10.0.0.5"))
	})

	t.Run("a replica leaving takes only its own addresses out", func(t *testing.T) {
		s := patched(fleet(), 5)
		s.apply("shop/api", nil, 5)

		snap := s.snapshot()

		assert.Equal(t, []string{"10.0.0.5"}, answered(t, snap, "frontend."+zone, "10.0.0.5"),
			"the service kept the replica that stayed")
		assert.Nil(t, answered(t, snap, "api."+zone, "10.0.0.5"),
			"its own name had nothing else answering there")
	})

	t.Run("an address moving leaves the name where it was", func(t *testing.T) {
		s := patched(fleet(), 5)

		s.apply("shop/web",
			instanceOn("shop", "web", map[string]string{metaService: "frontend"},
				map[string]string{"net-a": "10.0.0.9"}), 5)

		snap := s.snapshot()

		assert.Equal(t, []string{"10.0.0.9"}, answered(t, snap, "web."+zone, "10.0.0.9"))
		assert.Equal(t, []string{"10.0.0.6", "10.0.0.9"}, answered(t, snap, "frontend."+zone, "10.0.0.9"),
			"the address it left is not still answered for the service")
	})
}
