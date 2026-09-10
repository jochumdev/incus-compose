package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lxc/incus-compose/client"
	"github.com/lxc/incus-compose/project"
	"github.com/lxc/incus-compose/shared"
)

func TestParseDNSNetwork(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		network string
		global  bool
		want    sidecarNetworkRef
		wantErr bool
	}{
		{
			name:    "empty is the project default network",
			network: "",
			want:    sidecarNetworkRef{name: "default", deflt: true},
		},
		{
			name:    "empty is the shared daemon's own bridge",
			network: "",
			global:  true,
			want: sidecarNetworkRef{
				name:      globalDNSNetwork,
				deflt:     true,
				incusName: globalDNSNetwork,
			},
		},
		{
			name:    "an explicit bridge wins for the shared daemon too",
			network: "incusbr0",
			global:  true,
			want:    sidecarNetworkRef{name: "incusbr0"},
		},
		{
			name:    "project:network references a managed network",
			network: "default:default",
			want:    sidecarNetworkRef{project: "default", name: "default"},
		},
		{
			name:    "project:network with distinct names",
			network: "infra:backend",
			want:    sidecarNetworkRef{project: "infra", name: "backend"},
		},
		{
			name:    "no colon is a bridge name",
			network: "incusbr0",
			want:    sidecarNetworkRef{name: "incusbr0"},
		},
		{
			name:    "missing network errors",
			network: "default:",
			wantErr: true,
		},
		{
			name:    "too many colons errors",
			network: "a:b:c",
			wantErr: true,
		},
	}

	c := client.NewOfflineClient(t.Context(), "default")
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := parseDNSNetwork(c, tt.network, tt.global)
			if tt.wantErr {
				require.Error(t, err)
				return
			}

			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestDNSGetResources(t *testing.T) {
	t.Parallel()

	t.Run("project daemon resources", func(t *testing.T) {
		t.Parallel()

		c := client.NewOfflineClient(t.Context(), "shop")
		params := dnsParams{
			image:       "ghcr.io/lxc/incus-compose/ic-dns:latest",
			scope:       "shop",
			ipv4Address: "10.0.0.53",
			ipv6Address: "fd00::53",
			xIncus: map[string]string{
				"limits.cpu": "2",
			},
		}

		inst, resources, err := dnsGetResources(c, params)
		require.NoError(t, err)
		require.NotNil(t, inst)

		require.Len(t, resources, 2)
		assert.Equal(t, client.KindImage, resources[0].Kind())
		assert.Equal(t, client.KindStorageVolume, resources[1].Kind())
		assert.Equal(t, "vol-ic-dns", resources[1].IncusName())

		assert.Equal(t, "shop-ic-dns", inst.Name())
		assert.Equal(t, defaultDNSMemoryLimit, inst.Config.Extensions["limits.memory"])
		assert.Equal(t, "2", inst.Config.Extensions["limits.cpu"])
		assert.Equal(t, "shop", inst.Config.Extensions[shared.DNSDaemonScopeKey])
		assert.Equal(t, "true", inst.Config.Extensions[managedKey])
		assert.Equal(t, client.PriorityInstance-1, inst.Config.Priority)

		var dataDevice *client.InstanceDevice
		for i := range inst.Config.Devices {
			if inst.Config.Devices[i].Name == "data" {
				dataDevice = &inst.Config.Devices[i]
				break
			}
		}
		require.NotNil(t, dataDevice, "data disk device must be configured")
		assert.Equal(t, client.InstanceDeviceTypeDisk, dataDevice.Config.DeviceType)
		assert.Equal(t, "/var/lib/dns-incus", dataDevice.Config.Disk.Path)
		assert.Equal(t, "vol-ic-dns", dataDevice.Config.Disk.Source)
		assert.True(t, dataDevice.Config.Disk.Shift)
	})

	t.Run("global daemon resources", func(t *testing.T) {
		t.Parallel()

		c := client.NewOfflineClient(t.Context(), "incus-compose-system")
		params := dnsParams{
			global: true,
			image:  "ghcr.io/lxc/incus-compose/ic-dns:latest",
			scope:  shared.DNSScopeGlobal,
		}

		inst, resources, err := dnsGetResources(c, params)
		require.NoError(t, err)
		require.NotNil(t, inst)

		require.Len(t, resources, 2)
		assert.Equal(t, "vol-ic-dns", resources[1].IncusName())
		assert.Equal(t, "ic-dns", inst.Name())
		assert.Equal(t, shared.DNSScopeGlobal, inst.Config.Extensions[shared.DNSDaemonScopeKey])
	})
}

