package main

import (
	"context"
	"net/http"
	"sort"
	"testing"

	"github.com/compose-spec/compose-go/v2/types"
	incusApi "github.com/lxc/incus/v7/shared/api"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lxc/incus-compose/client"
	"github.com/lxc/incus-compose/project"
)

// mockResource satisfies client.Resource for testing without a real Incus connection.
type mockResource struct {
	name     string
	kind     client.Kind
	priority int
	ensured  bool
	created  bool
}

func newMockResource(name string, kind client.Kind) *mockResource {
	return &mockResource{name: name, kind: kind, priority: client.PriorityInstance}
}

func (m *mockResource) Name() string      { return m.name }
func (m *mockResource) IncusName() string { return m.name }
func (m *mockResource) Kind() client.Kind { return m.kind }
func (m *mockResource) Priority() int     { return m.priority }
func (m *mockResource) IsEnsured() bool   { return m.ensured }
func (m *mockResource) Created() bool     { return m.created }

var _ client.Resource = (*mockResource)(nil)

// newProject builds a minimal project.Project from a types.Project for testing.
func newProject(tp *types.Project) *project.Project {
	p := project.New()
	p.Project = tp
	return p
}

// resourceNames returns sorted resource names for deterministic assertions.
func resourceNames(res []client.Resource) []string {
	names := make([]string, len(res))
	for i, r := range res {
		names[i] = r.Name()
	}
	sort.Strings(names)
	return names
}

// resultKeys returns sorted keys of the result map.
func resultKeys(m map[string][]client.Resource) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func TestFilterResources(t *testing.T) {
	t.Parallel()

	webInst := newMockResource("web", client.KindInstance)
	webNet := newMockResource("net-web", client.KindNetwork)
	dbInst := newMockResource("db", client.KindInstance)
	dbVol := newMockResource("vol-db", client.KindStorageVolume)

	// makeInput returns a fresh map each call; filterResources mutates the map
	// when OnlyServices is empty (result = in), so parallel subtests must not share one.
	makeInput := func() map[string][]client.Resource {
		return map[string][]client.Resource{
			"web": {webInst, webNet},
			"db":  {dbInst, dbVol},
		}
	}

	webProject := newProject(&types.Project{
		Services: types.Services{
			"web": types.ServiceConfig{
				Name: "web",
				DependsOn: types.DependsOnConfig{
					"db": types.ServiceDependency{Condition: types.ServiceConditionStarted},
				},
			},
			"db": types.ServiceConfig{Name: "db"},
		},
	})

	emptyProject := newProject(&types.Project{Services: types.Services{}})

	tests := []struct {
		name     string
		p        *project.Project
		inFn     func() map[string][]client.Resource
		args     filterResourcesArgs
		wantKeys []string
		check    func(t *testing.T, got map[string][]client.Resource)
	}{
		{
			name:     "no_filter_returns_all",
			p:        emptyProject,
			inFn:     makeInput,
			args:     filterResourcesArgs{},
			wantKeys: []string{"db", "web"},
		},
		{
			name:     "only_services_single",
			p:        emptyProject,
			inFn:     makeInput,
			args:     filterResourcesArgs{OnlyServices: []string{"web"}},
			wantKeys: []string{"web"},
		},
		{
			name:     "only_services_both",
			p:        emptyProject,
			inFn:     makeInput,
			args:     filterResourcesArgs{OnlyServices: []string{"web", "db"}},
			wantKeys: []string{"db", "web"},
		},
		{
			name:     "only_services_unknown_skipped",
			p:        emptyProject,
			inFn:     makeInput,
			args:     filterResourcesArgs{OnlyServices: []string{"missing"}},
			wantKeys: []string{},
		},
		{
			name:     "with_dependencies_adds_dep_key",
			p:        webProject,
			inFn:     makeInput,
			args:     filterResourcesArgs{OnlyServices: []string{"web"}, WithDependencies: true},
			wantKeys: []string{"db", "web"},
			check: func(t *testing.T, got map[string][]client.Resource) {
				// dep resources land under their own key, not under "web"
				assert.Equal(t, []string{"db", "vol-db"}, resourceNames(got["db"]))
				assert.Equal(t, []string{"net-web", "web"}, resourceNames(got["web"]))
			},
		},
		{
			name:     "with_dependencies_no_only_services_ignored",
			p:        webProject,
			inFn:     makeInput,
			args:     filterResourcesArgs{WithDependencies: true},
			wantKeys: []string{"db", "web"},
		},
		{
			name:     "with_dependencies_dep_absent_from_resources",
			p:        webProject,
			inFn:     func() map[string][]client.Resource { return map[string][]client.Resource{"web": {webInst}} },
			args:     filterResourcesArgs{OnlyServices: []string{"web"}, WithDependencies: true},
			wantKeys: []string{"web"}, // dep skipped gracefully
		},
		{
			// OnlyServices=["db"]: reverse dep pulls in web because web DependsOn db.
			name:     "with_reverse_dependencies_adds_dependent",
			p:        webProject,
			inFn:     makeInput,
			args:     filterResourcesArgs{OnlyServices: []string{"db"}, WithDependencies: true, Reverse: true},
			wantKeys: []string{"db", "web"},
			check: func(t *testing.T, got map[string][]client.Resource) {
				assert.Equal(t, []string{"db", "vol-db"}, resourceNames(got["db"]))
				assert.Equal(t, []string{"net-web", "web"}, resourceNames(got["web"]))
			},
		},
		{
			// Forward deps (Reverse=false) must not pull in services that depend on OnlyServices.
			name:     "with_forward_dependencies_does_not_add_dependents",
			p:        webProject,
			inFn:     makeInput,
			args:     filterResourcesArgs{OnlyServices: []string{"db"}, WithDependencies: true, Reverse: false},
			wantKeys: []string{"db"}, // web depends on db, but forward mode must not pull web in
		},
		{
			// Reverse dep is skipped when WithDependencies is false.
			name:     "with_reverse_dependencies_requires_flag",
			p:        webProject,
			inFn:     makeInput,
			args:     filterResourcesArgs{OnlyServices: []string{"db"}, WithDependencies: false, Reverse: true},
			wantKeys: []string{"db"},
		},
		{
			// Dependent service (web) absent from in map: reverse dep skips gracefully.
			name:     "with_reverse_dependencies_dependent_absent_from_resources",
			p:        webProject,
			inFn:     func() map[string][]client.Resource { return map[string][]client.Resource{"db": {dbInst}} },
			args:     filterResourcesArgs{OnlyServices: []string{"db"}, WithDependencies: true, Reverse: true},
			wantKeys: []string{"db"},
		},
		{
			name: "exclude_kinds_removes_non_instance",
			p:    emptyProject,
			inFn: makeInput,
			args: filterResourcesArgs{ExcludeKinds: []client.Kind{client.KindNetwork, client.KindStorageVolume}},
			check: func(t *testing.T, got map[string][]client.Resource) {
				assert.Equal(t, []string{"web"}, resourceNames(got["web"]))
				assert.Equal(t, []string{"db"}, resourceNames(got["db"]))
			},
		},
		{
			name: "exclude_kinds_never_removes_instance",
			p:    emptyProject,
			inFn: makeInput,
			args: filterResourcesArgs{ExcludeKinds: []client.Kind{client.KindInstance}},
			check: func(t *testing.T, got map[string][]client.Resource) {
				// KindInstance in ExcludeKinds is a no-op; instances always kept
				assert.Contains(t, resourceNames(got["web"]), "web")
				assert.Contains(t, resourceNames(got["db"]), "db")
			},
		},
		{
			name:     "exclude_kinds_nil_no_filtering",
			p:        emptyProject,
			inFn:     makeInput,
			args:     filterResourcesArgs{ExcludeKinds: nil},
			wantKeys: []string{"db", "web"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := filterResources(tt.p, tt.inFn(), tt.args)
			if tt.wantKeys != nil {
				assert.Equal(t, tt.wantKeys, resultKeys(got))
			}
			if tt.check != nil {
				tt.check(t, got)
			}
		})
	}
}

