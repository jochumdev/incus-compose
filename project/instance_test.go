package project

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/compose-spec/compose-go/v2/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lxc/incus-compose/client"
	"github.com/lxc/incus-compose/shared"
	"github.com/lxc/incus-compose/testlib"
)

func TestFormatMemoryLimit(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		bytes int64
		want  string
	}{
		{name: "gib", bytes: 2 << 30, want: "2GiB"},
		{name: "mib", bytes: 512 << 20, want: "512MiB"},
		{name: "kib", bytes: 64 << 10, want: "64KiB"},
		{name: "bytes", bytes: 1537, want: "1537B"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, formatMemoryLimit(tt.bytes))
		})
	}
}

func TestApplyRestartPolicy(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		policy string
		want   map[string]string
	}{
		{name: "always", policy: "always", want: map[string]string{"boot.autostart": "true"}},
		{name: "on failure", policy: "on-failure", want: map[string]string{"boot.autostart": "true", "boot.autorestart": "true"}},
		{name: "unless stopped", policy: "unless-stopped", want: map[string]string{}},
		{name: "no", policy: "no", want: map[string]string{"boot.autostart": "false"}},
		{name: "default", policy: "", want: map[string]string{"boot.autostart": "false"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			config := map[string]string{}
			applyRestartPolicy(config, tt.policy)
			assert.Equal(t, tt.want, config)
		})
	}
}

func TestFormatTmpfsSize(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "", formatTmpfsSize(nil))
	assert.Equal(t, "", formatTmpfsSize(&types.ServiceVolumeTmpfs{}))
	assert.Equal(t, "4096", formatTmpfsSize(&types.ServiceVolumeTmpfs{Size: 4096}))
}

func TestParseSecretOwnershipAndMode(t *testing.T) {
	t.Parallel()

	mode := types.FileMode(0o640)

	// Empty maps to -1 so InstanceFile falls back to the image's oci.uid/oci.gid.
	assert.Equal(t, int64(-1), parseSecretID(""))
	assert.Equal(t, int64(1000), parseSecretID("1000"))
	assert.Equal(t, int64(0), parseSecretID("not-a-number"))
	assert.Equal(t, 0o400, parseSecretMode(nil))
	assert.Equal(t, 0o440, parseSecretMode(&mode))
}

func TestCheckEntrypoint(t *testing.T) {
	t.Parallel()

	// Both explicitly empty leaves nothing to exec.
	err := checkEntrypoint(types.ServiceConfig{
		Name:       "web",
		Entrypoint: types.ShellCommand{},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "web")

	// An explicitly empty entrypoint is fine as long as a command remains.
	require.NoError(t, checkEntrypoint(types.ServiceConfig{
		Name:       "web",
		Entrypoint: types.ShellCommand{},
		Command:    types.ShellCommand{"httpd"},
	}))

	// An unset entrypoint is never an error, with or without a command.
	require.NoError(t, checkEntrypoint(types.ServiceConfig{Name: "web"}))
	require.NoError(t, checkEntrypoint(types.ServiceConfig{
		Name:    "web",
		Command: types.ShellCommand{"httpd"},
	}))
}

func TestNetworkExtensionsExtractsXIncus(t *testing.T) {
	t.Parallel()

	proj, err := New().Load(t.Context(), LoadWorkingDir(fixturePath("with-network-ranges")))
	require.NoError(t, err)

	assert.Equal(t, map[string]string{
		"ipv4.address":     "10.200.0.1/24",
		"ipv4.dhcp.ranges": "10.200.0.100-10.200.0.200",
		"ipv6.address":     "fd42:1::1/64",
	}, networkExtensions(proj.Networks["backend"]))
	// Never nil: callers write defaults such as ipv4.nat into it.
	assert.Equal(t, map[string]string{}, networkExtensions(types.NetworkConfig{}))
}

func TestServiceXIncusExtensionsExtractsXIncus(t *testing.T) {
	t.Parallel()

	proj, err := New().Load(t.Context(), LoadWorkingDir(fixturePath("with-incus-options")))
	require.NoError(t, err)

	assert.Equal(t, map[string]string{
		"limits.memory":       "1024MB",
		"limits.cpu":          "2",
		"security.nesting":    "false",
		"oci.dns.nameservers": "9.9.9.9",
	}, serviceXIncusExtensions(proj.Services["web"]))
	assert.Nil(t, serviceXIncusExtensions(types.ServiceConfig{}))
}

func TestInstanceName(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		service types.ServiceConfig
		index   int
		scale   int
		want    string
	}{
		{name: "default", service: types.ServiceConfig{Name: "web"}, index: 1, scale: 1, want: "web-1"},
		{name: "container name single", service: types.ServiceConfig{Name: "web", ContainerName: "mydb"}, index: 1, scale: 1, want: "mydb"},
		{name: "container name scaled", service: types.ServiceConfig{Name: "web", ContainerName: "mydb"}, index: 2, scale: 3, want: "mydb-2"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, instanceName(tt.service, tt.index, tt.scale))
		})
	}
}

func TestInstanceConfig(t *testing.T) {
	t.Parallel()
	testlib.SkipLocal(t)

	gc, err := client.NewTestClient(t.Context())
	require.NoError(t, err)
	c, err := gc.EnsureProject("default")
	require.NoError(t, err)

	val := "bar"
	retries := uint64(3)
	service := types.ServiceConfig{
		Name:        "web",
		Environment: types.MappingWithEquals{"FOO": &val, "SKIP": nil},
		Labels:      types.Labels{"com.example": "x"},
		Command:     types.ShellCommand{"/bin/sh", "-c", "echo hi"},
		Restart:     "always",
		DNS:         types.StringList{"8.8.8.8", "1.1.1.1"},
		DNSSearch:   types.StringList{"example.com"},
		DomainName:  "example.com",
		Deploy: &types.DeployConfig{
			Resources: types.Resources{
				Limits: &types.Resource{NanoCPUs: 2, MemoryBytes: 512 << 20},
			},
		},
		HealthCheck: &types.HealthCheckConfig{
			Test:    types.HealthCheckTest{"CMD", "curl", "-f", "http://localhost"},
			Retries: &retries,
		},
	}

	config, err := instanceConfig(c, service, "", nil)
	require.NoError(t, err)

	assert.Equal(t, "bar", config["environment.FOO"])
	assert.NotContains(t, config, "environment.SKIP")
	assert.Equal(t, "x", config["user.label.com.example"])
	// The entrypoint is assembled in the client layer (image entrypoint +
	// AppendEntrypoint), so instanceConfig no longer emits oci.entrypoint.
	assert.NotContains(t, config, "oci.entrypoint")
	assert.Equal(t, "true", config["boot.autostart"])
	assert.Equal(t, "always", config[client.HealthKeyPrefix+"restart"])
	assert.Equal(t, "200ms/100ms", config["limits.cpu.allowance"])
	assert.Equal(t, "512MiB", config["limits.memory"])
	assert.NotContains(t, config, client.HealthStatusKey,
		"ic-healthd is the only writer of the status; a value here is one it has to correct")
	assert.Equal(t, `["CMD","curl","-f","http://localhost"]`, config[client.HealthKeyPrefix+"test"])
	assert.Equal(t, "3", config[client.HealthKeyPrefix+"retries"])
	assert.Equal(t, "8.8.8.8,1.1.1.1", config["oci.dns.nameservers"])
	assert.Equal(t, "example.com", config["oci.dns.search"])
	assert.Equal(t, "example.com", config["oci.dns.domain"])
}

