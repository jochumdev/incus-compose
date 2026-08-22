package client

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lxc/incus-compose/shared"
)

// ----------------------------------------------------------------------------
// SanitizeInstanceName Tests
// ----------------------------------------------------------------------------

func TestSanitizeInstanceName(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name              string
		input             string
		expected          string
		checkHashFallback bool
	}{
		{
			name:     "simple name",
			input:    "web",
			expected: "web",
		},
		{
			name:     "underscore replacement",
			input:    "my_service",
			expected: "my-service",
		},
		{
			name:     "uppercase to lowercase",
			input:    "MyService",
			expected: "myservice",
		},
		{
			name:     "special characters",
			input:    "my service!",
			expected: "my-service",
		},
		{
			name:              "very long name uses hash",
			input:             "this-is-a-very-long-service-name-that-exceeds-the-63-character-limit-for-incus-instances",
			checkHashFallback: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			result := SanitizeIncusName(tt.input, -1)

			if tt.checkHashFallback {
				require.Len(t, result, 32)
				require.Regexp(t, "^[0-9a-f]{32}$", result)
			} else {
				require.Equal(t, tt.expected, result)
			}
			require.LessOrEqual(t, len(result), MaxIncusNameLen)
		})
	}
}

// TestResolveEntrypoint covers the compose entrypoint/command combinations.
func TestResolveEntrypoint(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name            string
		imageEntrypoint string
		entrypoint      []string
		command         []string
		want            string
	}{
		{
			name: "neither set leaves the image alone",
			want: "",
		},
		{
			name:            "command alone keeps the image entrypoint and replaces its command",
			imageEntrypoint: "caddy",
			command:         []string{"httpd", "-f"},
			want:            "caddy httpd -f",
		},
		{
			name:    "command alone against an image with no entrypoint is the command",
			command: []string{"redis-server", "--appendonly", "yes"},
			want:    "redis-server --appendonly yes",
		},
		{
			name:       "entrypoint alone replaces the image argv",
			entrypoint: []string{"httpd", "-f", "-p", "8080"},
			want:       "httpd -f -p 8080",
		},
		{
			name:       "entrypoint and command concatenate",
			entrypoint: []string{"/bin/sh", "-c"},
			command:    []string{"echo hello"},
			want:       "/bin/sh -c 'echo hello'",
		},
		{
			name:       "empty entrypoint with a command runs the command alone",
			entrypoint: []string{},
			command:    []string{"httpd", "-f"},
			want:       "httpd -f",
		},
		{
			name:       "entrypoint discards the image entrypoint entirely",
			entrypoint: []string{"httpd"},
			want:       "httpd",
		},
		{
			name:       "arguments needing quotes survive the round trip",
			entrypoint: []string{"/bin/sh", "-c", `echo "a b" && $HOME`},
			want:       `/bin/sh -c 'echo "a b" && $HOME'`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			require.Equal(t, tt.want, resolveEntrypoint(tt.imageEntrypoint, tt.entrypoint, tt.command))
		})
	}
}

