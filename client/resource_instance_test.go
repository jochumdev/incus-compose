package client

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lxc/incus-compose/internal/testlib"
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
	testlib.SkipLocal(t)
	ctx := t.Context()
	c := newRandomTestClient(ctx, t, "ensure-addmissing-")

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
	testlib.SkipLocal(t)
	ctx := t.Context()
	c := newRandomTestClient(ctx, t, "patch-config-")

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
	testlib.SkipLocal(t)
	ctx := t.Context()
	c := newRandomTestClient(ctx, t, "clone-events-")

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
	testlib.SkipLocal(t)
	ctx := t.Context()

	gc, err := NewTestClient(t.Context())
	require.NoError(t, err)

	name := "fetch-isolation-" + strings.ToLower(RandString(12))
	c, err := createProjectClient(gc, name)
	require.NoError(t, err)

	t.Cleanup(func() { _ = gc.DeleteProject(name, true) })

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
	testlib.SkipLocal(t)
	ctx := t.Context()
	c := newRandomTestClient(ctx, t, "stopped-status-")

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
	testlib.SkipLocal(t)
	ctx := t.Context()
	c := newRandomTestClient(ctx, t, "nohealthd-status-")

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