func TestDNSCarriedConfig(t *testing.T) {
	t.Parallel()

	input := map[string]string{
		"volatile.idmap.current":              "foo",
		"image.description":                   "bar",
		"oci.cwd":                             "/",
		"user.image_alias":                    "old-alias",
		"environment.INCUS_COMPOSE_DNS_TOKEN": "old-token",
		"environment.DNS_TOKEN":               "old-token-legacy",
		shared.HealthStoppedKey:               "false",
		"environment.INCUS_COMPOSE_DNS_INCUS": "https://10.0.0.1:8443",
		"custom.key":                          "custom-value",
		"limits.memory":                       "32MiB", // below defaultDNSMemoryLimit (64MiB)
	}

	carried := dnsCarriedConfig(input)

	assert.NotContains(t, carried, "volatile.idmap.current")
	assert.NotContains(t, carried, "image.description")
	assert.NotContains(t, carried, "oci.cwd")
	assert.NotContains(t, carried, "user.image_alias")
	assert.NotContains(t, carried, "environment.INCUS_COMPOSE_DNS_TOKEN")
	assert.NotContains(t, carried, "environment.DNS_TOKEN")
	assert.NotContains(t, carried, shared.HealthStoppedKey)
	assert.NotContains(t, carried, "limits.memory", "memory below floor should be cleared")

	assert.Equal(t, "https://10.0.0.1:8443", carried["environment.INCUS_COMPOSE_DNS_INCUS"])
	assert.Equal(t, "custom-value", carried["custom.key"])
}

func TestDNSSettings(t *testing.T) {
	t.Parallel()

	t.Run("global settings", func(t *testing.T) {
		t.Parallel()

		params := dnsParams{
			global:    true,
			listen:    ":5353",
			http:      ":9154",
			forward:   []string{"1.1.1.1", "8.8.8.8"},
			suffix:    "test.incus",
			ttl:       10,
			noMetrics: true,
			carry:     map[string]string{},
		}

		settings := dnsSettings(params, "https://10.0.0.1:8443", true)

		assert.Equal(t, "https://10.0.0.1:8443", settings[envDNSIncus])
		assert.Equal(t, "https://10.0.0.1:8443", settings["environment.DNS_INCUS"])
		assert.Equal(t, ":5353", settings[envDNSListen])
		assert.Equal(t, ":5353", settings["environment.DNS_LISTEN"])
		assert.Equal(t, ":9154", settings[envDNSHTTP])
		assert.Equal(t, ":9154", settings["environment.DNS_HTTP"])
		assert.Equal(t, "1.1.1.1,8.8.8.8", settings[envDNSForward])
		assert.Equal(t, "1.1.1.1,8.8.8.8", settings["environment.DNS_FORWARD"])
		assert.Equal(t, "test.incus", settings[envDNSSuffix])
		assert.Equal(t, "test.incus", settings["environment.DNS_SUFFIX"])
		assert.Equal(t, "10", settings[envDNSTTL])
		assert.Equal(t, "10", settings["environment.DNS_TTL"])
		assert.Equal(t, "false", settings[envDNSMetrics])
		assert.Equal(t, "false", settings["environment.DNS_METRICS"])
		assert.Equal(t, "DEBUG", settings[envDNSLog])
		assert.Equal(t, "DEBUG", settings["environment.DNS_LOG"])
		assert.Equal(t, shared.DNSScopeKey+"="+shared.DNSScopeGlobal, settings[envDNSProjectMarker])
	})

	t.Run("project scope settings", func(t *testing.T) {
		t.Parallel()

		params := dnsParams{
			scope: shared.DNSScopeProject,
			carry: map[string]string{},
		}

		settings := dnsSettings(params, "https://10.0.0.1:8443", false)

		assert.Equal(t, "true", settings[envDNSRestricted])
		assert.Equal(t, "true", settings["environment.DNS_RESTRICTED"])
		assert.Equal(t, ":9153", settings[envDNSHTTP])
	})

	t.Run("multi-project scope settings", func(t *testing.T) {
		t.Parallel()

		params := dnsParams{
			scope: "alpha,beta",
			carry: map[string]string{},
		}

		settings := dnsSettings(params, "https://10.0.0.1:8443", false)

		assert.Equal(t, "alpha,beta", settings[envDNSProjects])
		assert.Equal(t, "alpha,beta", settings["environment.DNS_PROJECTS"])
		assert.Equal(t, "true", settings[envDNSRestricted])
		assert.Equal(t, "true", settings["environment.DNS_RESTRICTED"])
	})
}