func TestInstanceConfigResourceLimits(t *testing.T) {
	t.Parallel()

	gc, err := client.NewTestClient(t.Context())
	require.NoError(t, err)
	c, err := gc.EnsureProject("default")
	require.NoError(t, err)

	tests := []struct {
		name       string
		cpus       float32
		memLimit   types.UnitBytes
		limits     *types.Resource
		xIncus     map[string]any
		want       map[string]string
		notPresent []string
	}{
		{
			name:   "integer cpu and memory",
			limits: &types.Resource{NanoCPUs: 2, MemoryBytes: 512 << 20},
			want: map[string]string{
				"limits.cpu.allowance": "200ms/100ms",
				"limits.memory":        "512MiB",
			},
			// limits.cpu pins a cpuset rather than capping usage, so compose
			// cpus must never land there.
			notPresent: []string{"limits.cpu"},
		},
		{
			name:   "fractional cpu",
			limits: &types.Resource{NanoCPUs: 0.5},
			want: map[string]string{
				"limits.cpu.allowance": "50ms/100ms",
			},
			notPresent: []string{"limits.cpu", "limits.memory"},
		},
		{
			name:   "sub-millisecond cpu keeps a non-zero quota",
			limits: &types.Resource{NanoCPUs: 0.001},
			want: map[string]string{
				"limits.cpu.allowance": "1ms/100ms",
			},
			notPresent: []string{"limits.cpu"},
		},
		{
			name:     "service-level cpus and mem_limit",
			cpus:     2,
			memLimit: 512 << 20,
			want: map[string]string{
				"limits.cpu.allowance": "200ms/100ms",
				"limits.memory":        "512MiB",
			},
			notPresent: []string{"limits.cpu"},
		},
		{
			name: "service-level fractional cpus",
			cpus: 0.5,
			want: map[string]string{
				"limits.cpu.allowance": "50ms/100ms",
			},
			notPresent: []string{"limits.cpu", "limits.memory"},
		},
		{
			name:     "deploy limits win over service-level",
			cpus:     1,
			memLimit: 256 << 20,
			limits:   &types.Resource{NanoCPUs: 4, MemoryBytes: 1 << 30},
			want: map[string]string{
				"limits.cpu.allowance": "400ms/100ms",
				"limits.memory":        "1GiB",
			},
			notPresent: []string{"limits.cpu"},
		},
		{
			name:   "service-level cpus fills what deploy leaves out",
			cpus:   2,
			limits: &types.Resource{MemoryBytes: 512 << 20},
			want: map[string]string{
				"limits.cpu.allowance": "200ms/100ms",
				"limits.memory":        "512MiB",
			},
			notPresent: []string{"limits.cpu"},
		},
		{
			name:     "service-level mem_limit fills what deploy leaves out",
			memLimit: 512 << 20,
			limits:   &types.Resource{NanoCPUs: 2},
			want: map[string]string{
				"limits.cpu.allowance": "200ms/100ms",
				"limits.memory":        "512MiB",
			},
			notPresent: []string{"limits.cpu"},
		},
		{
			name:   "deploy fractional cpus replaces a service-level value",
			cpus:   4,
			limits: &types.Resource{NanoCPUs: 0.25},
			want: map[string]string{
				"limits.cpu.allowance": "25ms/100ms",
			},
			notPresent: []string{"limits.cpu", "limits.memory"},
		},
		{
			name:     "x-incus overrides service-level cpus and mem_limit",
			cpus:     2,
			memLimit: 512 << 20,
			xIncus: map[string]any{
				"limits.cpu.allowance": "400ms/100ms",
				"limits.memory":        "1GiB",
			},
			want: map[string]string{
				"limits.cpu.allowance": "400ms/100ms",
				"limits.memory":        "1GiB",
			},
			notPresent: []string{"limits.cpu"},
		},
		{
			name:   "x-incus overrides the allowance and memory",
			limits: &types.Resource{NanoCPUs: 2, MemoryBytes: 512 << 20},
			xIncus: map[string]any{
				"limits.cpu.allowance": "25%",
				"limits.memory":        "1GiB",
			},
			want: map[string]string{
				"limits.cpu.allowance": "25%",
				"limits.memory":        "1GiB",
			},
			notPresent: []string{"limits.cpu"},
		},
		{
			// x-incus is raw passthrough, so pinning stays available to anyone
			// who wants it alongside the allowance we derive.
			name:   "x-incus can still pin a cpuset",
			limits: &types.Resource{NanoCPUs: 2},
			xIncus: map[string]any{
				"limits.cpu": "0-3",
			},
			want: map[string]string{
				"limits.cpu":           "0-3",
				"limits.cpu.allowance": "200ms/100ms",
			},
			notPresent: []string{"limits.memory"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			service := types.ServiceConfig{
				Name:     "web",
				CPUS:     tt.cpus,
				MemLimit: tt.memLimit,
			}
			if tt.limits != nil {
				service.Deploy = &types.DeployConfig{
					Resources: types.Resources{Limits: tt.limits},
				}
			}

			if tt.xIncus != nil {
				service.Extensions = types.Extensions{"x-incus": tt.xIncus}
			}

			config, err := instanceConfig(c, service, "test", nil)
			require.NoError(t, err)

			for key, value := range tt.want {
				assert.Equal(t, value, config[key])
			}
			for _, key := range tt.notPresent {
				assert.NotContains(t, config, key)
			}
		})
	}
}

func TestInstanceConfigSysctls(t *testing.T) {
	t.Parallel()

	gc, err := client.NewTestClient(t.Context())
	require.NoError(t, err)
	c, err := gc.EnsureProject("default")
	require.NoError(t, err)

	service := types.ServiceConfig{
		Name: "web",
		Sysctls: types.Mapping{
			"net.ipv4.conf.all.src_valid_mark": "1",
			"net.ipv6.conf.all.disable_ipv6":   "0",
		},
	}

	config, err := instanceConfig(c, service, "test", nil)
	require.NoError(t, err)

	assert.Equal(t, "1", config["linux.sysctl.net.ipv4.conf.all.src_valid_mark"])
	assert.Equal(t, "0", config["linux.sysctl.net.ipv6.conf.all.disable_ipv6"])
}

