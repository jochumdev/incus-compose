package project

import (
	"testing"

	"github.com/compose-spec/compose-go/v2/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func hashTestService() types.ServiceConfig {
	a, b := "1", "2"

	return types.ServiceConfig{
		Name:        "web",
		Image:       "docker.io/nginx:alpine",
		Command:     types.ShellCommand{"nginx", "-g", "daemon off;"},
		Environment: types.MappingWithEquals{"A": &a, "B": &b},
		Extensions:  map[string]any{"x-incus": map[string]any{"limits.cpu": "2"}},
	}
}

func mustHash(t *testing.T, s types.ServiceConfig) string {
	t.Helper()

	hash, err := ServiceConfigHash(s)
	require.NoError(t, err)

	return hash
}

func TestServiceConfigHash(t *testing.T) {
	t.Parallel()

	base := mustHash(t, hashTestService())
	assert.Regexp(t, `^v1:[0-9a-f]{64}$`, base)

	t.Run("stable", func(t *testing.T) {
		t.Parallel()
		assert.Equal(t, base, mustHash(t, hashTestService()))
	})

	// A compose-go or yaml bump that changes the canonical form would mark
	// every instance changed; this catches it before users do. Bump "v1:" in
	// ServiceConfigHash rather than just updating the literal.
	t.Run("pinned", func(t *testing.T) {
		t.Parallel()
		assert.Equal(t, "v1:5d09a098a9646658760f53330ff45191e7b96570bfa513c7388a3017ba865712", base)
	})

	t.Run("changes", func(t *testing.T) {
		t.Parallel()

		tests := map[string]func(*types.ServiceConfig){
			"env value": func(s *types.ServiceConfig) { v := "x"; s.Environment["A"] = &v },
			"image":     func(s *types.ServiceConfig) { s.Image = "docker.io/nginx:latest" },
			"command":   func(s *types.ServiceConfig) { s.Command = types.ShellCommand{"sh"} },
			"x-incus": func(s *types.ServiceConfig) {
				s.Extensions = map[string]any{"x-incus": map[string]any{"limits.cpu": "4"}}
			},
			"deploy.mode": func(s *types.ServiceConfig) {
				s.Deploy = &types.DeployConfig{Mode: "global"}
			},
			"deploy.placement": func(s *types.ServiceConfig) {
				s.Deploy = &types.DeployConfig{Placement: types.Placement{Constraints: []string{"node.role == worker"}}}
			},
			"deploy.restart_policy": func(s *types.ServiceConfig) {
				s.Deploy = &types.DeployConfig{RestartPolicy: &types.RestartPolicy{Condition: "on-failure"}}
			},
		}

		for name, mutate := range tests {
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				s := hashTestService()
				mutate(&s)
				assert.NotEqual(t, base, mustHash(t, s))
			})
		}
	})

	t.Run("ignored", func(t *testing.T) {
		t.Parallel()

		replicas, scale := 2, 3

		tests := map[string]func(*types.ServiceConfig){
			"deploy.replicas": func(s *types.ServiceConfig) { s.Deploy = &types.DeployConfig{Replicas: &replicas} },
			"deploy.update_config": func(s *types.ServiceConfig) {
				s.Deploy = &types.DeployConfig{UpdateConfig: &types.UpdateConfig{Order: "start-first"}}
			},
			"scale": func(s *types.ServiceConfig) { s.Scale = &scale },
			"depends_on": func(s *types.ServiceConfig) {
				s.DependsOn = types.DependsOnConfig{"db": {Condition: "service_started"}}
			},
			"pull_policy": func(s *types.ServiceConfig) { s.PullPolicy = "always" },
			"build":       func(s *types.ServiceConfig) { s.Build = &types.BuildConfig{Context: "."} },
			"profiles":    func(s *types.ServiceConfig) { s.Profiles = []string{"dev"} },
		}

		for name, mutate := range tests {
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				s := hashTestService()
				mutate(&s)
				assert.Equal(t, base, mustHash(t, s))
			})
		}
	})

	t.Run("does not touch the caller's deploy", func(t *testing.T) {
		t.Parallel()

		replicas := 2
		s := hashTestService()
		s.Deploy = &types.DeployConfig{Replicas: &replicas}
		mustHash(t, s)
		require.NotNil(t, s.Deploy.Replicas)
	})
}
