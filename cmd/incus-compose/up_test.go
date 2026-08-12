package main

import (
	"bytes"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/urfave/cli/v3"

	"github.com/lxc/incus-compose/cmd/incus-compose/version"
	"github.com/lxc/incus-compose/project"
	"github.com/lxc/incus-compose/testlib"
)

func TestVersionCommand(t *testing.T) {
	// Not parallel: mutates the package-global version.Version.
	oldVersion := version.Version
	version.Version = "v1.2.3"
	defer func() { version.Version = oldVersion }()

	out := &bytes.Buffer{}
	require.NoError(t, newVersionCommand().Action(t.Context(), &cli.Command{Writer: out}))
	assert.Equal(t, "incus-compose version v1.2.3\n", out.String())
}

func TestResolveHealthdImage(t *testing.T) {
	// Not parallel: mutates the package-global version.Version.
	oldVersion := version.Version
	version.Version = "v1.2.3"
	defer func() { version.Version = oldVersion }()

	assert.Equal(t,
		"ghcr.io/lxc/incus-compose/ic-healthd:1.2.3",
		resolveHealthdImage("ghcr.io/lxc/incus-compose/ic-healthd:{version}"),
	)
	assert.Equal(t, "custom:latest", resolveHealthdImage("custom:latest"))
}

func TestBuiltServices(t *testing.T) {
	t.Parallel()

	dir := testlib.WriteTempFiles(t, map[string]string{
		"Dockerfile": "FROM docker.io/alpine:latest\n",
		"compose.yaml": `name: built
services:
  app:
    image: localhost/app:latest
    build:
      context: .
  consumer:
    image: localhost/app:latest
  plain:
    image: docker.io/alpine:edge
  dependent:
    image: docker.io/alpine:edge
    depends_on:
      - app
`})

	p, err := project.New().Load(t.Context(), project.LoadFiles([]string{filepath.Join(dir, "compose.yaml")}))
	require.NoError(t, err)

	tests := []struct {
		name string
		args filterResourcesArgs
		want []string
	}{
		{name: "whole project", args: filterResourcesArgs{}, want: []string{"app", "consumer"}},
		{name: "the builder", args: filterResourcesArgs{OnlyServices: []string{"app"}}, want: []string{"app"}},
		{name: "a consumer of the built image", args: filterResourcesArgs{OnlyServices: []string{"consumer"}}, want: []string{"consumer"}},
		{name: "nothing built in scope", args: filterResourcesArgs{OnlyServices: []string{"plain"}}, want: []string{}},
		{
			name: "a dependency is in scope",
			args: filterResourcesArgs{OnlyServices: []string{"dependent"}, WithDependencies: true},
			want: []string{"app"},
		},
		{name: "no-deps drops it again", args: filterResourcesArgs{OnlyServices: []string{"dependent"}}, want: []string{}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, builtServices(p, tt.args))
		})
	}
}

func TestParseScale(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		values []string
		want   map[string]int
	}{
		{name: "empty", values: nil, want: map[string]int{}},
		{name: "single", values: []string{"web=3"}, want: map[string]int{"web": 3}},
		{name: "multiple", values: []string{"web=3", "api=2"}, want: map[string]int{"web": 3, "api": 2}},
		{name: "invalid ignored", values: []string{"web", "api=bad", "db=1"}, want: map[string]int{"db": 1}},
		{name: "last wins", values: []string{"web=2", "web=4"}, want: map[string]int{"web": 4}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, parseScale(tt.values))
		})
	}
}
