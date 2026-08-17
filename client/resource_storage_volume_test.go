package client

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/lxc/incus-compose/internal/testlib"
)

// ----------------------------------------------------------------------------
// Local-only Tests (no Incus required)
// ----------------------------------------------------------------------------

func TestStorageVolumeResource_ReturnsSameInstance(t *testing.T) {
	t.Parallel()
	c := NewOfflineClient(t.Context(), "volume-test")

	r1, err := c.Resource(KindStorageVolume, "test-same", &StorageVolumeConfig{})
	require.NoError(t, err)

	r2, err := c.Resource(KindStorageVolume, "test-same", &StorageVolumeConfig{})
	require.NoError(t, err)

	require.Same(t, r1, r2)
}

func TestStorageVolumeResource_DifferentNamesAreDifferent(t *testing.T) {
	t.Parallel()
	c := NewOfflineClient(t.Context(), "volume-test")

	r1, err := c.Resource(KindStorageVolume, "volume-a", &StorageVolumeConfig{})
	require.NoError(t, err)

	r2, err := c.Resource(KindStorageVolume, "volume-b", &StorageVolumeConfig{})
	require.NoError(t, err)

	require.NotSame(t, r1, r2)
}

func TestStorageVolumeIncusName_PrefixedWithProject(t *testing.T) {
	t.Parallel()
	c := NewOfflineClient(t.Context(), "volume-test")

	r, err := c.Resource(KindStorageVolume, "mydata", &StorageVolumeConfig{})
	require.NoError(t, err)

	vol, ok := r.(*StorageVolume)
	require.True(t, ok)
	require.Equal(t, "mydata", vol.Name())
	require.Equal(t, "vol-mydata", vol.IncusName())
}

func TestStorageVolumeConfig_DefaultPool(t *testing.T) {
	t.Parallel()
	c := NewOfflineClient(t.Context(), "volume-test")

	r, err := c.Resource(KindStorageVolume, "default-pool", &StorageVolumeConfig{})
	require.NoError(t, err)

	vol, ok := r.(*StorageVolume)
	require.True(t, ok)
	require.Equal(t, c.Config().DefaultStoragePool, vol.Config.Pool)
}

func TestStorageVolumeConfig_CustomPool(t *testing.T) {
	t.Parallel()
	c := NewOfflineClient(t.Context(), "volume-test")

	r, err := c.Resource(KindStorageVolume, "custom-pool", &StorageVolumeConfig{Pool: "mypool"})
	require.NoError(t, err)

	vol, ok := r.(*StorageVolume)
	require.True(t, ok)
	require.Equal(t, "mypool", vol.Config.Pool)
}

// ----------------------------------------------------------------------------
// Ensure Tests
// ----------------------------------------------------------------------------

func TestStorageVolumeEnsure(t *testing.T) {
	t.Parallel()
	testlib.SkipLocal(t)
	ctx := t.Context()

	tests := []struct {
		name     string
		volume   string
		config   *StorageVolumeConfig
		opts     []Option
		wantErr  bool
		validate func(*testing.T, Resource)
	}{
		{
			name:   "with create",
			volume: "test-vol",
			config: &StorageVolumeConfig{},
			opts:   []Option{OptionCreate()},
		},
		{
			name:    "without create fails",
			volume:  "non-existent",
			config:  &StorageVolumeConfig{},
			wantErr: true,
			validate: func(t *testing.T, r Resource) {
				t.Helper()
				require.False(t, r.IsEnsured())
			},
		},
		{
			name:   "shifted volume",
			volume: "test-shifted",
			config: &StorageVolumeConfig{Shifted: true, UID: 1000, GID: 1000},
			opts:   []Option{OptionCreate()},
			validate: func(t *testing.T, r Resource) {
				t.Helper()
				vol, ok := r.(*StorageVolume)
				require.True(t, ok)
				require.NotNil(t, vol.State().IncusVolume)
				require.Equal(t, "true", vol.State().IncusVolume.Config["security.shifted"])
				require.Equal(t, "1000", vol.State().IncusVolume.Config["initial.uid"])
				require.Equal(t, "1000", vol.State().IncusVolume.Config["initial.gid"])
			},
		},
		{
			name:   "extra config",
			volume: "test-extra",
			config: &StorageVolumeConfig{Extensions: map[string]string{"size": "5GiB"}},
			opts:   []Option{OptionCreate()},
			validate: func(t *testing.T, r Resource) {
				t.Helper()
				vol, ok := r.(*StorageVolume)
				require.True(t, ok)
				require.Equal(t, "5GiB", vol.State().IncusVolume.Config["size"])
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			c := newRandomTestClient(t, "volume-ensure-")

			r, err := c.Resource(KindStorageVolume, tt.volume, tt.config)
			require.NoError(t, err)

			err = RunAction(ctx, r, ActionEnsure, tt.opts...)
			if tt.wantErr {
				require.Error(t, err)
				require.ErrorIs(t, err, ErrNotFound)
			} else {
				require.NoError(t, err)
				require.True(t, r.IsEnsured())
			}
			if tt.validate != nil {
				tt.validate(t, r)
			}
		})
	}
}

