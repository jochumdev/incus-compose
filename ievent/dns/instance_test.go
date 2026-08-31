package dns

import (
	"testing"

	incusapi "github.com/lxc/incus/v7/shared/api"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ours is the raw config an instance carries one dns label under.
func ours(key, value string) map[string]string {
	return map[string]string{userLabel(labelPrefix + key): value}
}

func TestInstanceCarriesWhatItContributes(t *testing.T) {
	t.Run("its own name, and the service name its replicas share", func(t *testing.T) {
		inst := patchInstance(labeled("shop", "web", nil, nil), "example.")
		require.NotNil(t, inst)

		assert.Equal(t, []string{"web.shop.example."}, inst.names)

		scaled := patchInstance(
			labeled("shop", "web", ours(metaService, "frontend"), nil), "example.")
		require.NotNil(t, scaled)

		assert.Equal(t, []string{"web.shop.example.", "frontend.shop.example."}, scaled.names,
			"its own name stays first, which is what a PTR answers with")
	})

	t.Run("the aliases it claims", func(t *testing.T) {
		inst := patchInstance(
			labeled("shop", "web", ours(metaAliases, "shop,www"), nil), "example.")
		require.NotNil(t, inst)

		assert.Equal(t, []string{"shop.shop.example.", "www.shop.example."}, inst.alias)
	})

	t.Run("a reverse name per address, on the network it sits on", func(t *testing.T) {
		inst := patchInstance(labeled("shop", "web", nil, nil), "example.")
		require.NotNil(t, inst)

		assert.Equal(t,
			map[string]ptrOn{"2.0.0.10.in-addr.arpa.": {key: "shop/net0", zone: "0.0.10.in-addr.arpa."}},
			inst.ptrs)
	})

	t.Run("no reverse name where the network does not reach the address", func(t *testing.T) {
		// What Incus reports for a network it does not manage the addressing of.
		ev := event(incusapi.EventLifecycleInstanceStarted, "shop", "web").WithInstance(
			read("shop", true, nil, map[string]onNet{
				"net0": {addrs: []string{"10.0.0.2"}},
			}), true)

		inst := patchInstance(ev, "example.")
		require.NotNil(t, inst)

		assert.Empty(t, inst.ptrs)
		assert.Equal(t, []string{"web.shop.example."}, inst.names, "still served forward")
	})

	t.Run("every name it carries is one a patch answers to", func(t *testing.T) {
		config := ours(metaService, "frontend")
		config[userLabel(labelPrefix+metaAliases)] = "www"

		inst := patchInstance(labeled("shop", "web", config, nil), "example.")
		require.NotNil(t, inst)

		s := newState(nil)
		s.apply("shop/web", inst, 5)

		snap := s.snapshot()

		carried := append(append([]string{}, inst.names...), inst.alias...)
		for name := range inst.ptrs {
			carried = append(carried, name)
		}

		for _, name := range carried {
			_, held := snap.Answers(name)
			assert.True(t, held, "%s is carried but not answered", name)
		}
	})
}