func TestResolveDNSScopePrecedence(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		projectConfig map[string]string
		cliScope      string
		composeScope  string
		want          string
	}{
		{
			name: "project config wins over all",
			projectConfig: map[string]string{
				shared.DNSScopeKey: "project-conf-scope",
			},
			cliScope:     "cli-scope",
			composeScope: "compose-scope",
			want:         "project-conf-scope",
		},
		{
			name: "cli flag wins over compose and default",
			projectConfig: map[string]string{
				"other.key": "foo",
			},
			cliScope:     "cli-scope",
			composeScope: "compose-scope",
			want:         "cli-scope",
		},
		{
			name:          "compose file wins over default",
			projectConfig: map[string]string{},
			cliScope:      "",
			composeScope:  "compose-scope",
			want:          "compose-scope",
		},
		{
			name:          "default global when all empty",
			projectConfig: map[string]string{},
			cliScope:      "",
			composeScope:  "",
			want:          shared.DNSScopeGlobal,
		},
		{
			name:          "nil project config falls back to cli",
			projectConfig: nil,
			cliScope:      "cli-scope",
			composeScope:  "compose-scope",
			want:          "cli-scope",
		},
		{
			name:          "nil project config falls back to compose",
			projectConfig: nil,
			cliScope:      "",
			composeScope:  "compose-scope",
			want:          "compose-scope",
		},
		{
			name:          "nil project config falls back to default",
			projectConfig: nil,
			cliScope:      "",
			composeScope:  "",
			want:          shared.DNSScopeGlobal,
		},
		{
			name: "project config wins over compose when cli is empty",
			projectConfig: map[string]string{
				shared.DNSScopeKey: shared.DNSScopeProject,
			},
			cliScope:     "",
			composeScope: "alpha,beta",
			want:         shared.DNSScopeProject,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := resolveDNSScope(tt.projectConfig, tt.cliScope, tt.composeScope)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestUpCommandDNSFlags(t *testing.T) {
	t.Parallel()

	cmd := newUpCommand()
	require.NotNil(t, cmd)

	var hasDisableDNS, hasDNSImage bool
	for _, f := range cmd.Flags {
		for _, name := range f.Names() {
			switch name {
			case "disable-dns":
				hasDisableDNS = true
			case "dns-image":
				hasDNSImage = true
			}
		}
	}

	assert.True(t, hasDisableDNS, "up command should have --disable-dns flag")
	assert.True(t, hasDNSImage, "up command should have --dns-image flag")
}

func TestDNSInstanceMarksWithZone(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	composeYAML := `name: shop
services:
  web:
    image: docker.io/alpine:edge
`
	err := os.WriteFile(filepath.Join(dir, "compose.yaml"), []byte(composeYAML), 0o600)
	require.NoError(t, err)

	p, err := project.New().Load(t.Context(), project.LoadFiles([]string{filepath.Join(dir, "compose.yaml")}))
	require.NoError(t, err)

	p.InstanceMarks = map[string]string{
		"oci.dns.nameservers": "10.0.0.53",
		"oci.dns.search":      "shop.incus",
	}

	c := client.NewOfflineClient(t.Context(), "shop")
	res, err := p.Resources(c)
	require.NoError(t, err)

	instances := res["web"]
	require.NotEmpty(t, instances)

	inst, ok := instances[0].(*client.Instance)
	require.True(t, ok)

	assert.Equal(t, "10.0.0.53", inst.Config.Extensions["oci.dns.nameservers"])
	assert.Equal(t, "shop.incus", inst.Config.Extensions["oci.dns.search"])
}
