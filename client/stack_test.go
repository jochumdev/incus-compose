package client

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/lxc/incus-compose/internal/testlib"
)

// ----------------------------------------------------------------------------
// Unit Tests (no Incus required)
// ----------------------------------------------------------------------------

// TestGroupByPriority tests the batch grouping logic without Incus.
func TestGroupByPriority(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		tasks       []Resource
		wantBatches int
		wantSizes   []int
	}{
		{
			name:        "empty tasks",
			tasks:       []Resource{},
			wantBatches: 0,
			wantSizes:   nil,
		},
		{
			name: "single task",
			tasks: []Resource{
				newMockResource("a", "", 0, false),
			},
			wantBatches: 1,
			wantSizes:   []int{1},
		},
		{
			name: "same priority groups together",
			tasks: []Resource{
				newMockResource("a", "", PriorityImage, false),
				newMockResource("b", "", PriorityImage, false),
				newMockResource("c", "", PriorityImage, false),
			},
			wantBatches: 1,
			wantSizes:   []int{3},
		},
		{
			name: "different priorities create separate batches",
			tasks: []Resource{
				newMockResource("profile", KindProfile, PriorityProfile, false),
				newMockResource("volume", KindStorageVolume, PriorityVolume, false),
				newMockResource("instance", KindInstance, PriorityInstance, false),
			},
			wantBatches: 3,
			wantSizes:   []int{1, 1, 1},
		},
		{
			name: "mixed priorities with multiple per batch",
			tasks: []Resource{
				newMockResource("profile", KindProfile, PriorityProfile, false),
				newMockResource("image", KindImage, PriorityImage, false),
				newMockResource("image2", KindImage, PriorityImage, false),
				newMockResource("volume", KindStorageVolume, PriorityVolume, false),
				newMockResource("volume2", KindStorageVolume, PriorityVolume, false),
				newMockResource("instance", KindInstance, PriorityInstance, false),
			},
			wantBatches: 4,
			wantSizes:   []int{1, 2, 2, 1},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			stack := NewStack(nil)
			stack.Add(tc.tasks...)

			batches := stack.groupByPriority()

			require.Len(t, batches, tc.wantBatches)

			if tc.wantSizes != nil {
				for i, size := range tc.wantSizes {
					require.Len(t, batches[i], size, "batch %d should have %d tasks", i, size)
				}
			}
		})
	}
}

// TestAddDeduplicatesSamePointer is a regression test for the "Alias already exists"
// race: two services sharing the same image resolve to the same Resource pointer via
// Client.Resource(), but Stack.Add used to append it twice, causing parallel Ensure
// calls on the same object.
func TestAddDeduplicatesSamePointer(t *testing.T) {
	t.Parallel()
	r := newMockResource("nginx", KindImage, PriorityImage, false)

	stack := NewStack(nil)
	stack.Add(r, r) // same pointer twice, as mkUpStack does for shared images

	require.Len(t, stack.resources, 1, "same resource added twice must appear only once")
}

// ----------------------------------------------------------------------------
// Integration Tests
// ----------------------------------------------------------------------------

// TestParallelImageDownload verifies multiple images download in parallel.
// Uses tiny busybox variants to minimize bandwidth.
func TestParallelImageDownload(t *testing.T) {
	t.Parallel()
	testlib.SkipLocal(t)
	ctx := t.Context()
	c := newRandomTestClient(ctx, t, "stack-parallel-")

	imageNames := []string{
		"docker.io/library/busybox:1.36",
		"docker.io/library/busybox:1.35",
		"docker.io/library/busybox:1.34",
	}

	stack := NewStack(c, StackWorkers(3))
	for _, name := range imageNames {
		img, err := c.Resource(KindImage, name, &ImageConfig{})
		require.NoError(t, err)
		stack.Add(img)
	}

	batches := stack.groupByPriority()
	require.Len(t, batches, 1, "all images should be in one batch")
	require.Len(t, batches[0], 3, "batch should have 3 images")

	require.NoError(t, stack.Run(ctx, ActionEnsure, OptionCreate()))

	for _, name := range imageNames {
		img, err := c.Resource(KindImage, name, &ImageConfig{})
		require.NoError(t, err)
		require.True(t, img.IsEnsured(), "image %s should be ensured", name)
	}

	t.Logf("Successfully downloaded %d images in parallel", len(imageNames))
}