// TestInstanceEnsureAddsMissingConfigOnly pins that Ensure compares keys, not
// values: a missing one is added, an existing one keeps what it holds.
func TestInstanceEnsureAddsMissingConfigOnly(t *testing.T) {
	t.Parallel()
	skipLocal(t)
	ctx := t.Context()
	c := newRandomTestClient(t, "ensure-addmissing-")

	imageResource, err := c.Resource(KindImage, "docker.io/nginx:alpine", &ImageConfig{})
	require.NoError(t, err)
	image, ok := imageResource.(*Image)
	require.True(t, ok)

	create, err := c.Resource(KindInstance, "web", &InstanceConfig{
		Image: image.Name(),
		Extensions: map[string]string{
			HealthStatusKey: shared.HealthStatusStarting,
			"user.keep.me":  "original",
		},
	})
	require.NoError(t, err)

	stack := NewStack(c)
	stack.Add(image, create)
	require.NoError(t, stack.ForAction(ActionEnsure).Run(ctx, ActionEnsure, OptionCreate()))

	conn, err := c.Connection()
	require.NoError(t, err)

	inst, ok := create.(*Instance)
	require.True(t, ok)

	// Stand in for ic-healthd reporting, and for a key nobody declared.
	require.NoError(t, conn.PatchInstanceConfig(ctx, inst.IncusName(), map[string]string{
		HealthStatusKey: HealthStatusHealthy,
		"user.theirs":   "set-by-hand",
	}))

	// Declare one new key, and a different value for one already carried.
	inst.Config.Extensions["user.added.later"] = "true"
	inst.Config.Extensions["user.keep.me"] = "changed"

	require.NoError(t, RunAction(ctx, inst, ActionEnsure))

	got, _, err := conn.GetInstance(ctx, inst.IncusName(), nil)
	require.NoError(t, err)

	require.Equal(t, "true", got.Config["user.added.later"], "a declared key the instance lacks must be added")
	require.Equal(t, HealthStatusHealthy, got.Config[HealthStatusKey],
		"an existing key keeps its value: resetting this would undo ic-healthd on every up")
	require.Equal(t, "original", got.Config["user.keep.me"],
		"a changed value still needs --recreate; keys are compared, values are not")
	require.Equal(t, "set-by-hand", got.Config["user.theirs"], "a key nobody declared must survive")
}

// TestInstanceConfigPatchOnlyTouchesNamedKeys pins the semantics
// SetHealthCheckingStopped relies on.
func TestInstanceConfigPatchOnlyTouchesNamedKeys(t *testing.T) {
	t.Parallel()
	skipLocal(t)
	ctx := t.Context()
	c := newRandomTestClient(t, "patch-config-")

	imageResource, err := c.Resource(KindImage, "docker.io/nginx:alpine", &ImageConfig{})
	require.NoError(t, err)
	image, ok := imageResource.(*Image)
	require.True(t, ok)

	instRes, err := c.Resource(KindInstance, "web", &InstanceConfig{
		Image: image.Name(),
		Extensions: map[string]string{
			HealthStatusKey:             shared.HealthStatusStarting,
			HealthStoppedKey:            "true",
			HealthKeyPrefix + "restart": "unless-stopped",
			"user.keep.me":              "untouched",
		},
	})
	require.NoError(t, err)
	inst, ok := instRes.(*Instance)
	require.True(t, ok)

	stack := NewStack(c)
	stack.Add(image, inst)
	require.NoError(t, stack.ForAction(ActionEnsure).Run(ctx, ActionEnsure, OptionCreate()))
	require.True(t, inst.IsEnsured())

	conn, err := c.Connection()
	require.NoError(t, err)

	before, _, err := conn.GetInstance(ctx, inst.IncusName(), nil)
	require.NoError(t, err)
	require.NotEmpty(t, before.Description, "fixture needs a description to detect wiping")
	require.NotEmpty(t, before.Devices, "fixture needs devices to detect wiping")

	// Stand in for ic-healthd writing its own key.
	require.NoError(t, conn.PatchInstanceConfig(ctx, inst.IncusName(),
		map[string]string{HealthStatusKey: HealthStatusHealthy}))

	require.NoError(t, inst.SetHealthCheckingStopped(ctx, false))

	got, _, err := conn.GetInstance(ctx, inst.IncusName(), nil)
	require.NoError(t, err)

	require.Equal(t, "false", got.Config[HealthStoppedKey], "the key we asked for must be written")
	require.Equal(t, HealthStatusHealthy, got.Config[HealthStatusKey], "another writer's key must survive")
	require.Equal(t, "unless-stopped", got.Config[HealthKeyPrefix+"restart"])
	require.Equal(t, "untouched", got.Config["user.keep.me"])
	require.Equal(t, before.Description, got.Description, "PATCH must not wipe the description")
	require.Equal(t, before.Devices, got.Devices, "PATCH must not wipe devices")
	require.Equal(t, before.Profiles, got.Profiles, "PATCH must not wipe profiles")
	require.Equal(t, before.Architecture, got.Architecture, "PATCH must not wipe the architecture")
}

