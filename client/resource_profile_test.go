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

func TestProfileResource_ReturnsSameInstance(t *testing.T) {
	t.Parallel()
	c := NewOfflineClient(t.Context(), "profile-test")

	r1, err := c.Resource(KindProfile, "test-same", &ProfileConfig{})
	require.NoError(t, err)

	r2, err := c.Resource(KindProfile, "test-same", &ProfileConfig{})
	require.NoError(t, err)

	require.Same(t, r1, r2)
}

func TestProfileResource_DifferentNamesAreDifferent(t *testing.T) {
	t.Parallel()
	c := NewOfflineClient(t.Context(), "profile-test")

	r1, err := c.Resource(KindProfile, "profile-a", &ProfileConfig{})
	require.NoError(t, err)

	r2, err := c.Resource(KindProfile, "profile-b", &ProfileConfig{})
	require.NoError(t, err)

	require.NotSame(t, r1, r2)
}

func TestProfileIncusName_Sanitized(t *testing.T) {
	t.Parallel()
	c := NewOfflineClient(t.Context(), "profile-test")

	r, err := c.Resource(KindProfile, "Test_Profile", &ProfileConfig{})
	require.NoError(t, err)

	profile, ok := r.(*Profile)
	require.True(t, ok)
	require.Equal(t, "Test_Profile", profile.Name())
	require.Equal(t, "test-profile", profile.IncusName())
}

// ----------------------------------------------------------------------------
// Ensure Tests
// ----------------------------------------------------------------------------

