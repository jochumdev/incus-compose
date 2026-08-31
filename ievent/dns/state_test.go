package dns

import (
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lxc/incus-compose/ievent/dns/ecs_view"
)

func TestStateCountsTheViewsOverAWire(t *testing.T) {
	zone := "shop.example."

	t.Run("two instances on one wire share the view over it", func(t *testing.T) {
		s := newState(nil)

		web := instanceOn("shop", "web", nil, map[string]string{"net-a": "10.0.0.5"})
		api := instanceOn("shop", "api", nil, map[string]string{"net-a": "10.0.0.6"})

		mine, _ := s.view(web)
		theirs, _ := s.view(api)

		assert.Equal(t, mine, theirs, "one network set is one view")

		// One view, not two: an instance on a single wire queries from exactly
		// the set that wire's anonymous view already names.
		assert.Len(t, s.nets[onWire("net-a")], 1)

		freed, _ := s.release(web)
		assert.Empty(t, freed, "the wire still has one instance on it")
		assert.Len(t, s.nets[onWire("net-a")], 1, "and the view over it is still claimed")

		freed, _ = s.release(api)
		assert.Equal(t, []string{onWire("net-a")}, freed,
			"the last instance leaving is what frees the wire")
		assert.NotContains(t, s.nets, onWire("net-a"))
	})

	t.Run("a second network set is a second view over the wire they share", func(t *testing.T) {
		s := newState(nil)

		web := instanceOn("shop", "web", nil, map[string]string{"net-a": "10.0.0.5"})
		gate := instanceOn("shop", "gate", nil, map[string]string{"net-a": "10.0.0.7", "net-b": "10.1.0.7"})

		own, _ := s.view(web)
		both, fresh := s.view(gate)

		assert.Contains(t, fresh, both, "a set nothing had is a view names have to be written under")

		require.NotEqual(t, own, both)

		assert.ElementsMatch(t,
			[]ecs_view.ViewID{own, both},
			s.views(onWire("net-a")),
			"a name on net-a has to be written under every view that can see it")

		assert.ElementsMatch(t,
			[]ecs_view.ViewID{both, ecs_view.ViewOf([]string{onWire("net-b")})},
			s.views(onWire("net-b")))
	})

	t.Run("the index names the views a patched name answers in", func(t *testing.T) {
		s := newState(nil)

		s.apply("shop/web", instanceOn("shop", "web", nil, map[string]string{"net-a": "10.0.0.5"}), 5)
		s.apply("shop/gate", instanceOn("shop", "gate", nil,
			map[string]string{"net-a": "10.0.0.7", "net-b": "10.1.0.7"}), 5)

		answers, ok := s.snapshot().Answers("web." + zone)
		require.True(t, ok)

		// web sits on net-a alone, so exactly the views over net-a answer for it.
		got := make([]ecs_view.ViewID, 0, len(answers))
		for id := range answers {
			got = append(got, id)
		}

		want := s.views(onWire("net-a"))

		slices.Sort(got)
		slices.Sort(want)

		assert.Equal(t, want, got)
	})
}