func TestInstanceConfigMinimal(t *testing.T) {
	t.Parallel()
	testlib.SkipLocal(t)

	gc, err := client.NewTestClient(t.Context())
	require.NoError(t, err)
	c, err := gc.EnsureProject("default")
	require.NoError(t, err)

	config, err := instanceConfig(c, types.ServiceConfig{Name: "web"}, "project1", nil)
	require.NoError(t, err)
	// Only the default restart policy is applied.
	assert.Equal(t, map[string]string{
		"boot.autostart":                   "false",
		"raw.lxc":                          "lxc.start.delay = 1\n",
		"user.label.incus-compose.project": "project1",
		"user.label.incus-compose.service": "web",
	}, config)
}

func TestInstanceConfigXIncusOverrides(t *testing.T) {
	t.Parallel()
	testlib.SkipLocal(t)

	gc, err := client.NewTestClient(t.Context())
	require.NoError(t, err)
	c, err := gc.EnsureProject("default")
	require.NoError(t, err)

	proj, err := New().Load(t.Context(), LoadWorkingDir(fixturePath("with-incus-options")))
	require.NoError(t, err)

	config, err := instanceConfig(c, proj.Services["web"], "", nil)
	require.NoError(t, err)
	assert.Equal(t, "1024MB", config["limits.memory"])
	assert.Equal(t, "2", config["limits.cpu"])
	assert.Equal(t, "false", config["security.nesting"])
	// x-incus wins over the compose `dns:` field for the same Incus key.
	assert.Equal(t, "9.9.9.9", config["oci.dns.nameservers"])
}

func TestInstanceDependencyWaits(t *testing.T) {
	t.Parallel()

	t.Run("scale from options", func(t *testing.T) {
		t.Parallel()
		p := &types.Project{Services: types.Services{"db": {Name: "db"}}}
		service := types.ServiceConfig{Name: "web", DependsOn: types.DependsOnConfig{
			"db": {Condition: types.ServiceConditionHealthy},
		}}
		deps := instanceDependencyWaits(p, service, &ResourcesOptions{Scale: map[string]int{"db": 2}})
		assert.Equal(t, map[string]string{
			"db-1": client.HealthStatusHealthy,
			"db-2": client.HealthStatusHealthy,
		}, deps)
	})

	t.Run("scale from replicas", func(t *testing.T) {
		t.Parallel()
		reps := 3
		p := &types.Project{Services: types.Services{"db": {Name: "db", Deploy: &types.DeployConfig{Replicas: &reps}}}}
		service := types.ServiceConfig{Name: "web", DependsOn: types.DependsOnConfig{
			"db": {Condition: types.ServiceConditionHealthy},
		}}
		deps := instanceDependencyWaits(p, service, &ResourcesOptions{})
		assert.Equal(t, map[string]string{
			"db-1": client.HealthStatusHealthy,
			"db-2": client.HealthStatusHealthy,
			"db-3": client.HealthStatusHealthy,
		}, deps)
	})

	t.Run("container name", func(t *testing.T) {
		t.Parallel()
		p := &types.Project{Services: types.Services{"db": {Name: "db", ContainerName: "mydb"}}}
		service := types.ServiceConfig{Name: "web", DependsOn: types.DependsOnConfig{
			"db": {Condition: types.ServiceConditionHealthy},
		}}
		deps := instanceDependencyWaits(p, service, &ResourcesOptions{})
		assert.Equal(t, map[string]string{"mydb": client.HealthStatusHealthy}, deps)
	})

	t.Run("non-healthy condition skipped", func(t *testing.T) {
		t.Parallel()
		p := &types.Project{Services: types.Services{"db": {Name: "db"}}}
		service := types.ServiceConfig{Name: "web", DependsOn: types.DependsOnConfig{
			"db": {Condition: types.ServiceConditionStarted},
		}}
		assert.Empty(t, instanceDependencyWaits(p, service, &ResourcesOptions{}))
	})

	t.Run("no dependencies", func(t *testing.T) {
		t.Parallel()
		assert.Empty(t, instanceDependencyWaits(&types.Project{}, types.ServiceConfig{Name: "web"}, &ResourcesOptions{}))
	})
}

func TestInstanceSecrets(t *testing.T) {
	// Not parallel: the "environment source" subtest uses t.Setenv, which panics
	// under a parallel ancestor.

	t.Run("file source", func(t *testing.T) {
		t.Parallel()
		path := filepath.Join(t.TempDir(), "secret.txt")
		require.NoError(t, os.WriteFile(path, []byte("s3cr3t"), 0o600))
		p := &types.Project{Secrets: types.Secrets{"db_pw": {File: path}}}
		service := types.ServiceConfig{Name: "web", Secrets: []types.ServiceSecretConfig{{Source: "db_pw", Target: "db_pw"}}}

		secrets, err := instanceSecrets(p, service)
		require.NoError(t, err)
		require.Len(t, secrets, 1)
		require.Equal(t, "db_pw", secrets[0].Target)
		// Unset UID/GID fall back to the image's oci.uid/oci.gid.
		assert.Equal(t, int64(-1), secrets[0].UID)
		assert.Equal(t, int64(-1), secrets[0].GID)
	})

	t.Run("environment source", func(t *testing.T) {
		// Not parallel: uses t.Setenv.
		p := &types.Project{Secrets: types.Secrets{"s": {Environment: "MY_SECRET"}}, Environment: map[string]string{"MY_SECRET": "envval"}}
		service := types.ServiceConfig{Name: "web", Secrets: []types.ServiceSecretConfig{{Source: "s", Target: "s"}}}

		secrets, err := instanceSecrets(p, service)
		require.NoError(t, err)
		require.Len(t, secrets, 1)
		require.Equal(t, "s", secrets[0].Target)
	})

	t.Run("undefined secret", func(t *testing.T) {
		t.Parallel()
		service := types.ServiceConfig{Name: "web", Secrets: []types.ServiceSecretConfig{{Source: "missing"}}}
		_, err := instanceSecrets(&types.Project{}, service)
		require.Error(t, err)
	})

	t.Run("no source", func(t *testing.T) {
		t.Parallel()
		p := &types.Project{Secrets: types.Secrets{"s": {}}}
		service := types.ServiceConfig{Name: "web", Secrets: []types.ServiceSecretConfig{{Source: "s"}}}
		_, err := instanceSecrets(p, service)
		require.Error(t, err)
	})

	t.Run("missing file", func(t *testing.T) {
		t.Parallel()
		p := &types.Project{Secrets: types.Secrets{"s": {File: filepath.Join(t.TempDir(), "nope")}}}
		service := types.ServiceConfig{Name: "web", Secrets: []types.ServiceSecretConfig{{Source: "s"}}}
		_, err := instanceSecrets(p, service)
		require.Error(t, err)
	})
}