func TestProfileEnsure(t *testing.T) {
	t.Parallel()
	testlib.SkipLocal(t)
	ctx := t.Context()

	tests := []struct {
		name     string
		profile  string
		config   *ProfileConfig
		opts     []Option
		wantErr  bool
		validate func(*testing.T, Resource)
	}{
		{
			name:    "with create",
			profile: "test-profile",
			config:  &ProfileConfig{},
			opts:    []Option{OptionCreate()},
		},
		{
			name:    "without create fails",
			profile: "non-existent",
			config:  &ProfileConfig{},
			wantErr: true,
			validate: func(t *testing.T, r Resource) {
				t.Helper()
				require.False(t, r.IsEnsured())
			},
		},
		{
			name:    "long name",
			profile: "this-is-a-very-long-profile-name-that-exceeds-normal-limits",
			config:  &ProfileConfig{},
			opts:    []Option{OptionCreate()},
		},
		{
			name:    "copies from default profile",
			profile: "test-default-copy",
			config:  &ProfileConfig{SourceProject: "default", SourceProfile: "default"},
			opts:    []Option{OptionCreate()},
			validate: func(t *testing.T, r Resource) {
				t.Helper()
				profile, ok := r.(*Profile)
				require.True(t, ok)
				require.NotNil(t, profile.State().IncusProfile)
				require.NotEmpty(t, profile.State().IncusProfile.Devices)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			c := newRandomTestClient(ctx, t, "profile-ensure-")

			r, err := c.Resource(KindProfile, tt.profile, tt.config)
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

func TestProfileEnsure_Idempotent(t *testing.T) {
	t.Parallel()
	testlib.SkipLocal(t)
	ctx := t.Context()
	c := newRandomTestClient(ctx, t, "profile-idempotent-")

	r, err := c.Resource(KindProfile, "test-idempotent", &ProfileConfig{})
	require.NoError(t, err)

	require.NoError(t, RunAction(ctx, r, ActionEnsure, OptionCreate()))
	require.True(t, r.IsEnsured())

	require.NoError(t, RunAction(ctx, r, ActionEnsure, OptionCreate()))
	require.True(t, r.IsEnsured())
}

func TestProfileEnsure_WithoutCreate_ThenWithCreate(t *testing.T) {
	t.Parallel()
	testlib.SkipLocal(t)
	ctx := t.Context()
	c := newRandomTestClient(ctx, t, "profile-retry-")

	r, err := c.Resource(KindProfile, "test-retry", &ProfileConfig{})
	require.NoError(t, err)

	err = RunAction(ctx, r, ActionEnsure)
	require.Error(t, err)
	require.False(t, r.IsEnsured())

	err = RunAction(ctx, r, ActionEnsure, OptionCreate())
	require.NoError(t, err)
	require.True(t, r.IsEnsured())
}

func TestProfileEnsure_ExistsOnNewClient(t *testing.T) {
	t.Parallel()
	testlib.SkipLocal(t)
	ctx := t.Context()
	c := newRandomTestClient(ctx, t, "profile-persist-")

	r, err := c.Resource(KindProfile, "test-persist", &ProfileConfig{})
	require.NoError(t, err)
	require.NoError(t, RunAction(ctx, r, ActionEnsure, OptionCreate()))

	newClient, err := c.globalClient.getProject(c.project)
	require.NoError(t, err)

	r2, err := newClient.Resource(KindProfile, "test-persist", &ProfileConfig{})
	require.NoError(t, err)

	require.NoError(t, RunAction(ctx, r2, ActionEnsure))
	require.True(t, r2.IsEnsured())
}

// ----------------------------------------------------------------------------
// Delete Tests
// ----------------------------------------------------------------------------

func TestProfileDelete(t *testing.T) {
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
			c := newRandomTestClient(ctx, t, "profile-delete-")

			r, err := c.Resource(KindProfile, "test-delete", &ProfileConfig{})
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

func TestProfileHooks(t *testing.T) {
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
					if action == ActionEnsure && r.Kind() == KindProfile {
						called = true
					}
					return err
				})
				r, err := c.Resource(KindProfile, "test-before-hook", &ProfileConfig{})
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
					if action == ActionEnsure && r.Kind() == KindProfile {
						called = true
					}
					return err
				})
				r, err := c.Resource(KindProfile, "test-after-hook", &ProfileConfig{})
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
					if action == ActionEnsure && r.Kind() == KindProfile {
						receivedErr = err
					}
					return err
				})
				r, err := c.Resource(KindProfile, "non-existent", &ProfileConfig{})
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
				r, err := c.Resource(KindProfile, "abort-me", &ProfileConfig{})
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
				r, err := c.Resource(KindProfile, "non-existent", &ProfileConfig{})
				require.NoError(t, err)
				err = RunAction(ctx, r, ActionEnsure)
				require.ErrorIs(t, err, ErrAborted)
			},
		},
		{
			name: "before FIFO",
			run: func(t *testing.T, c *Client) {
				t.Helper()
				var order []int
				for _, n := range []int{1, 2, 3} {
					c.AddHookBefore(func(_ context.Context, _ Action, _ Resource, _ Options, err error) error {
						order = append(order, n)
						return err
					})
				}
				r, err := c.Resource(KindProfile, "test-fifo", &ProfileConfig{})
				require.NoError(t, err)
				require.NoError(t, RunAction(ctx, r, ActionEnsure, OptionCreate()))
				require.Equal(t, []int{1, 2, 3}, order, "before hooks should run FIFO")
			},
		},
		{
			name: "after LIFO",
			run: func(t *testing.T, c *Client) {
				t.Helper()
				var order []int
				for _, n := range []int{1, 2, 3} {
					c.AddHookAfter(func(_ context.Context, _ Action, _ Resource, _ Options, err error) error {
						order = append(order, n)
						return err
					})
				}
				r, err := c.Resource(KindProfile, "test-lifo", &ProfileConfig{})
				require.NoError(t, err)
				require.NoError(t, RunAction(ctx, r, ActionEnsure, OptionCreate()))
				require.Equal(t, []int{3, 2, 1}, order, "after hooks should run LIFO")
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
				r, err := c.Resource(KindProfile, "test-delete-hook", &ProfileConfig{})
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
			c := newRandomTestClient(ctx, t, "profile-hook-")
			tt.run(t, c)
		})
	}
}

func TestProfileEnsure_ConcurrentCreate(t *testing.T) {
	testlib.SkipLocal(t)
	ctx := t.Context()

	// One project, so every worker races for the same profile.
	c := newRandomTestClient(ctx, t, "profile-race-")
	name := "ic-prof-" + strings.ToLower(RandString(6))

	const workers = 6
	profiles := make([]Resource, workers)
	for i := range profiles {
		r, err := newProfile(c, name, &ProfileConfig{})
		require.NoError(t, err)
		profiles[i] = r
	}

	var wg sync.WaitGroup
	errs := make([]error, workers)
	start := make(chan struct{})

	for i, r := range profiles {
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
		require.True(t, profiles[i].IsEnsured(), i)
	}
}
