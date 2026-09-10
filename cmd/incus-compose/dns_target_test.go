package main

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lxc/incus-compose/client"
	"github.com/lxc/incus-compose/shared"
)

func TestMatchDNSScope(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		daemonScope   string
		daemonProject string
		targetScope   string
		targetProject string
		want          bool
	}{
		{
			name:          "scope=global matches target scope=global",
			daemonScope:   shared.DNSScopeGlobal,
			daemonProject: "default",
			targetScope:   shared.DNSScopeGlobal,
			targetProject: "shop",
			want:          true,
		},
		{
			name:          "scope=global does not match custom target scope",
			daemonScope:   shared.DNSScopeGlobal,
			daemonProject: "system",
			targetScope:   "custom",
			targetProject: "blog",
			want:          false,
		},
		{
			name:          "scope=project matches same project with scope=project",
			daemonScope:   shared.DNSScopeProject,
			daemonProject: "shop",
			targetScope:   shared.DNSScopeProject,
			targetProject: "shop",
			want:          true,
		},
		{
			name:          "scope=project does NOT match different project",
			daemonScope:   shared.DNSScopeProject,
			daemonProject: "shop",
			targetScope:   shared.DNSScopeProject,
			targetProject: "blog",
			want:          false,
		},
		{
			name:          "scope=alpha,beta matches target scope alpha",
			daemonScope:   "alpha,beta",
			daemonProject: "infra",
			targetScope:   "alpha",
			targetProject: "shop",
			want:          true,
		},
		{
			name:          "scope=alpha,beta matches target scope beta",
			daemonScope:   "alpha,beta",
			daemonProject: "infra",
			targetScope:   "beta",
			targetProject: "shop",
			want:          true,
		},
		{
			name:          "scope=alpha,beta does NOT match target scope gamma",
			daemonScope:   "alpha,beta",
			daemonProject: "infra",
			targetScope:   "gamma",
			targetProject: "shop",
			want:          false,
		},
		{
			name:          "scope=alpha, beta with whitespace matches beta",
			daemonScope:   "alpha, beta",
			daemonProject: "infra",
			targetScope:   "beta",
			targetProject: "shop",
			want:          true,
		},
		{
			name:          "empty scope matches nothing",
			daemonScope:   "",
			daemonProject: "shop",
			targetScope:   "shop",
			targetProject: "shop",
			want:          false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := matchDNSScope(tt.daemonScope, tt.daemonProject, tt.targetScope, tt.targetProject)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestResolveDNSIPWhenNoDNS(t *testing.T) {
	t.Parallel()

	c := client.NewOfflineClient(t.Context(), "default")
	ip, err := resolveDNSIP(t.Context(), c, time.Second)
	require.Error(t, err)
	assert.Empty(t, ip)
}