func TestInstanceConfigs(t *testing.T) {
	t.Run("file source", func(t *testing.T) {
		t.Parallel()
		path := filepath.Join(t.TempDir(), "config.txt")
		require.NoError(t, os.WriteFile(path, []byte("config-value"), 0o644))
		p := &types.Project{Configs: types.Configs{"app": {File: path}}}
		service := types.ServiceConfig{Name: "web", Configs: []types.ServiceConfigObjConfig{{Source: "app", Target: "/etc/app.conf"}}}

		configs, err := instanceConfigs(p, service)
		require.NoError(t, err)
		require.Len(t, configs, 1)
		require.Equal(t, "/etc/app.conf", configs[0].Target)
		assert.Equal(t, int64(-1), configs[0].UID)
		assert.Equal(t, int64(-1), configs[0].GID)
	})

	t.Run("content source", func(t *testing.T) {
		t.Parallel()
		p := &types.Project{Configs: types.Configs{"app": {Content: "inline-config"}}}
		service := types.ServiceConfig{Name: "web", Configs: []types.ServiceConfigObjConfig{{Source: "app", Target: "/etc/app.conf"}}}

		configs, err := instanceConfigs(p, service)
		require.NoError(t, err)
		require.Len(t, configs, 1)
		require.Equal(t, "/etc/app.conf", configs[0].Target)
	})

	t.Run("environment source", func(t *testing.T) {
		p := &types.Project{Configs: types.Configs{"app": {Environment: "MY_CONFIG", Content: "env-value"}}}
		service := types.ServiceConfig{Name: "web", Configs: []types.ServiceConfigObjConfig{{Source: "app", Target: "/etc/app.conf"}}}

		configs, err := instanceConfigs(p, service)
		require.NoError(t, err)
		require.Len(t, configs, 1)
		require.Equal(t, "/etc/app.conf", configs[0].Target)
	})

	t.Run("environment not found", func(t *testing.T) {
		p := &types.Project{Configs: types.Configs{"app": {Environment: "MISSING"}}}
		service := types.ServiceConfig{Name: "web", Configs: []types.ServiceConfigObjConfig{{Source: "app"}}}
		_, err := instanceConfigs(p, service)
		require.Error(t, err)
	})

	t.Run("undefined config", func(t *testing.T) {
		t.Parallel()
		service := types.ServiceConfig{Name: "web", Configs: []types.ServiceConfigObjConfig{{Source: "missing"}}}
		_, err := instanceConfigs(&types.Project{}, service)
		require.Error(t, err)
	})

	t.Run("no source", func(t *testing.T) {
		t.Parallel()
		p := &types.Project{Configs: types.Configs{"app": {}}}
		service := types.ServiceConfig{Name: "web", Configs: []types.ServiceConfigObjConfig{{Source: "app"}}}
		_, err := instanceConfigs(p, service)
		require.Error(t, err)
	})

	t.Run("missing file", func(t *testing.T) {
		t.Parallel()
		p := &types.Project{Configs: types.Configs{"app": {File: filepath.Join(t.TempDir(), "nope")}}}
		service := types.ServiceConfig{Name: "web", Configs: []types.ServiceConfigObjConfig{{Source: "app"}}}
		_, err := instanceConfigs(p, service)
		require.Error(t, err)
	})

	t.Run("default target", func(t *testing.T) {
		t.Parallel()
		path := filepath.Join(t.TempDir(), "config.txt")
		require.NoError(t, os.WriteFile(path, []byte("val"), 0o644))
		p := &types.Project{Configs: types.Configs{"app": {File: path}}}
		service := types.ServiceConfig{Name: "web", Configs: []types.ServiceConfigObjConfig{{Source: "app"}}}

		configs, err := instanceConfigs(p, service)
		require.NoError(t, err)
		require.Len(t, configs, 1)
		assert.Equal(t, "/app", configs[0].Target)
	})
}

func TestServiceToInstanceUser(t *testing.T) {
	t.Parallel()

	opts := &ResourcesOptions{}

	// A fresh client per call: c.Resource deduplicates by name, so a shared
	// client would hand every subtest the same cached "web-1" instance.
	build := func(user string) (*client.Instance, error) {
		c := client.NewOfflineClient(t.Context(), "test")
		service := types.ServiceConfig{Name: "web", Image: "docker.io/nginx:alpine", User: user}
		p := &types.Project{Services: types.Services{"web": service}}
		inst, _, err := serviceToInstance(c, p, "web", opts, 1, 1)
		return inst, err
	}

	t.Run("uid only", func(t *testing.T) {
		t.Parallel()
		inst, err := build("1000")
		require.NoError(t, err)
		assert.Equal(t, uint64(1000), inst.Config.UID)
		assert.Equal(t, uint64(0), inst.Config.GID)
		assert.Equal(t, "1000", inst.Config.Extensions["oci.uid"])
	})

	t.Run("uid and gid", func(t *testing.T) {
		t.Parallel()
		inst, err := build("1000:1001")
		require.NoError(t, err)
		assert.Equal(t, uint64(1000), inst.Config.UID)
		assert.Equal(t, uint64(1001), inst.Config.GID)
		assert.Equal(t, "1000", inst.Config.Extensions["oci.uid"])
	})

	t.Run("no user", func(t *testing.T) {
		t.Parallel()
		inst, err := build("")
		require.NoError(t, err)
		assert.Equal(t, uint64(0), inst.Config.UID)
		assert.Equal(t, uint64(0), inst.Config.GID)
		assert.NotContains(t, inst.Config.Extensions, "oci.uid")
	})

	t.Run("invalid uid", func(t *testing.T) {
		t.Parallel()
		_, err := build("abc")
		require.Error(t, err)
		assert.ErrorContains(t, err, "cannot convert service 'web' user 'abc' to int")
	})

	t.Run("invalid gid", func(t *testing.T) {
		t.Parallel()
		_, err := build("1000:xyz")
		require.Error(t, err)
		assert.ErrorContains(t, err, "cannot convert service 'web' user 'xyz' to int")
	})
}