// TestCloneInstancesFollowLifecycleEvents pins that instances on a cloned client
// are kept fresh by the project client's listener; nothing else wakes them.
func TestCloneInstancesFollowLifecycleEvents(t *testing.T) {
	t.Parallel()
	skipLocal(t)
	ctx := t.Context()
	c := newRandomTestClient(t, "clone-events-")

	// A phase of restart: its own hooks and its own resources.
	rc := c.Clone()

	imageResource, err := rc.Resource(KindImage, "docker.io/nginx:alpine", &ImageConfig{})
	require.NoError(t, err)
	image, ok := imageResource.(*Image)
	require.True(t, ok)

	instRes, err := rc.Resource(KindInstance, "web", &InstanceConfig{
		Image:      image.Name(),
		Extensions: map[string]string{HealthStatusKey: shared.HealthStatusStarting},
	})
	require.NoError(t, err)
	inst, ok := instRes.(*Instance)
	require.True(t, ok)

	stack := NewStack(rc)
	stack.Add(image, inst)
	require.NoError(t, stack.ForAction(ActionEnsure).Run(ctx, ActionEnsure, OptionCreate()))

	conn, err := c.Connection()
	require.NoError(t, err)

	// Stand in for ic-healthd reporting a verdict, once the wait below is parked.
	go func() {
		time.Sleep(2 * time.Second)

		_ = conn.PatchInstanceConfig(ctx, inst.IncusName(),
			map[string]string{HealthStatusKey: HealthStatusHealthy})
	}()

	waitCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	require.NoError(t, inst.waitForHealthCheck(waitCtx),
		"the wait must be woken by the listener, which only reaches resources it knows about")
}

// TestFetchIntoAStateLeavesTheInstanceAlone pins the property the health wait
// rests on: a fetch handed a state of its own publishes nothing, so a poll
// cannot rewind a verdict another instance is waiting for.
//
// The client is deliberately never opened. Its event listener is the only other
// thing allowed to replace the published state, and it would race the pointer
// comparison below.
func TestFetchIntoAStateLeavesTheInstanceAlone(t *testing.T) {
	t.Parallel()
	skipLocal(t)
	ctx := t.Context()

	gc, err := NewTestClient(testContext(t))
	require.NoError(t, err)

	name := "fetch-isolation-" + strings.ToLower(RandString(12))
	c, err := createProjectClient(gc, name)
	require.NoError(t, err)

	deleteProjectOnCleanup(t, gc, name)

	imageResource, err := c.Resource(KindImage, "docker.io/nginx:alpine", &ImageConfig{})
	require.NoError(t, err)
	image, ok := imageResource.(*Image)
	require.True(t, ok)

	instRes, err := c.Resource(KindInstance, "web", &InstanceConfig{
		Image:      image.Name(),
		Extensions: map[string]string{HealthStatusKey: shared.HealthStatusStarting},
	})
	require.NoError(t, err)
	inst, ok := instRes.(*Instance)
	require.True(t, ok)

	stack := NewStack(c)
	stack.Add(image, inst)
	require.NoError(t, stack.ForAction(ActionEnsure).Run(ctx, ActionEnsure, OptionCreate()))

	require.NoError(t, inst.fetch(ctx, nil))
	published := inst.State()
	require.False(t, healthy(published), "it is not healthy before anyone reports")

	conn, err := c.Connection()
	require.NoError(t, err)
	require.NoError(t, conn.PatchInstanceConfig(ctx, inst.IncusName(),
		map[string]string{HealthStatusKey: HealthStatusHealthy}))

	local := &InstanceState{}
	require.NoError(t, inst.fetch(ctx, local))

	assert.True(t, healthy(local), "the caller's own state sees the verdict")
	assert.Same(t, published, inst.State(), "the instance's state was not replaced")
}

