package project

import (
	"testing"

	"github.com/compose-spec/compose-go/v2/types"
	incusApi "github.com/lxc/incus/v7/shared/api"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func testClusterMembers() []incusApi.ClusterMember {
	return []incusApi.ClusterMember{
		{
			ServerName:   "node1",
			Status:       "Online",
			Architecture: "x86_64",
			ClusterMemberPut: incusApi.ClusterMemberPut{
				Roles:         []string{"database", "database-leader"},
				Groups:        []string{"default", "web"},
				FailureDomain: "az1",
				Config: map[string]string{
					"user.disk": "ssd",
					"user.tier": "frontend",
				},
			},
		},
		{
			ServerName:   "node2",
			Status:       "Online",
			Architecture: "x86_64",
			ClusterMemberPut: incusApi.ClusterMemberPut{
				Roles:         []string{"database"},
				Groups:        []string{"default", "web"},
				FailureDomain: "az1",
				Config: map[string]string{
					"user.disk": "hdd",
					"user.tier": "backend",
				},
			},
		},
		{
			ServerName:   "node3",
			Status:       "Online",
			Architecture: "aarch64",
			ClusterMemberPut: incusApi.ClusterMemberPut{
				Roles:         []string{"worker"},
				Groups:        []string{"default", "compute"},
				FailureDomain: "az2",
				Config: map[string]string{
					"user.disk": "ssd",
					"user.tier": "backend",
				},
			},
		},
		{
			ServerName:   "node4-offline",
			Status:       "Evacuated",
			Architecture: "x86_64",
			ClusterMemberPut: incusApi.ClusterMemberPut{
				Roles:         []string{"worker"},
				Groups:        []string{"default"},
				FailureDomain: "az2",
			},
		},
	}
}

func TestFilterClusterMembers(t *testing.T) {
	t.Parallel()

	members := testClusterMembers()

	t.Run("online only by default", func(t *testing.T) {
		t.Parallel()

		got, err := filterClusterMembers(members, nil)
		require.NoError(t, err)
		require.Len(t, got, 3)
	})

	t.Run("node.hostname and node.id", func(t *testing.T) {
		t.Parallel()

		got, err := filterClusterMembers(members, []string{"node.hostname == node2"})
		require.NoError(t, err)
		require.Len(t, got, 1)
		assert.Equal(t, "node2", got[0].ServerName)

		got, err = filterClusterMembers(members, []string{"node.id != node2"})
		require.NoError(t, err)
		require.Len(t, got, 2)
		assert.Equal(t, "node1", got[0].ServerName)
		assert.Equal(t, "node3", got[1].ServerName)
	})

	t.Run("node.role", func(t *testing.T) {
		t.Parallel()

		got, err := filterClusterMembers(members, []string{"node.role == database"})
		require.NoError(t, err)
		require.Len(t, got, 2)

		got, err = filterClusterMembers(members, []string{"node.role != database"})
		require.NoError(t, err)
		require.Len(t, got, 1)
		assert.Equal(t, "node3", got[0].ServerName)
	})

	t.Run("node.labels", func(t *testing.T) {
		t.Parallel()

		got, err := filterClusterMembers(members, []string{"node.labels.disk == ssd", "node.labels.tier == backend"})
		require.NoError(t, err)
		require.Len(t, got, 1)
		assert.Equal(t, "node3", got[0].ServerName)
	})

	t.Run("node.group and node.failure_domain", func(t *testing.T) {
		t.Parallel()

		got, err := filterClusterMembers(members, []string{"node.group == web"})
		require.NoError(t, err)
		require.Len(t, got, 2)

		got, err = filterClusterMembers(members, []string{"node.failure_domain == az2"})
		require.NoError(t, err)
		require.Len(t, got, 1)
		assert.Equal(t, "node3", got[0].ServerName)
	})

	t.Run("invalid constraint", func(t *testing.T) {
		t.Parallel()

		_, err := filterClusterMembers(members, []string{"invalid_syntax"})
		require.ErrorContains(t, err, "invalid placement constraint")

		_, err = filterClusterMembers(members, []string{"node.unknown == foo"})
		require.ErrorContains(t, err, "unsupported placement constraint key")
	})
}

func TestOrderClusterMembersSpread(t *testing.T) {
	t.Parallel()

	members := testClusterMembers()[:3] // node1 (az1, ssd), node2 (az1, hdd), node3 (az2, ssd)

	ordered := orderClusterMembers(members, []types.PlacementPreferences{
		{Spread: "node.failure_domain"},
		{Spread: "node.labels.disk"},
	}, "node2")
	require.Len(t, ordered, 3)
	// First bucket az1 (preferredServer node2 first), second bucket az2 (node3), then remaining az1 (node1)
	assert.Equal(t, []string{"node2", "node3", "node1"}, []string{
		ordered[0].ServerName,
		ordered[1].ServerName,
		ordered[2].ServerName,
	})
}

func TestResolveServicePlacementEnv(t *testing.T) {
	t.Parallel()

	members := testClusterMembers()

	t.Run("single replica defaults to connected server on bridge or local storage (#213)", func(t *testing.T) {
		t.Parallel()

		env := clusterPlacementEnv{
			clustered:   true,
			serverName:  "node2",
			networkType: "bridge",
			remotePool:  false,
			members:     members,
		}
		svc := types.ServiceConfig{Name: "web"}

		desired, targets, warn, err := resolveServicePlacementEnv(env, svc, nil, false)
		require.NoError(t, err)
		assert.False(t, warn)
		assert.Equal(t, 1, desired)
		assert.Equal(t, []string{"node2"}, targets)

		// With OVN + remote storage and no explicit placement, target is left empty for Incus scheduler
		env.networkType = "ovn"
		env.remotePool = true
		desired, targets, warn, err = resolveServicePlacementEnv(env, svc, nil, false)
		require.NoError(t, err)
		assert.False(t, warn)
		assert.Equal(t, 1, desired)
		assert.Equal(t, []string{""}, targets)
	})

	t.Run("scaling on local storage fails by default and warns with override (#1)", func(t *testing.T) {
		t.Parallel()

		env := clusterPlacementEnv{
			clustered:   true,
			serverName:  "node1",
			networkType: "ovn",
			remotePool:  false,
			members:     members,
		}
		replicas := 2
		svc := types.ServiceConfig{
			Name: "web",
			Deploy: &types.DeployConfig{
				Replicas: &replicas,
			},
		}

		_, _, _, err := resolveServicePlacementEnv(env, svc, nil, false)
		require.ErrorContains(t, err, "cannot deploy multiple replicas on a cluster without shared (remote) storage")

		desired, targets, warn, err := resolveServicePlacementEnv(env, svc, nil, true)
		require.NoError(t, err)
		assert.True(t, warn)
		assert.Equal(t, 2, desired)
		assert.Equal(t, []string{"node1", "node2"}, targets)
	})

	t.Run("deploy.mode global and max_replicas_per_node on remote storage (#1)", func(t *testing.T) {
		t.Parallel()

		env := clusterPlacementEnv{
			clustered:   true,
			serverName:  "node1",
			networkType: "ovn",
			remotePool:  true,
			members:     members,
		}
		svc := types.ServiceConfig{
			Name: "agent",
			Deploy: &types.DeployConfig{
				Mode: "global",
				Placement: types.Placement{
					Constraints: []string{"node.role == database"},
				},
			},
		}

		desired, targets, warn, err := resolveServicePlacementEnv(env, svc, nil, false)
		require.NoError(t, err)
		assert.False(t, warn)
		assert.Equal(t, 2, desired)
		assert.Equal(t, []string{"node1", "node2"}, targets)

		// Exceeding max_replicas_per_node returns an error
		four := 4
		svc.Deploy.Mode = "replicated"
		svc.Deploy.Replicas = &four
		svc.Deploy.Placement.MaxReplicas = 1

		_, _, _, err = resolveServicePlacementEnv(env, svc, nil, false)
		require.ErrorContains(t, err, "exceed max_replicas_per_node")
	})

	t.Run("deploy.restart_policy fallback (#1)", func(t *testing.T) {
		t.Parallel()

		assert.Equal(t, "no", serviceRestartPolicy(types.ServiceConfig{
			Deploy: &types.DeployConfig{RestartPolicy: &types.RestartPolicy{Condition: "none"}},
		}))
		assert.Equal(t, "on-failure", serviceRestartPolicy(types.ServiceConfig{
			Deploy: &types.DeployConfig{RestartPolicy: &types.RestartPolicy{Condition: "on-failure"}},
		}))
		assert.Equal(t, "always", serviceRestartPolicy(types.ServiceConfig{
			Deploy: &types.DeployConfig{RestartPolicy: &types.RestartPolicy{Condition: "any"}},
		}))
		assert.Equal(t, "unless-stopped", serviceRestartPolicy(types.ServiceConfig{
			Restart: "unless-stopped",
			Deploy:  &types.DeployConfig{RestartPolicy: &types.RestartPolicy{Condition: "none"}},
		}))
	})
}