func TestServiceToInstancePorts(t *testing.T) {
	t.Parallel()

	t.Run("pre-7.0 server uses userspace proxy", func(t *testing.T) {
		t.Parallel()
		c := client.NewOfflineClient(t.Context(), "test")
		service := types.ServiceConfig{
			Name:  "web",
			Image: "docker.io/nginx:alpine",
			Ports: []types.ServicePortConfig{{Published: "8080", Target: 80}},
		}
		p := &types.Project{Services: types.Services{"web": service}}
		inst, _, err := serviceToInstance(c, p, "web", &ResourcesOptions{}, 1, 1)
		require.NoError(t, err)
		require.Len(t, inst.Config.Devices, 1)
		assert.False(t, inst.Config.Devices[0].Config.Proxy.Nat)
		assert.Equal(t, "127.0.0.1", inst.Config.Devices[0].Config.Proxy.ConnectAddr)
	})

	t.Run("no ports succeeds on pre-7.0 server", func(t *testing.T) {
		t.Parallel()
		c := client.NewOfflineClient(t.Context(), "test")
		service := types.ServiceConfig{Name: "web", Image: "docker.io/nginx:alpine"}
		p := &types.Project{Services: types.Services{"web": service}}
		_, _, err := serviceToInstance(c, p, "web", &ResourcesOptions{}, 1, 1)
		require.NoError(t, err)
	})
}

func TestServiceExtraDevices(t *testing.T) {
	t.Parallel()

	t.Run("raw devices", func(t *testing.T) {
		t.Parallel()
		service := types.ServiceConfig{Name: "web", Extensions: types.Extensions{
			"x-incus-compose": map[string]any{
				"devices": map[string]any{
					"gpu0": map[string]any{"type": "gpu", "gputype": "physical"},
				},
			},
		}}

		devices, err := serviceExtraDevices(service)
		require.NoError(t, err)
		require.Len(t, devices, 1)
		assert.Equal(t, "gpu0", devices[0].Name)
		assert.Equal(t, "gpu", devices[0].Config.DeviceType)
		assert.Equal(t, "physical", devices[0].Config.Extensions["gputype"])

		// Round-trips to a raw Incus device passed through verbatim.
		name, cfg, derr := devices[0].ToIncusDevice()
		require.Nil(t, derr)
		assert.Equal(t, "gpu0", name)
		assert.Equal(t, map[string]string{"type": "gpu", "gputype": "physical"}, cfg)
	})

	t.Run("missing type errors", func(t *testing.T) {
		t.Parallel()
		service := types.ServiceConfig{Name: "web", Extensions: types.Extensions{
			"x-incus-compose": map[string]any{
				"devices": map[string]any{"bad": map[string]any{"foo": "bar"}},
			},
		}}
		_, err := serviceExtraDevices(service)
		require.Error(t, err)
	})

	t.Run("device not a map errors", func(t *testing.T) {
		t.Parallel()
		service := types.ServiceConfig{Name: "web", Extensions: types.Extensions{
			"x-incus-compose": map[string]any{"devices": map[string]any{"bad": "nope"}},
		}}
		_, err := serviceExtraDevices(service)
		require.Error(t, err)
	})

	t.Run("no extension", func(t *testing.T) {
		t.Parallel()
		devices, err := serviceExtraDevices(types.ServiceConfig{Name: "web"})
		require.NoError(t, err)
		assert.Nil(t, devices)
	})
}

func TestInstanceImage(t *testing.T) {
	t.Parallel()

	c := client.NewOfflineClient(t.Context(), "test")

	t.Run("pull", func(t *testing.T) {
		t.Parallel()
		res, err := instanceImage(c, types.ServiceConfig{Name: "web", Image: "docker.io/nginx:alpine"})
		require.NoError(t, err)
		img, ok := res.(*client.Image)
		require.True(t, ok)
		assert.Contains(t, img.IncusName(), "nginx")
		assert.Contains(t, img.Config.Services, "web")
	})

	t.Run("build defaults image name", func(t *testing.T) {
		t.Parallel()
		res, err := instanceImage(c, types.ServiceConfig{Name: "web", Build: &types.BuildConfig{Context: "."}})
		require.NoError(t, err)
		img, ok := res.(*client.Image)
		require.True(t, ok)
		assert.Contains(t, img.IncusName(), "web")
	})

	t.Run("relative dockerfile resolves against the context", func(t *testing.T) {
		t.Parallel()
		res, err := instanceImage(c, types.ServiceConfig{
			Name:  "dfrel",
			Build: &types.BuildConfig{Context: "/ctx", Dockerfile: "Dockerfile"},
		})
		require.NoError(t, err)
		img, ok := res.(*client.Image)
		require.True(t, ok)
		require.NotNil(t, img.Config.Build)
		assert.Equal(t, "/ctx/Dockerfile", img.Config.Build.Dockerfile)
	})

	t.Run("absolute dockerfile is left alone", func(t *testing.T) {
		t.Parallel()
		res, err := instanceImage(c, types.ServiceConfig{
			Name:  "dfabs",
			Build: &types.BuildConfig{Context: "/ctx", Dockerfile: "/elsewhere/Dockerfile"},
		})
		require.NoError(t, err)
		img, ok := res.(*client.Image)
		require.True(t, ok)
		require.NotNil(t, img.Config.Build)
		assert.Equal(t, "/elsewhere/Dockerfile", img.Config.Build.Dockerfile)
	})

	t.Run("empty dockerfile stays empty", func(t *testing.T) {
		t.Parallel()
		res, err := instanceImage(c, types.ServiceConfig{
			Name:  "dfnone",
			Build: &types.BuildConfig{Context: "/ctx"},
		})
		require.NoError(t, err)
		img, ok := res.(*client.Image)
		require.True(t, ok)
		require.NotNil(t, img.Config.Build)
		assert.Empty(t, img.Config.Build.Dockerfile)
	})

	t.Run("build with multiple platforms errors but still builds image", func(t *testing.T) {
		t.Parallel()
		res, err := instanceImage(c, types.ServiceConfig{
			Name:  "web",
			Build: &types.BuildConfig{Context: ".", Platforms: []string{"linux/amd64", "linux/arm64"}},
		})
		require.Error(t, err)
		assert.NotNil(t, res)
	})
}