// TestInstanceStoppedLeavesTheStatusAlone pins the single-writer rule: a stop
// writes the intent marker ic-healthd reads, and nothing else.
func TestInstanceStoppedLeavesTheStatusAlone(t *testing.T) {
	t.Parallel()
	skipLocal(t)
	ctx := t.Context()
	c := newRandomTestClient(t, "stopped-status-")

	imageResource, err := c.Resource(KindImage, "docker.io/nginx:alpine", &ImageConfig{})
	require.NoError(t, err)
	image, ok := imageResource.(*Image)
	require.True(t, ok)

	instRes, err := c.Resource(KindInstance, "web", &InstanceConfig{
		Image: image.Name(),
	})
	require.NoError(t, err)
	inst, ok := instRes.(*Instance)
	require.True(t, ok)

	stack := NewStack(c)
	stack.Add(image, inst)
	// Healthd is on by default, which is the case this pins.
	require.NoError(t, stack.ForAction(ActionEnsure).Run(ctx, ActionEnsure, OptionCreate()))

	conn, err := c.Connection()
	require.NoError(t, err)

	got, _, err := conn.GetInstance(ctx, inst.IncusName(), nil)
	require.NoError(t, err)
	require.Empty(t, got.Config[HealthStatusKey],
		"with a daemon to report, the instance is created without a status of its own")

	// Stand in for ic-healthd having reported on it.
	require.NoError(t, conn.PatchInstanceConfig(ctx, inst.IncusName(),
		map[string]string{HealthStatusKey: HealthStatusHealthy}))

	require.NoError(t, inst.SetHealthCheckingStopped(ctx, true))

	got, _, err = conn.GetInstance(ctx, inst.IncusName(), nil)
	require.NoError(t, err)
	require.Equal(t, "true", got.Config[HealthStoppedKey], "the marker is what a stop writes")
	require.Equal(t, HealthStatusHealthy, got.Config[HealthStatusKey],
		"the status is the daemon's alone; it writes stopped from the event it sees")

	require.NoError(t, inst.SetHealthCheckingStopped(ctx, false))

	got, _, err = conn.GetInstance(ctx, inst.IncusName(), nil)
	require.NoError(t, err)
	require.Equal(t, "false", got.Config[HealthStoppedKey])
	require.Equal(t, HealthStatusHealthy, got.Config[HealthStatusKey])
}

// TestInstanceWithoutHealthdReportsUnknown pins the other half of the rule:
// with no daemon to report, the instance says so.
func TestInstanceWithoutHealthdReportsUnknown(t *testing.T) {
	t.Parallel()
	skipLocal(t)
	ctx := t.Context()
	c := newRandomTestClient(t, "nohealthd-status-")

	imageResource, err := c.Resource(KindImage, "docker.io/nginx:alpine", &ImageConfig{})
	require.NoError(t, err)
	image, ok := imageResource.(*Image)
	require.True(t, ok)

	instRes, err := c.Resource(KindInstance, "web", &InstanceConfig{
		Image: image.Name(),
	})
	require.NoError(t, err)
	inst, ok := instRes.(*Instance)
	require.True(t, ok)

	stack := NewStack(c)
	stack.Add(image, inst)
	require.NoError(t, stack.ForAction(ActionEnsure).
		Run(ctx, ActionEnsure, OptionCreate(), OptionNoHealthd()))

	conn, err := c.Connection()
	require.NoError(t, err)

	got, _, err := conn.GetInstance(ctx, inst.IncusName(), nil)
	require.NoError(t, err)
	require.Equal(t, shared.HealthStatusUnknown, got.Config[HealthStatusKey])
	require.Empty(t, got.Config[HealthStoppedKey], "there is no daemon to hold back")
}