// TestDiscoveredResources pins what an ensure pass adds to a stack.
func TestDiscoveredResources(t *testing.T) {
	t.Parallel()
	c := client.NewOfflineClient(t.Context(), "discovered-test")

	kept, err := c.Resource(client.KindStorageVolume, "declared", &client.StorageVolumeConfig{})
	require.NoError(t, err)

	before := c.Resources()

	found, err := c.Resource(client.KindStorageVolume, "auto-web-db", &client.StorageVolumeConfig{})
	require.NoError(t, err)

	image, err := c.Resource(client.KindImage, "docker.io/nginx:alpine", &client.ImageConfig{})
	require.NoError(t, err)

	got := discoveredResources(before, c.Resources(), []client.Kind{client.KindImage})
	require.Equal(t, []client.Resource{found}, got, "only what the pass added, and not an excluded kind")
	require.NotContains(t, got, kept)
	require.NotContains(t, got, image)

	require.Empty(t, discoveredResources(c.Resources(), c.Resources(), nil))
}

// deleteProjectOnCleanup removes an Incus project on teardown, for a test whose
// compose file down cannot read.
func deleteProjectOnCleanup(t *testing.T, project string) {
	t.Helper()

	t.Cleanup(func() {
		gc, err := client.NewTestClient(context.Background())
		if err == nil {
			err = gc.Connect()
		}

		if err != nil {
			t.Errorf("connecting to delete project %s: %v", project, err)

			return
		}

		err = gc.DeleteProject(project, true)
		if err != nil && !incusApi.StatusErrorCheck(err, http.StatusNotFound) {
			t.Errorf("deleting project %s: %v", project, err)
		}
	})
}
