package main

import (
	"bytes"
	"context"
	"path/filepath"
	"testing"

	incusApi "github.com/lxc/incus/v7/shared/api"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/urfave/cli/v3"

	"github.com/lxc/incus-compose/cmd/incus-compose/version"
	"github.com/lxc/incus-compose/internal/testlib"
	"github.com/lxc/incus-compose/project"
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
		resolveImageVersion("ghcr.io/lxc/incus-compose/ic-healthd:{version}"),
	)
	assert.Equal(t, "custom:latest", resolveImageVersion("custom:latest"))
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

// TestPulledImageChanged covers what up feeds the recreate teardown: an
// unensured image never recreates, a changed fingerprint does.
func TestPulledImageChanged(t *testing.T) {
	t.Parallel()

	imgAlias := func(target string) *incusApi.ImageAliasesEntry {
		return &incusApi.ImageAliasesEntry{ImageAliasesEntryPut: incusApi.ImageAliasesEntryPut{Target: target}}
	}

	tests := []struct {
		name      string
		baseImage string
		alias     *incusApi.ImageAliasesEntry
		want      bool
	}{
		{
			name:      "an unchanged fingerprint does not recreate",
			baseImage: "same-fingerprint",
			alias:     imgAlias("same-fingerprint"),
			want:      false,
		},
		{
			name:      "a changed fingerprint recreates",
			baseImage: "old-fingerprint",
			alias:     imgAlias("new-fingerprint"),
			want:      true,
		},
		{
			name:      "no ensured alias never recreates",
			baseImage: "old-fingerprint",
			alias:     nil,
			want:      false,
		},
		{
			name:      "an empty alias target never recreates",
			baseImage: "old-fingerprint",
			alias:     imgAlias(""),
			want:      false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, pulledImageChanged(tt.baseImage, tt.alias))
		})
	}
}

func TestUpCommand_NetworkDriverFlag(t *testing.T) {
	t.Parallel()

	cmd := newUpCommand()
	var netDriverFlag *cli.StringFlag
	for _, f := range cmd.Flags {
		sf, ok := f.(*cli.StringFlag)
		if ok && sf.Name == "network-driver" {
			netDriverFlag = sf
			break
		}
	}

	require.NotNil(t, netDriverFlag, "network-driver flag must be defined on up command")
	assert.Equal(t, []string{"INCUS_COMPOSE_NETWORK_DRIVER"}, netDriverFlag.Sources.EnvKeys())

	// Test validator
	require.NotNil(t, netDriverFlag.Validator)
	assert.NoError(t, netDriverFlag.Validator(""))
	assert.NoError(t, netDriverFlag.Validator("auto"))
	assert.NoError(t, netDriverFlag.Validator("ovn"))
	assert.NoError(t, netDriverFlag.Validator("bridge"))
	assert.Error(t, netDriverFlag.Validator("invalid"))
	assert.Error(t, netDriverFlag.Validator("macvlan"))
}

func TestUpCommand_NetworkUplinkFlag(t *testing.T) {
	t.Parallel()

	cmd := newUpCommand()
	var netUplinkFlag *cli.StringFlag
	for _, f := range cmd.Flags {
		sf, ok := f.(*cli.StringFlag)
		if ok && sf.Name == "network-uplink" {
			netUplinkFlag = sf
			break
		}
	}

	require.NotNil(t, netUplinkFlag, "network-uplink flag must be defined on up command")
	assert.Equal(t, []string{"INCUS_COMPOSE_NETWORK_UPLINK"}, netUplinkFlag.Sources.EnvKeys())
}

func TestBuildLoadOptions_NetworkOptions(t *testing.T) {
	t.Parallel()

	cmd := newUpCommand()
	var capturedOpts []project.LoadOption
	cmd.Action = func(ctx context.Context, c *cli.Command) error {
		capturedOpts = buildLoadOptions(c)
		return nil
	}

	err := cmd.Run(t.Context(), []string{"up", "--network-driver=ovn", "--network-uplink=incusbr0"})
	require.NoError(t, err)

	loadOpts := project.NewLoadOptions(capturedOpts...)
	assert.Equal(t, "ovn", loadOpts.NetworkDriver)
	assert.Equal(t, "incusbr0", loadOpts.NetworkUplink)
}