// TestInstanceFileUnderAVolume pins that a file targeted inside a volume is
// written into the volume, not into the rootfs the mount then hides.
func TestInstanceFileUnderAVolume(t *testing.T) {
	t.Parallel()
	skipLocal(t)

	ctx := t.Context()
	c := newRandomTestClient(t, "file-in-volume-")

	imgRes, err := c.Resource(KindImage, "docker.io/nginx:alpine", &ImageConfig{})
	require.NoError(t, err)

	volRes, err := c.Resource(KindStorageVolume, "conf", &StorageVolumeConfig{
		Shifted:       true,
		ImageResource: imgRes,
	})
	require.NoError(t, err)

	vol, ok := volRes.(*StorageVolume)
	require.True(t, ok)

	instRes, err := c.Resource(KindInstance, "web", &InstanceConfig{
		Image:      imgRes.Name(),
		Resources:  []Resource{imgRes, volRes},
		Extensions: map[string]string{"oci.entrypoint": "sh"},
		Devices: []InstanceDevice{{
			Name: "imgvol-etc-nginx-conf-d",
			Config: InstanceDeviceConfig{
				DeviceType: InstanceDeviceTypeDisk,
				Disk: InstanceDeviceDiskConfig{
					StorageVolumeConfig: &vol.Config,
					Source:              vol.IncusName(),
					Path:                "/etc/nginx/conf.d",
					Shift:               true,
				},
			},
		}},
		Files: []InstanceFile{{
			Target:    "/etc/nginx/conf.d/site.conf",
			Content:   NewReaderFromBytes([]byte("server { listen 8080; }\n")),
			Mode:      0o644,
			Overwrite: true,
		}},
	})
	require.NoError(t, err)

	inst, ok := instRes.(*Instance)
	require.True(t, ok)

	stack := NewStack(c)
	stack.Add(imgRes, volRes, instRes)
	require.NoError(t, stack.ForAction(ActionEnsure).Run(ctx, ActionEnsure, OptionCreate()))
	require.NoError(t, RunAction(ctx, inst, ActionStart, OptionNoHealthd()))

	// The volume holds it under the mount point's path.
	vc, err := vol.SFTP(ctx)
	require.NoError(t, err)

	defer func() { _ = vc.Close() }()

	inVolume, err := vc.Lstat("/site.conf")
	require.NoError(t, err, "the file must be in the volume, not in the rootfs the volume hides")
	assert.Equal(t, int64(24), inVolume.Size())

	// And a running instance sees it where the compose file asked for it.
	conn, err := c.Connection()
	require.NoError(t, err)

	ic, err := conn.GetInstanceFileSFTP(ctx, inst.IncusName())
	require.NoError(t, err)

	defer func() { _ = ic.Close() }()

	_, err = ic.Lstat("/etc/nginx/conf.d/site.conf")
	require.NoError(t, err, "the running instance must see the file at its target")
}

