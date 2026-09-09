package dns

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lxc/incus-compose/ievent/iutil"
)

// userLabel builds the raw config key an instance or profile label arrives
// under. WithProject's config comes straight off the project, with no such prefix.
func userLabel(key string) string { return "user.label." + key }

// labeled is one instance event carrying both configurations, the way the
// enricher hands it over: the instance's own untouched, and the project's
// untouched. Folding one over the other is this plugin's business.
func labeled(project, name string, instance, own map[string]string) *iutil.Event {
	return iutil.NewEvent(time.Now(), "instance-updated", project, name, "").
		WithInstance(read(project, true, instance, map[string]onNet{
			"net0": addressed("10.0.0.2"),
		}), true).
		WithProject(iutil.NewProject(own))
}

// TestProjectZoneOverridesTheGeneratedOne is the setting end to end: the zone
// the fleet publishes under is the one the label names, not <project>.<suffix>.
func TestProjectZoneOverridesTheGeneratedOne(t *testing.T) {
	t.Parallel()

	own := map[string]string{labelPrefix + metaZone: "my.zone.com"}

	held := map[string]*instance{
		"shop/web": patchInstance(labeled("shop", "web", nil, own), "incus"),
		"shop/db":  patchInstance(labeled("shop", "db", nil, own), "incus"),
	}

	s := newState(nil)
	for key, inst := range held {
		s.apply(key, inst, 5)
	}

	snap := s.snapshot()

	// Trailing dot: a query always carries one, so a label written without it
	// still has to match.
	named := snap.ZoneOf("web.my.zone.com.")
	require.NotNil(t, named)
	assert.Equal(t, "my.zone.com.", named.Name)

	assert.Nil(t, snap.ZoneOf("web.shop.incus."),
		"the generated zone was published alongside the one the project named")

	// Every instance lands in it, since the setting is the project's.
	_, web := snap.Answers("web.my.zone.com.")
	assert.True(t, web)

	_, db := snap.Answers("db.my.zone.com.")
	assert.True(t, db)
}

// TestPatchInstanceNSResolvesAgainstTheZone pins where the zone's NS set comes
// from: the project's own label, resolved the way an alias is - a trailing dot
// absolute, anything else relative to the zone.
func TestPatchInstanceNSResolvesAgainstTheZone(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		own  map[string]string
		want []string
	}{
		{name: "no label names none"},
		{
			name: "a relative name takes the zone",
			own:  map[string]string{labelPrefix + metaNS: "ns1"},
			want: []string{"ns1.shop.incus."},
		},
		{
			name: "an absolute name is left alone",
			own:  map[string]string{labelPrefix + metaNS: "ns1.example.org."},
			want: []string{"ns1.example.org."},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			inst := patchInstance(labeled("shop", "web", nil, test.own), "incus")
			require.NotNil(t, inst)

			assert.Equal(t, test.want, inst.ns)
		})
	}
}

// TestInstanceLabelsPreferCompose pins why a compose fleet resolves without
// annotation, and which key wins when both name a service.
func TestInstanceLabelsPreferCompose(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		config map[string]string
		want   string
	}{
		{
			name:   "compose's own key names the service",
			config: map[string]string{userLabel(labelServiceCompose): "web"},
			want:   "web",
		},
		{
			name: "compose wins where both are set",
			config: map[string]string{
				userLabel(labelServiceCompose):       "web",
				userLabel(labelPrefix + metaService): "api",
			},
			want: "web",
		},
		{
			// Blank is off, same as everywhere else, so ours is left standing
			// rather than the name being blanked out.
			name: "a blank compose label leaves ours in place",
			config: map[string]string{
				userLabel(labelServiceCompose):       "  ",
				userLabel(labelPrefix + metaService): "api",
			},
			want: "api",
		},
		{
			name:   "ours alone still names the service",
			config: map[string]string{userLabel(labelPrefix + metaService): "api"},
			want:   "api",
		},
		{
			name:   "neither leaves it unnamed",
			config: map[string]string{},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			ev := iutil.NewEvent(time.Now(), "instance-updated", "shop", "web", "").
				WithInstance(read("shop", true, test.config, nil), true)

			assert.Equal(t, test.want, meta(ev)[metaService])
		})
	}
}

// TestAProjectMayNotNameTheService: a service is what one instance's replicas
// share. A project naming one would put every instance it holds under a single
// record, replicas of each other or not, and answer for none of them.
func TestAProjectMayNotNameTheService(t *testing.T) {
	t.Parallel()

	for _, key := range []string{labelPrefix + metaService, labelServiceCompose} {
		t.Run(key, func(t *testing.T) {
			t.Parallel()

			ev := labeled("shop", "web", nil, map[string]string{key: "frontend"})
			assert.Empty(t, meta(ev)[metaService], "a project named the service")

			// And it does not reach an instance that named its own either.
			ev = labeled("shop", "web", ours(metaService, "api"),
				map[string]string{key: "frontend"})

			assert.Equal(t, "api", meta(ev)[metaService],
				"a project took a service name off the instance that owns it")
		})
	}
}