func TestStorageVolumeEnsure_Idempotent(t *testing.T) {
	t.Parallel()
	testlib.SkipLocal(t)
	ctx := t.Context()
	c := newRandomTestClient(t, "volume-idempotent-")

	r, err := c.Resource(KindStorageVolume, "test-idempotent", &StorageVolumeConfig{})
	require.NoError(t, err)

	require.NoError(t, RunAction(ctx, r, ActionEnsure, OptionCreate()))
	require.True(t, r.IsEnsured())

	require.NoError(t, RunAction(ctx, r, ActionEnsure, OptionCreate()))
	require.True(t, r.IsEnsured())
}

func TestStorageVolumeEnsure_WithoutCreate_ThenWithCreate(t *testing.T) {
	t.Parallel()
	testlib.SkipLocal(t)
	ctx := t.Context()
	c := newRandomTestClient(t, "volume-retry-")

	r, err := c.Resource(KindStorageVolume, "test-retry", &StorageVolumeConfig{})
	require.NoError(t, err)

	err = RunAction(ctx, r, ActionEnsure)
	require.Error(t, err)
	require.False(t, r.IsEnsured())

	err = RunAction(ctx, r, ActionEnsure, OptionCreate())
	require.NoError(t, err)
	require.True(t, r.IsEnsured())
}

func TestStorageVolumeEnsure_ShiftedVolume_Start(t *testing.T) {
	t.Parallel()
	testlib.SkipLocal(t)
	ctx := t.Context()
	c := newRandomTestClient(t, "volume-shifted-")

	r, err := c.Resource(KindStorageVolume, "test-shifted", &StorageVolumeConfig{
		Shifted: true,
		UID:     1000,
		GID:     1000,
	})
	require.NoError(t, err)

	require.NoError(t, RunAction(ctx, r, ActionEnsure, OptionCreate()))
	require.NoError(t, RunAction(ctx, r, ActionStart))
}

func TestStorageVolumeEnsure_HealthdShiftedVolume(t *testing.T) {
	t.Parallel()
	testlib.SkipLocal(t)
	ctx := t.Context()
	c := newRandomTestClient(t, "volume-healthd-")

	ir, err := c.Resource(KindImage, "ghcr.io/lxc/incus-compose/ic-healthd:latest", &ImageConfig{})
	require.NoError(t, err)
	require.NoError(t, RunAction(ctx, ir, ActionEnsure, OptionCreate()))

	r, err := c.Resource(KindStorageVolume, "test-healthd-shifted", &StorageVolumeConfig{
		Shifted:       true,
		ImageResource: ir,
	})
	require.NoError(t, err)

	require.NoError(t, RunAction(ctx, r, ActionEnsure, OptionCreate()))

	vol, ok := r.(*StorageVolume)
	require.True(t, ok)
	require.NotNil(t, vol.State().IncusVolume)
	require.Equal(t, "true", vol.State().IncusVolume.Config["security.shifted"])
	require.Equal(t, "65534", vol.State().IncusVolume.Config["initial.uid"])
	require.Equal(t, "65534", vol.State().IncusVolume.Config["initial.gid"])
}

func TestStorageVolumeEnsure_ExistsOnNewClient(t *testing.T) {
	t.Parallel()
	testlib.SkipLocal(t)
	ctx := t.Context()
	c := newRandomTestClient(t, "volume-persist-")

	r, err := c.Resource(KindStorageVolume, "test-persist", &StorageVolumeConfig{})
	require.NoError(t, err)
	require.NoError(t, RunAction(ctx, r, ActionEnsure, OptionCreate()))

	newClient, err := c.globalClient.getProject(c.project)
	require.NoError(t, err)

	r2, err := newClient.Resource(KindStorageVolume, "test-persist", &StorageVolumeConfig{})
	require.NoError(t, err)

	require.NoError(t, RunAction(ctx, r2, ActionEnsure))
	require.True(t, r2.IsEnsured())
}

// ----------------------------------------------------------------------------
// Delete Tests
// ----------------------------------------------------------------------------

func TestStorageVolumeDelete(t *testing.T) {
	t.Parallel()
	testlib.SkipLocal(t)
	ctx := t.Context()

	tests := []struct {
		name   string
		ensure bool
	}{
		{
			name:   "after ensure",
			ensure: true,
		},
		{
			name: "not ensured no error",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			c := newRandomTestClient(t, "volume-delete-")

			r, err := c.Resource(KindStorageVolume, "test-delete", &StorageVolumeConfig{})
			require.NoError(t, err)

			if tt.ensure {
				require.NoError(t, RunAction(ctx, r, ActionEnsure, OptionCreate()))
				require.True(t, r.IsEnsured())
			}

			require.NoError(t, RunAction(ctx, r, ActionDelete, OptionForce()))
			require.False(t, r.IsEnsured())
		})
	}
}

// ----------------------------------------------------------------------------
// Hook Tests
// ----------------------------------------------------------------------------