// TestInstancePrefetchVolumes pins that a path the image declares as a volume
// gets one of its own, prefetched, and that a declared mount wins over it.
func TestInstancePrefetchVolumes(t *testing.T) {
	t.Parallel()
	skipLocal(t)

	ctx := t.Context()
	c := newRandomTestClient(t, "prefetch-volumes-")

	// isso declares /config and /db, and keeps a database in the second.
	imgRes, err := c.Resource(KindImage, "ghcr.io/isso-comments/isso:latest", &ImageConfig{})
	require.NoError(t, err)

	declared, err := c.Resource(KindStorageVolume, "mine", &StorageVolumeConfig{})
	require.NoError(t, err)

	instRes, err := c.Resource(KindInstance, "isso", &InstanceConfig{
		ServiceName: "isso",
		Image:       imgRes.Name(),
		Resources:   []Resource{imgRes, declared},
		Extensions:  map[string]string{"oci.entrypoint": "sh"},
		Devices: []InstanceDevice{{
			Name: "vol-mine",
			Config: InstanceDeviceConfig{
				DeviceType: InstanceDeviceTypeDisk,
				Disk: InstanceDeviceDiskConfig{
					StorageVolumeConfig: &StorageVolumeConfig{Pool: c.Config().DefaultStoragePool},
					Source:              declared.IncusName(),
					Path:                "/config",
				},
			},
		}},
	})
	require.NoError(t, err)

	inst, ok := instRes.(*Instance)
	require.True(t, ok)

	stack := NewStack(c)
	stack.Add(imgRes, declared, instRes)
	require.NoError(t, stack.ForAction(ActionEnsure).Run(ctx, ActionEnsure, OptionCreate()))

	require.Equal(t, []string{"/config", "/db"}, imgRes.(*Image).State().Volumes)

	devices := inst.State().IncusInstance.Devices
	require.Contains(t, devices, "imgvol-db", "the image declares /db and nothing else mounts there")
	assert.Equal(t, "/db", devices["imgvol-db"]["path"])
	assert.NotContains(t, devices, "imgvol-config", "a declared mount at the same target wins")

	// The volume it points at is one this client holds and can act on.
	auto, err := c.Resource(KindStorageVolume, prefetchVolumeName("isso", "/db"), nil)
	require.NoError(t, err)
	assert.Equal(t, devices["imgvol-db"]["source"], auto.IncusName())

	// A second client, which never created any of it, finds the same volume
	// through the instance alone - what down and backup rely on.
	other := c.Clone()

	adopted, err := other.Resource(KindInstance, "isso", &InstanceConfig{ServiceName: "isso"})
	require.NoError(t, err)
	require.NoError(t, RunAction(ctx, adopted, ActionEnsure))

	found, err := other.Resource(KindStorageVolume, prefetchVolumeName("isso", "/db"), nil)
	require.NoError(t, err, "an instance must name its own volumes without its image")
	assert.Equal(t, auto.IncusName(), found.IncusName())
}

// TestInstancePauseUnpause covers the freeze/unfreeze round trip and the
// guards on either side of it.
func TestInstancePauseUnpause(t *testing.T) {
	t.Parallel()
	skipLocal(t)

	ctx := t.Context()
	c := newRandomTestClient(t, "pause-")

	image, err := c.Resource(KindImage, "docker.io/alpine:edge", &ImageConfig{})
	require.NoError(t, err)

	instRes, err := c.Resource(KindInstance, "web", &InstanceConfig{
		Image:      image.Name(),
		Extensions: map[string]string{"oci.entrypoint": "sh"},
	})
	require.NoError(t, err)

	inst, ok := instRes.(*Instance)
	require.True(t, ok)

	stack := NewStack(c)
	stack.Add(image, inst)
	require.NoError(t, stack.ForAction(ActionEnsure).
		Run(ctx, ActionEnsure, OptionCreate(), OptionNoHealthd()))

	require.ErrorIs(t, RunAction(ctx, inst, ActionPause), ErrNotRunning,
		"a stopped instance has nothing to freeze")

	require.NoError(t, RunAction(ctx, inst, ActionStart, OptionNoHealthd()))
	require.True(t, inst.Running())

	require.NoError(t, RunAction(ctx, inst, ActionPause))
	assert.True(t, inst.Frozen())
	assert.False(t, inst.Running(), "frozen and running never both hold")

	// The marker is what keeps ic-healthd from restarting out of the pause.
	assert.Equal(t, "true", inst.State().IncusInstance.Config[HealthStoppedKey])

	assert.ErrorIs(t, RunAction(ctx, inst, ActionPause), ErrPaused)

	require.NoError(t, RunAction(ctx, inst, ActionUnpause))
	assert.True(t, inst.Running())
	assert.False(t, inst.Frozen())
	assert.Equal(t, "false", inst.State().IncusInstance.Config[HealthStoppedKey])

	assert.ErrorIs(t, RunAction(ctx, inst, ActionUnpause), ErrNotPaused)
}