func TestInstanceNetworkDevices(t *testing.T) {
	t.Parallel()
	testlib.SkipLocal(t)

	gc, err := client.NewTestClient(t.Context())
	require.NoError(t, err)
	c, err := gc.EnsureProject("default")
	require.NoError(t, err)

	t.Run("with static ip", func(t *testing.T) {
		t.Parallel()
		skipNo73(t, c)

		p := &types.Project{Networks: types.Networks{"frontend": {}}}
		service := types.ServiceConfig{Name: "web", Networks: map[string]*types.ServiceNetworkConfig{
			"frontend": {Ipv4Address: "10.0.0.5",
				Extensions: types.Extensions{"x-incus": map[string]any{"ipv4.gateway": "10.0.0.1"}},
			},
		}}

		devices, resources, err := instanceNetworkDevices(c, p, service, "")
		require.NoError(t, err)
		require.Len(t, devices, 1)
		assert.Equal(t, "eth0", devices[0].Name)
		assert.Equal(t, client.InstanceDeviceTypeNic, devices[0].Config.DeviceType)
		assert.Equal(t, "10.0.0.5", devices[0].Config.Extensions["ipv4.address"])
		require.Len(t, resources, 1)
		assert.Equal(t, client.KindNetwork, resources[0].Kind())
	})

	t.Run("no networks", func(t *testing.T) {
		t.Parallel()
		devices, resources, err := instanceNetworkDevices(c, &types.Project{}, types.ServiceConfig{Name: "web"}, "")
		require.NoError(t, err)
		assert.Empty(t, devices)
		assert.Empty(t, resources)
	})

	t.Run("network address none or unset does not error", func(t *testing.T) {
		t.Parallel()

		p := &types.Project{Networks: types.Networks{
			"frontend": {Extensions: types.Extensions{"x-incus": map[string]any{
				"ipv4.address": "10.0.1.1/24",
				"ipv6.address": "none",
			}}},
			"backend": {},
		}}
		service := types.ServiceConfig{Name: "web", Networks: map[string]*types.ServiceNetworkConfig{
			"frontend": {},
			"backend":  {},
		}}

		devices, _, err := instanceNetworkDevices(c, p, service, "")
		require.NoError(t, err)
		assert.Len(t, devices, 2)
	})

	t.Run("static ip on a network with no address errors", func(t *testing.T) {
		t.Parallel()

		p := &types.Project{Networks: types.Networks{
			"frontend": {},
		}}
		service := types.ServiceConfig{Name: "web", Networks: map[string]*types.ServiceNetworkConfig{
			"frontend": {Ipv4Address: "10.0.0.5"},
		}}

		_, _, err := instanceNetworkDevices(c, p, service, "")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "with no address")
	})

	t.Run("gateway false allows a static ip on an address-less network", func(t *testing.T) {
		t.Parallel()
		skipNo73(t, c)

		p := &types.Project{Networks: types.Networks{
			"frontend": {},
		}}
		service := types.ServiceConfig{Name: "web", Networks: map[string]*types.ServiceNetworkConfig{
			"frontend": {
				Ipv4Address: "10.0.0.5",
				Extensions:  types.Extensions{"x-incus-compose": map[string]any{"gateway": false}},
			},
		}}

		devices, _, err := instanceNetworkDevices(c, p, service, "")
		require.NoError(t, err)
		require.Len(t, devices, 1)
		assert.Equal(t, "10.0.0.5", devices[0].Config.Extensions["ipv4.address"])
		assert.Equal(t, "none", devices[0].Config.Extensions["ipv4.gateway"])
	})

	t.Run("gateway true still requires an address", func(t *testing.T) {
		t.Parallel()

		p := &types.Project{Networks: types.Networks{
			"frontend": {},
		}}
		service := types.ServiceConfig{Name: "web", Networks: map[string]*types.ServiceNetworkConfig{
			"frontend": {
				Ipv4Address: "10.0.0.5",
				Extensions:  types.Extensions{"x-incus-compose": map[string]any{"gateway": true}},
			},
		}}

		_, _, err := instanceNetworkDevices(c, p, service, "")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "with no address")
	})

	t.Run("an external network is named by its compose name", func(t *testing.T) {
		t.Parallel()

		p := &types.Project{Networks: types.Networks{
			"ext-named": {External: true, Name: "incusbr0"},
		}}
		service := types.ServiceConfig{Name: "web", Networks: map[string]*types.ServiceNetworkConfig{
			"ext-named": {},
		}}

		_, resources, err := instanceNetworkDevices(c, p, service, "")
		require.NoError(t, err)
		require.Len(t, resources, 1)
		assert.Equal(t, "incusbr0", resources[0].IncusName())
	})

	t.Run("a managed network is named by its compose name", func(t *testing.T) {
		t.Parallel()

		p := &types.Project{Name: "proj", Networks: types.Networks{
			"man-named": {Name: "custom-net"},
		}}
		service := types.ServiceConfig{Name: "web", Networks: map[string]*types.ServiceNetworkConfig{
			"man-named": {},
		}}

		_, resources, err := instanceNetworkDevices(c, p, service, "")
		require.NoError(t, err)
		require.Len(t, resources, 1)
		assert.Equal(t, "custom-net", resources[0].IncusName())
	})

	t.Run("a managed network without a name keeps its x-incus-compose.network", func(t *testing.T) {
		t.Parallel()

		// compose-go fills Name in with {project}_{key} when no `name:` is given.
		p := &types.Project{Name: "proj", Networks: types.Networks{
			"man-plain": {
				Name:       "proj_man-plain",
				Extensions: types.Extensions{"x-incus-compose": map[string]any{"network": "man-bridge"}},
			},
		}}
		service := types.ServiceConfig{Name: "web", Networks: map[string]*types.ServiceNetworkConfig{
			"man-plain": {},
		}}

		_, resources, err := instanceNetworkDevices(c, p, service, "")
		require.NoError(t, err)
		require.Len(t, resources, 1)
		assert.Equal(t, "man-bridge", resources[0].IncusName())
	})

	t.Run("an external network without a name keeps its x-incus-compose.network", func(t *testing.T) {
		t.Parallel()

		// compose-go fills Name in with the key when no `name:` is given.
		p := &types.Project{Networks: types.Networks{
			"ext-plain": {
				External:   true,
				Name:       "ext-plain",
				Extensions: types.Extensions{"x-incus-compose": map[string]any{"network": "ext-bridge"}},
			},
		}}
		service := types.ServiceConfig{Name: "web", Networks: map[string]*types.ServiceNetworkConfig{
			"ext-plain": {},
		}}

		_, resources, err := instanceNetworkDevices(c, p, service, "")
		require.NoError(t, err)
		require.Len(t, resources, 1)
		assert.Equal(t, "ext-bridge", resources[0].IncusName())
	})

	t.Run("static ip on an address-less network is allowed with an explicit nic gateway", func(t *testing.T) {
		t.Parallel()
		skipNo73(t, c)

		p := &types.Project{Networks: types.Networks{
			"frontend": {},
		}}
		service := types.ServiceConfig{Name: "web", Networks: map[string]*types.ServiceNetworkConfig{
			"frontend": {
				Ipv4Address: "10.0.0.5",
				Ipv6Address: "fd42::5",
				Extensions: types.Extensions{"x-incus": map[string]any{
					"ipv4.gateway": "10.0.0.1",
					"ipv6.gateway": "fd42::1",
				}},
			},
		}}

		devices, _, err := instanceNetworkDevices(c, p, service, "")
		require.NoError(t, err)
		require.Len(t, devices, 1)
		assert.Equal(t, "10.0.0.5", devices[0].Config.Extensions["ipv4.address"])
		assert.Equal(t, "fd42::5", devices[0].Config.Extensions["ipv6.address"])
	})
}