func TestStorageVolumeHooks(t *testing.T) {
	t.Parallel()
	testlib.SkipLocal(t)
	ctx := t.Context()

	tests := []struct {
		name string
		run  func(*testing.T, *Client)
	}{
		{
			name: "before is called",
			run: func(t *testing.T, c *Client) {
				t.Helper()
				called := false
				c.AddHookBefore(func(_ context.Context, action Action, r Resource, _ Options, err error) error {
					if action == ActionEnsure && r.Kind() == KindStorageVolume {
						called = true
					}
					return err
				})
				r, err := c.Resource(KindStorageVolume, "test-before-hook", &StorageVolumeConfig{})
				require.NoError(t, err)
				require.NoError(t, RunAction(ctx, r, ActionEnsure, OptionCreate()))
				require.True(t, called, "before hook should have been called")
			},
		},
		{
			name: "after is called",
			run: func(t *testing.T, c *Client) {
				t.Helper()
				called := false
				c.AddHookAfter(func(_ context.Context, action Action, r Resource, _ Options, err error) error {
					if action == ActionEnsure && r.Kind() == KindStorageVolume {
						called = true
					}
					return err
				})
				r, err := c.Resource(KindStorageVolume, "test-after-hook", &StorageVolumeConfig{})
				require.NoError(t, err)
				require.NoError(t, RunAction(ctx, r, ActionEnsure, OptionCreate()))
				require.True(t, called, "after hook should have been called")
			},
		},
		{
			name: "after receives error",
			run: func(t *testing.T, c *Client) {
				t.Helper()
				var receivedErr error
				c.AddHookAfter(func(_ context.Context, action Action, r Resource, _ Options, err error) error {
					if action == ActionEnsure && r.Kind() == KindStorageVolume {
						receivedErr = err
					}
					return err
				})
				r, err := c.Resource(KindStorageVolume, "non-existent", &StorageVolumeConfig{})
				require.NoError(t, err)
				_ = RunAction(ctx, r, ActionEnsure)
				require.NotNil(t, receivedErr, "after hook should receive the error")
			},
		},
		{
			name: "before can abort",
			run: func(t *testing.T, c *Client) {
				t.Helper()
				c.AddHookBefore(func(_ context.Context, _ Action, r Resource, _ Options, err error) error {
					if r.Name() == "abort-me" {
						return ErrAborted
					}
					return err
				})
				r, err := c.Resource(KindStorageVolume, "abort-me", &StorageVolumeConfig{})
				require.NoError(t, err)
				err = RunAction(ctx, r, ActionEnsure, OptionCreate())
				require.ErrorIs(t, err, ErrAborted)
				require.False(t, r.IsEnsured())
			},
		},
		{
			name: "after can modify error",
			run: func(t *testing.T, c *Client) {
				t.Helper()
				c.AddHookAfter(func(_ context.Context, _ Action, _ Resource, _ Options, err error) error {
					if err != nil {
						return ErrAborted
					}
					return nil
				})
				r, err := c.Resource(KindStorageVolume, "non-existent", &StorageVolumeConfig{})
				require.NoError(t, err)
				err = RunAction(ctx, r, ActionEnsure)
				require.ErrorIs(t, err, ErrAborted)
			},
		},
		{
			name: "delete action",
			run: func(t *testing.T, c *Client) {
				t.Helper()
				var lastAction Action
				c.AddHookBefore(func(_ context.Context, a Action, _ Resource, _ Options, err error) error {
					lastAction = a
					return err
				})
				r, err := c.Resource(KindStorageVolume, "test-delete-hook", &StorageVolumeConfig{})
				require.NoError(t, err)
				require.NoError(t, RunAction(ctx, r, ActionEnsure, OptionCreate()))
				require.Equal(t, ActionEnsure, lastAction)
				require.NoError(t, RunAction(ctx, r, ActionDelete))
				require.Equal(t, ActionDelete, lastAction)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			c := newRandomTestClient(t, "volume-hook-")
			tt.run(t, c)
		})
	}
}

func TestStorageVolumeEnsure_ConcurrentCreate(t *testing.T) {
	testlib.SkipLocal(t)
	ctx := t.Context()

	// One project, one volume name, so every worker races to create it.
	c := newRandomTestClient(t, "volume-race-")
	name := "ic-vol-" + strings.ToLower(RandString(6))

	const workers = 6
	vols := make([]Resource, workers)
	for i := range vols {
		r, err := newStorageVolume(c, name, &StorageVolumeConfig{})
		require.NoError(t, err)
		vols[i] = r
	}

	var wg sync.WaitGroup
	errs := make([]error, workers)
	start := make(chan struct{})

	for i, r := range vols {
		wg.Add(1)
		go func() {
			defer wg.Done()

			<-start

			errs[i] = RunAction(ctx, r, ActionEnsure, OptionCreate())
		}()
	}

	close(start)
	wg.Wait()

	for i, err := range errs {
		require.NoError(t, err, i)
		require.True(t, vols[i].IsEnsured(), i)
	}
}