func TestStackHooksWithStack(t *testing.T) {
	t.Parallel()
	testlib.SkipLocal(t)
	ctx := t.Context()
	c := newRandomTestClient(ctx, t, "stack-hooks-")

	var beforeCalled, afterCalled bool
	var afterErr error

	c.AddHookBefore(func(_ context.Context, action Action, r Resource, _ Options, err error) error {
		if action == ActionEnsure && r.Kind() == KindProfile {
			if _, ok := r.(*Profile); ok {
				beforeCalled = true
			}
		}
		return err
	})

	c.AddHookAfter(func(_ context.Context, action Action, r Resource, _ Options, err error) error {
		if action == ActionEnsure && r.Kind() == KindProfile {
			if _, ok := r.(*Profile); ok {
				afterCalled = true
				afterErr = err
			}
		}
		return err
	})

	stack := NewStack(c)
	profile, err := c.Resource(KindProfile, "test-hooks-stack", &ProfileConfig{})
	require.NoError(t, err)

	stack.Add(profile)
	require.NoError(t, stack.Run(ctx, ActionEnsure, OptionCreate()))
	require.True(t, beforeCalled, "before hook should be called")
	require.True(t, afterCalled, "after hook should be called")
	require.NoError(t, afterErr, "after hook should receive nil error")
}

func TestStackErrorAggregation(t *testing.T) {
	t.Parallel()
	testlib.SkipLocal(t)
	ctx := t.Context()
	c := newRandomTestClient(ctx, t, "stack-erragg-")

	stack := NewStack(c)

	p1, err := c.Resource(KindProfile, "error-test-1", &ProfileConfig{})
	require.NoError(t, err)

	p2, err := c.Resource(KindProfile, "error-test-2", &ProfileConfig{})
	require.NoError(t, err)

	stack.Add(p1, p2)

	err = stack.Run(ctx, ActionEnsure)
	require.Error(t, err)
	require.Contains(t, err.Error(), "error-test-1")
	require.Contains(t, err.Error(), "error-test-2")
}

func TestStackInstanceWithSecrets(t *testing.T) {
	t.Parallel()
	testlib.SkipLocal(t)
	ctx := t.Context()
	c := newRandomTestClient(ctx, t, "stack-secrets-")

	network, err := c.Resource(KindNetwork, "default", &NetworkConfig{})
	require.NoError(t, err)

	imageResource, err := c.Resource(KindImage, "docker.io/alpine:latest", &ImageConfig{})
	require.NoError(t, err)

	image, ok := imageResource.(*Image)
	require.True(t, ok)

	devices := []InstanceDevice{
		{
			Name: "eth0",
			Config: InstanceDeviceConfig{
				DeviceType: InstanceDeviceTypeNic,
				Network:    network,
			},
		},
	}

	files := []InstanceFile{
		{
			Target:  "/run/secrets/db_password",
			Content: NewReaderFromBytes([]byte("super-secret-password")),
		},
		{
			Target:  "/app/secrets/api.key",
			Content: NewReaderFromBytes([]byte("my-api-key-value")),
			UID:     0,
			GID:     0,
			Mode:    0o440,
		},
	}

	instance, err := c.Resource(KindInstance, "app-with-secrets", &InstanceConfig{
		Image:   image.Name(),
		Devices: devices,
		Files:   files,
	})
	require.NoError(t, err)

	stack := NewStack(c)
	stack.Add(network, image, instance)

	ensureStack := stack.ForAction(ActionEnsure)
	require.NoError(t, ensureStack.Run(ctx, ActionEnsure, OptionCreate()))
	for _, r := range ensureStack.All() {
		require.True(t, r.IsEnsured(), "resource %q should be ensured", r.Name())
	}
	require.NoError(t, stack.ForAction(ActionStart).Run(ctx, ActionStart))
	require.NoError(t, stack.ForAction(ActionStop).Run(ctx, ActionStop, OptionForce()))
}

func TestStackEnsureWithoutCreate_Fails(t *testing.T) {
	t.Parallel()
	testlib.SkipLocal(t)
	ctx := t.Context()
	c := newRandomTestClient(ctx, t, "stack-nocreate-")

	profile, err := c.Resource(KindProfile, "p1", &ProfileConfig{})
	require.NoError(t, err)

	stack := NewStack(c)
	stack.Add(profile)
	require.Error(t, stack.ForAction(ActionEnsure).Run(ctx, ActionEnsure))
}