func TestInstanceProxyDevices(t *testing.T) {
	t.Parallel()
	testlib.SkipLocal(t)

	gc, err := client.NewTestClient(t.Context())
	require.NoError(t, err)
	c, err := gc.EnsureProject("default")
	require.NoError(t, err)

	if !c.Global().HasExtension(shared.Incus72Extension) {
		t.Skip("Nat tests require at least incus 7.2 or 7.0.1 LTS")
	}

	t.Run("published port with nat no static IP", func(t *testing.T) {
		t.Parallel()
		service := types.ServiceConfig{Name: "web", Ports: []types.ServicePortConfig{
			{
				Published:  "8080",
				Target:     80,
				Protocol:   "tcp",
				Extensions: types.Extensions{"x-incus-compose": struct{ Nat bool }{Nat: true}},
			},
		}}

		devices, err := instanceProxyDevices(c, []client.InstanceDevice{}, service)
		require.NoError(t, err)
		require.Len(t, devices, 1)
		assert.Equal(t, "proxy-8080", devices[0].Name)
		proxy := devices[0].Config.Proxy
		assert.Equal(t, "0.0.0.0", proxy.ListenAddr)
		assert.Equal(t, uint32(8080), proxy.ListenPort)
		assert.Equal(t, "0.0.0.0", proxy.ConnectAddr)
		assert.Equal(t, uint32(80), proxy.ConnectPort)
		assert.True(t, proxy.Nat)
	})

	t.Run("published port with nat and static IP", func(t *testing.T) {
		t.Parallel()
		skipNo73(t, c)

		service := types.ServiceConfig{Name: "web", Ports: []types.ServicePortConfig{
			{
				Published:  "8080",
				Target:     80,
				Protocol:   "tcp",
				Extensions: types.Extensions{"x-incus-compose": struct{ Nat bool }{Nat: true}},
			},
		}}

		devices := []client.InstanceDevice{
			{
				Name: "eth0",
				Config: client.InstanceDeviceConfig{
					DeviceType: client.InstanceDeviceTypeNic,
					Extensions: map[string]string{
						"ipv4.address": "10.0.0.100/24",
					},
				},
			},
		}

		devices, err := instanceProxyDevices(c, devices, service)
		require.NoError(t, err)
		require.Len(t, devices, 2)
		assert.Equal(t, "proxy-8080", devices[1].Name)
		proxy := devices[1].Config.Proxy
		assert.Equal(t, "0.0.0.0", proxy.ListenAddr)
		assert.Equal(t, uint32(8080), proxy.ListenPort)
		assert.Equal(t, "10.0.0.100", proxy.ConnectAddr)
		assert.Equal(t, uint32(80), proxy.ConnectPort)
		assert.True(t, proxy.Nat)
	})

	t.Run("published port without nat", func(t *testing.T) {
		t.Parallel()
		service := types.ServiceConfig{Name: "web", Ports: []types.ServicePortConfig{
			{Published: "8080", Target: 80, Protocol: "tcp"},
		}}

		devices, err := instanceProxyDevices(c, []client.InstanceDevice{}, service)
		require.NoError(t, err)
		require.Len(t, devices, 1)
		assert.Equal(t, "proxy-8080", devices[0].Name)
		proxy := devices[0].Config.Proxy
		assert.Equal(t, "0.0.0.0", proxy.ListenAddr)
		assert.Equal(t, uint32(8080), proxy.ListenPort)
		assert.Equal(t, "127.0.0.1", proxy.ConnectAddr)
		assert.Equal(t, uint32(80), proxy.ConnectPort)
		assert.False(t, proxy.Nat)
	})

	t.Run("bad published port", func(t *testing.T) {
		t.Parallel()
		service := types.ServiceConfig{Name: "web", Ports: []types.ServicePortConfig{
			{Published: "not-a-port", Target: 80},
		}}
		_, err := instanceProxyDevices(c, []client.InstanceDevice{}, service)
		require.Error(t, err)
	})

	t.Run("no ports", func(t *testing.T) {
		t.Parallel()
		service := types.ServiceConfig{Name: "web"}
		devices, err := instanceProxyDevices(c, []client.InstanceDevice{}, service)
		require.NoError(t, err)
		assert.Empty(t, devices)
	})
}

