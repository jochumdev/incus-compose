package main

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/lxc/incus-compose/shared"
)

func TestResolveHealthdScope(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		projectConfig map[string]string
		cli           string
		compose       string
		want          string
		wantErr       string
	}{
		{
			name: "nothing set defaults to global",
			want: shared.HealthScopeGlobal,
		},
		{
			name:    "the compose file is used when nothing else says",
			compose: shared.HealthScopeProject,
			want:    shared.HealthScopeProject,
		},
		{
			name:    "the cli beats the compose file",
			cli:     shared.HealthScopeGlobal,
			compose: shared.HealthScopeProject,
			want:    shared.HealthScopeGlobal,
		},
		{
			name:          "the project beats the cli and the compose file",
			projectConfig: map[string]string{shared.HealthScopeKey: shared.HealthScopeProject},
			cli:           shared.HealthScopeGlobal,
			compose:       shared.HealthScopeGlobal,
			want:          shared.HealthScopeProject,
		},
		{
			name:          "an unrelated project key does not count",
			projectConfig: map[string]string{shared.HealthEnabledKey: "true"},
			want:          shared.HealthScopeGlobal,
		},
		{
			name:          "custom scope on the project",
			projectConfig: map[string]string{shared.HealthScopeKey: "worldwide"},
			want:          "worldwide",
		},
		{
			name: "custom scope on the cli",
			cli:  "worldwide",
			want: "worldwide",
		},
		{
			name:    "custom scope in the compose file",
			compose: "worldwide",
			want:    "worldwide",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := resolveHealthdScope(tt.projectConfig, tt.cli, tt.compose)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestHealthdInstanceAndCertNames(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "blog-ic-healthd", healthdInstanceName("blog", false))
	assert.Equal(t, globalHealthdName, healthdInstanceName("blog", true))

	assert.Equal(t, "ic-healthd-blog", healthdCertName("blog", false))
	assert.Equal(t, "ic-healthd-global", healthdCertName("blog", true))
}
