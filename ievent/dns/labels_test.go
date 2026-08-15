package dns

import (
	"net/netip"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lxc/incus-compose/ievent/iutil"
)

// userLabel builds the raw config key an instance or profile label arrives
// under. WithInstance only picks up this namespace; WithProject's config comes
// straight off the project, which carries no such prefix.
func userLabel(key string) string { return "user.label." + key }

// labeled is one instance event carrying both configurations, the way the
// enricher hands it over.
func labeled(project, name string, instance, own map[string]string) *iutil.Event {
	net := iutil.NewNetwork("net0", project, true,
		[]netip.Prefix{netip.MustParsePrefix("10.0.0.0/24")},
		[]netip.Addr{netip.MustParseAddr("10.0.0.2")}, nil)

	return iutil.NewEvent(time.Now(), "instance-updated", project, name, "").
		WithInstance(true, instance, map[string]*iutil.Network{project + "/net0": net}).
		WithProject(own)
}

// TestDistillJoinsTheProjectUnderTheInstance pins which way the two sets of
// labels fold: a project sets a default and an instance overrides it.
func TestDistillJoinsTheProjectUnderTheInstance(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		instance map[string]string
		own      map[string]string

		zone    string
		service string
	}{
		{
			name: "a project's zone names every instance in it",
			own:  map[string]string{labelPrefix + metaZone: "shop.example."},
			zone: "shop.example.",
		},
		{
			name:     "an instance overrides what its project asked for",
			instance: map[string]string{userLabel(labelPrefix + metaZone): "own.example."},
			own:      map[string]string{labelPrefix + metaZone: "shop.example."},
			zone:     "own.example.",
		},
		{
			name: "neither says, so the zone is the project under the suffix",
			zone: "shop.incus.",
		},
		{
			// Nobody wants a project-wide service, but the fold is the fold and
			// the case that matters is the instance still winning.
			name:     "the instance's service wins over the project's",
			instance: map[string]string{userLabel(labelPrefix + metaService): "api"},
			own:      map[string]string{labelPrefix + metaService: "web"},
			zone:     "shop.incus.",
			service:  "api",
		},
		{
			// Blank turns off a value inherited from a profile, so it neither
			// comes through as a setting nor shadows the project's.
			name:     "an instance blanking a key falls back to the project's",
			instance: map[string]string{userLabel(labelPrefix + metaZone): "  "},
			own:      map[string]string{labelPrefix + metaZone: "shop.example."},
			zone:     "shop.example.",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			inst := patchInstance(labeled("shop", "web", test.instance, test.own), nil, "incus")
			require.NotNil(t, inst)

			assert.Equal(t, test.zone, inst.zone)
			assert.Equal(t, test.service, inst.meta[metaService])
		})
	}
}

// TestProjectZoneOverridesTheGeneratedOne is the setting end to end: the zone
// the fleet publishes under is the one the label names, not <project>.<suffix>.
func TestProjectZoneOverridesTheGeneratedOne(t *testing.T) {
	t.Parallel()

	own := map[string]string{labelPrefix + metaZone: "my.zone.com"}

	held := map[string]*instance{
		"shop/web": patchInstance(labeled("shop", "web", nil, own), nil, "incus"),
		"shop/db":  patchInstance(labeled("shop", "db", nil, own), nil, "incus"),
	}

	snap := build(held, nil, 5)

	// Trailing dot: a query always carries one, so a label written without it
	// still has to match.
	require.Contains(t, snap.ByZone, "my.zone.com.")
	assert.NotContains(t, snap.ByZone, "shop.incus.",
		"the generated zone was published alongside the one the project named")

	// Every instance lands in it, since the setting is the project's.
	assert.Contains(t, snap.ByZone["my.zone.com."].Names, "web.my.zone.com.")
	assert.Contains(t, snap.ByZone["my.zone.com."].Names, "db.my.zone.com.")
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
				WithInstance(true, test.config, nil)

			assert.Equal(t, test.want, instanceLabels(ev)[metaService])
		})
	}
}

// TestProjectAliasesAreNotInherited pins the one key that does not fold from a
// project onto its instances: inherited, it is one name every instance claims.
func TestProjectAliasesAreNotInherited(t *testing.T) {
	t.Parallel()

	ev := labeled("shop", "web", nil, map[string]string{
		labelPrefix + metaAliases: "store",
		labelPrefix + metaZone:    "shop.example.",
	})

	inst := patchInstance(ev, nil, "incus")
	require.NotNil(t, inst)

	assert.NotContains(t, inst.meta, metaAliases,
		"a project-wide alias would be claimed by every instance in it at once")
	assert.Equal(t, "shop.example.", inst.zone, "the keys that do fold still fold")

	// The instance's own is read as it always was.
	own := patchInstance(labeled("shop", "web",
		map[string]string{userLabel(labelPrefix + metaAliases): "store"},
		map[string]string{labelPrefix + metaAliases: "shop"}), nil, "incus")
	require.NotNil(t, own)

	assert.Equal(t, "store", own.meta[metaAliases])
}