func TestInstanceVolumeDevices(t *testing.T) {
	t.Parallel()

	c := client.NewOfflineClient(t.Context(), "test")

	t.Run("named volume", func(t *testing.T) {
		t.Parallel()
		p := &types.Project{Volumes: types.Volumes{"data": {}}}
		service := types.ServiceConfig{Name: "web", Volumes: []types.ServiceVolumeConfig{
			{Type: "volume", Source: "data", Target: "/data"},
		}}

		devices, files, resources, err := instanceVolumeDevices(c, p, service, nil, 0, 0)
		require.NoError(t, err)
		assert.Empty(t, files)
		require.Len(t, devices, 1)
		assert.Equal(t, client.InstanceDeviceTypeDisk, devices[0].Config.DeviceType)
		assert.Equal(t, "/data", devices[0].Config.Disk.Path)
		assert.Len(t, resources, 1)
	})

	t.Run("named volume with x-incus extensions", func(t *testing.T) {
		t.Parallel()
		p := &types.Project{Volumes: types.Volumes{"data": {
			Extensions: types.Extensions{"x-incus": map[string]any{"size": "5GiB"}},
		}}}
		service := types.ServiceConfig{Name: "web", Volumes: []types.ServiceVolumeConfig{
			{Type: "volume", Source: "data", Target: "/data"},
		}}

		devices, _, resources, err := instanceVolumeDevices(c, p, service, nil, 0, 0)
		require.NoError(t, err)
		require.Len(t, devices, 1)
		require.Len(t, resources, 1)
		require.NotNil(t, devices[0].Config.Disk.StorageVolumeConfig)
		assert.Equal(t, "5GiB", devices[0].Config.Disk.StorageVolumeConfig.Extensions["size"])
	})

	t.Run("named volume with security.shifted=false", func(t *testing.T) {
		t.Parallel()
		p := &types.Project{Volumes: types.Volumes{"data": {
			Extensions: types.Extensions{"x-incus": map[string]any{"security.shifted": "false"}},
		}}}
		service := types.ServiceConfig{Name: "web", Volumes: []types.ServiceVolumeConfig{
			{Type: "volume", Source: "data", Target: "/data"},
		}}

		devices, _, resources, err := instanceVolumeDevices(c, p, service, nil, 0, 0)
		require.NoError(t, err)
		require.Len(t, devices, 1)
		require.Len(t, resources, 1)
		require.NotNil(t, devices[0].Config.Disk.StorageVolumeConfig)
		assert.False(t, devices[0].Config.Disk.StorageVolumeConfig.Shifted)
		assert.Equal(t, "false", devices[0].Config.Disk.StorageVolumeConfig.Extensions["security.shifted"])
	})

	t.Run("named volume inline x-incus overrides named definition", func(t *testing.T) {
		t.Parallel()
		p := &types.Project{Volumes: types.Volumes{"data": {
			Extensions: types.Extensions{"x-incus": map[string]any{"security.shifted": "true"}},
		}}}
		service := types.ServiceConfig{Name: "web", Volumes: []types.ServiceVolumeConfig{
			{
				Type: "volume", Source: "data", Target: "/data",
				Extensions: types.Extensions{"x-incus": map[string]any{"security.shifted": "false"}},
			},
		}}

		devices, _, resources, err := instanceVolumeDevices(c, p, service, nil, 0, 0)
		require.NoError(t, err)
		require.Len(t, devices, 1)
		require.Len(t, resources, 1)
		require.NotNil(t, devices[0].Config.Disk.StorageVolumeConfig)
		assert.Equal(t, "false", devices[0].Config.Disk.StorageVolumeConfig.Extensions["security.shifted"])
		assert.False(t, devices[0].Config.Disk.StorageVolumeConfig.Shifted)
		assert.False(t, devices[0].Config.Disk.Shift)
	})

	t.Run("named volume with x-incus-compose pool", func(t *testing.T) {
		t.Parallel()
		p := &types.Project{Volumes: types.Volumes{"data": {
			Extensions: types.Extensions{"x-incus-compose": map[string]any{"pool": "fast"}},
		}}}
		service := types.ServiceConfig{Name: "web", Volumes: []types.ServiceVolumeConfig{
			{Type: "volume", Source: "data", Target: "/data"},
		}}

		devices, _, resources, err := instanceVolumeDevices(c, p, service, nil, 0, 0)
		require.NoError(t, err)
		require.Len(t, devices, 1)
		require.Len(t, resources, 1)
		require.NotNil(t, devices[0].Config.Disk.StorageVolumeConfig)
		assert.Equal(t, "fast", devices[0].Config.Disk.StorageVolumeConfig.Pool)
	})

	t.Run("bind seed file", func(t *testing.T) {
		t.Parallel()
		file := filepath.Join(t.TempDir(), "seed.conf")
		require.NoError(t, os.WriteFile(file, []byte("hello"), 0o644))
		service := types.ServiceConfig{Name: "web", Volumes: []types.ServiceVolumeConfig{
			{
				Type: "bind", Source: file, Target: "/etc/app.conf",
				Extensions: types.Extensions{"x-incus-compose": map[string]any{"seed": true}},
			},
		}}

		devices, files, resources, err := instanceVolumeDevices(c, &types.Project{}, service, nil, 0, 0)
		require.NoError(t, err)
		assert.Empty(t, devices)
		assert.Empty(t, resources)

		var found client.InstanceFile
		for _, f := range files {
			if f.Target == "/etc/app.conf" {
				found = f
				break
			}
		}
		require.NotEmpty(t, found.Target)
		assert.Equal(t, file, found.File)
	})

	t.Run("bind seed directory", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		service := types.ServiceConfig{Name: "web", Volumes: []types.ServiceVolumeConfig{
			{
				Type: "bind", Source: dir, Target: "/mnt",
				Extensions: types.Extensions{"x-incus-compose": map[string]any{"seed": true}},
			},
		}}

		devices, files, resources, err := instanceVolumeDevices(c, &types.Project{}, service, nil, 0, 0)
		require.NoError(t, err)
		assert.Empty(t, files)
		require.Len(t, devices, 1)
		assert.Equal(t, client.InstanceDeviceTypeDisk, devices[0].Config.DeviceType)
		require.NotNil(t, devices[0].Config.Disk.StorageVolumeConfig)
		assert.Equal(t, dir, devices[0].Config.Disk.StorageVolumeConfig.HostPath)
		require.Len(t, resources, 1)
	})

	t.Run("bind mount with security.shifted=false", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		service := types.ServiceConfig{Name: "web", Volumes: []types.ServiceVolumeConfig{
			{Type: "bind", Source: dir, Target: "/mnt", Extensions: types.Extensions{"x-incus": map[string]any{"security.shifted": "false"}}},
		}}

		devices, _, _, err := instanceVolumeDevices(c, &types.Project{}, service, nil, 0, 0)
		require.NoError(t, err)
		require.Len(t, devices, 1)
		assert.Equal(t, client.InstanceDeviceTypeDisk, devices[0].Config.DeviceType)
		assert.False(t, devices[0].Config.Disk.Shift)
	})

	t.Run("tmpfs", func(t *testing.T) {
		t.Parallel()
		service := types.ServiceConfig{Name: "web", Volumes: []types.ServiceVolumeConfig{
			{Type: "tmpfs", Target: "/cache"},
		}}

		devices, _, _, err := instanceVolumeDevices(c, &types.Project{}, service, nil, 0, 0)
		require.NoError(t, err)
		require.Len(t, devices, 1)
		assert.Equal(t, client.InstanceDeviceTypeTmpfs, devices[0].Config.DeviceType)
		assert.Equal(t, "/cache", devices[0].Config.Tmpfs.Path)
	})

	t.Run("bind directory", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		service := types.ServiceConfig{Name: "web", Volumes: []types.ServiceVolumeConfig{
			{Type: "bind", Source: dir, Target: "/mnt"},
		}}

		devices, files, resources, err := instanceVolumeDevices(c, &types.Project{}, service, nil, 0, 0)
		require.NoError(t, err)
		assert.Empty(t, files)
		require.Len(t, devices, 1)
		assert.Equal(t, client.InstanceDeviceTypeDisk, devices[0].Config.DeviceType)
		assert.Empty(t, resources)
	})

	t.Run("unknown type", func(t *testing.T) {
		t.Parallel()
		service := types.ServiceConfig{Name: "web", Volumes: []types.ServiceVolumeConfig{
			{Type: "weird", Source: "x", Target: "/y"},
		}}
		_, _, _, err := instanceVolumeDevices(c, &types.Project{}, service, nil, 0, 0)
		require.Error(t, err)
	})

	t.Run("missing bind source", func(t *testing.T) {
		t.Parallel()
		service := types.ServiceConfig{Name: "web", Volumes: []types.ServiceVolumeConfig{
			{Type: "bind", Source: filepath.Join(t.TempDir(), "nope"), Target: "/m"},
		}}
		_, _, _, err := instanceVolumeDevices(c, &types.Project{}, service, nil, 0, 0)
		require.Error(t, err)
	})
}