func TestStackSingleProfileEnsure(t *testing.T) {
	t.Parallel()
	testlib.SkipLocal(t)
	ctx := t.Context()
	c := newRandomTestClient(ctx, t, "stack-profile-")

	profile, err := c.Resource(KindProfile, "p1", &ProfileConfig{})
	require.NoError(t, err)

	stack := NewStack(c)
	stack.Add(profile)

	ensureStack := stack.ForAction(ActionEnsure)
	require.NoError(t, ensureStack.Run(ctx, ActionEnsure, OptionCreate()))
	for _, r := range ensureStack.All() {
		require.True(t, r.IsEnsured(), "resource %q should be ensured", r.Name())
	}
}

func TestStackProfileAndNetworkMixedPriorities(t *testing.T) {
	t.Parallel()
	testlib.SkipLocal(t)
	ctx := t.Context()
	c := newRandomTestClient(ctx, t, "stack-mixed-")

	profile, err := c.Resource(KindProfile, "p1", &ProfileConfig{})
	require.NoError(t, err)

	network, err := c.Resource(KindNetwork, "n1", &NetworkConfig{})
	require.NoError(t, err)

	stack := NewStack(c)
	stack.Add(profile, network)

	ensureStack := stack.ForAction(ActionEnsure)
	require.NoError(t, ensureStack.Run(ctx, ActionEnsure, OptionCreate()))
	for _, r := range ensureStack.All() {
		require.True(t, r.IsEnsured(), "resource %q should be ensured", r.Name())
	}
}

func TestStackSimple(t *testing.T) {
	t.Parallel()
	testlib.SkipLocal(t)
	ctx := t.Context()
	c := newRandomTestClient(ctx, t, "stack-simple-")

	network, err := c.Resource(KindNetwork, "default", &NetworkConfig{})
	require.NoError(t, err)

	imageResource, err := c.Resource(KindImage, "docker.io/nginx:alpine", &ImageConfig{})
	require.NoError(t, err)

	image, ok := imageResource.(*Image)
	require.True(t, ok)

	devices := []InstanceDevice{
		{
			Name: "eth0",
			Config: InstanceDeviceConfig{
				DeviceType: InstanceDeviceTypeNic,
				Network:    network,
			},
		},
	}

	instance, err := c.Resource(KindInstance, "web", &InstanceConfig{
		Image:   image.Name(),
		Devices: devices,
	})
	require.NoError(t, err)

	stack := NewStack(c)
	stack.Add(network, image, instance)

	ensureStack := stack.ForAction(ActionEnsure)
	require.NoError(t, ensureStack.Run(ctx, ActionEnsure, OptionCreate()))
	for _, r := range ensureStack.All() {
		require.True(t, r.IsEnsured(), "resource %q should be ensured", r.Name())
	}
	require.NoError(t, stack.ForAction(ActionStart).Run(ctx, ActionStart))
	require.NoError(t, stack.ForAction(ActionStop).Run(ctx, ActionStop, OptionForce()))
}

func TestStackScale(t *testing.T) {
	t.Parallel()
	testlib.SkipLocal(t)
	ctx := t.Context()
	c := newRandomTestClient(ctx, t, "stack-scale-")

	network, err := c.Resource(KindNetwork, "default", &NetworkConfig{})
	require.NoError(t, err)

	imageResource, err := c.Resource(KindImage, "docker.io/nginx:alpine", &ImageConfig{})
	require.NoError(t, err)

	image, ok := imageResource.(*Image)
	require.True(t, ok)

	devices := []InstanceDevice{
		{
			Name: "eth0",
			Config: InstanceDeviceConfig{
				DeviceType: InstanceDeviceTypeNic,
				Network:    network,
			},
		},
	}

	resources := []Resource{network, image}
	for i := 1; i <= 3; i++ {
		instance, err := c.Resource(KindInstance, fmt.Sprintf("web-%d", i), &InstanceConfig{
			Image:   image.Name(),
			Devices: devices,
		})
		require.NoError(t, err)
		resources = append(resources, instance)
	}

	stack := NewStack(c)
	stack.Add(resources...)

	ensureStack := stack.ForAction(ActionEnsure)
	require.NoError(t, ensureStack.Run(ctx, ActionEnsure, OptionCreate()))
	for _, r := range ensureStack.All() {
		require.True(t, r.IsEnsured(), "resource %q should be ensured", r.Name())
	}
	require.NoError(t, stack.ForAction(ActionStart).Run(ctx, ActionStart))
	require.NoError(t, stack.ForAction(ActionStop).Run(ctx, ActionStop, OptionForce()))
}
