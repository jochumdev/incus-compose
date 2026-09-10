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
		targetProject string
		want          bool
	}{
		{
			name:          "scope=global in default matches shop",
			daemonScope:   shared.DNSScopeGlobal,
			daemonProject: "default",
			targetProject: "shop",
			want:          true,
		},
		{
			name:          "scope=global matches any project",
			daemonScope:   shared.DNSScopeGlobal,
			daemonProject: "system",
			targetProject: "blog",
			want:          true,
		},
		{
			name:          "scope=project matches same project",
			daemonScope:   shared.DNSScopeProject,
			daemonProject: "shop",
			targetProject: "shop",
			want:          true,
		},
		{
			name:          "scope=project does NOT match different project",
			daemonScope:   shared.DNSScopeProject,
			daemonProject: "shop",
			targetProject: "blog",
			want:          false,
		},
		{
			name:          "scope=alpha,beta matches alpha",
			daemonScope:   "alpha,beta",
			daemonProject: "infra",
			targetProject: "alpha",
			want:          true,
		},
		{
			name:          "scope=alpha,beta matches beta",
			daemonScope:   "alpha,beta",
			daemonProject: "infra",
			targetProject: "beta",
			want:          true,
		},
		{
			name:          "scope=alpha,beta does NOT match gamma",
			daemonScope:   "alpha,beta",
			daemonProject: "infra",
			targetProject: "gamma",
			want:          false,
		},
		{
			name:          "scope=alpha, beta with whitespace matches beta",
			daemonScope:   "alpha, beta",
			daemonProject: "infra",
			targetProject: "beta",
			want:          true,
		},
		{
			name:          "empty scope matches nothing",
			daemonScope:   "",
			daemonProject: "shop",
			targetProject: "shop",
			want:          false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := matchDNSScope(tt.daemonScope, tt.daemonProject, tt.targetProject)
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
