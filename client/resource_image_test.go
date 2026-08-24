package client

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	incusApi "github.com/lxc/incus/v7/shared/api"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lxc/incus-compose/iclient"
)

// ----------------------------------------------------------------------------
// Local-only Tests (no Incus required)
// ----------------------------------------------------------------------------

func TestImageParsing(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name              string
		imageName         string
		expectedRemote    string
		expectedImage     string
		expectedIncusName string
		wantErr           bool
	}{
		{
			name:              "full docker reference",
			imageName:         "docker.io/library/alpine:3.18",
			expectedRemote:    "docker.io",
			expectedImage:     "library/alpine:3.18",
			expectedIncusName: "docker.io/library/alpine:3.18",
		},
		{
			name:              "full github reference",
			imageName:         "ghcr.io/linuxcontainers/alpine:latest",
			expectedRemote:    "ghcr.io",
			expectedImage:     "linuxcontainers/alpine:latest",
			expectedIncusName: "ghcr.io/linuxcontainers/alpine:latest",
		},
		{
			name:              "short reference defaults to docker.io",
			imageName:         "nginx:alpine",
			expectedRemote:    "docker.io",
			expectedImage:     "library/nginx:alpine",
			expectedIncusName: "docker.io/library/nginx:alpine",
		},
		{
			name:              "ghcr.io reference",
			imageName:         "ghcr.io/someorg/someimage:v1.0",
			expectedRemote:    "ghcr.io",
			expectedImage:     "someorg/someimage:v1.0",
			expectedIncusName: "ghcr.io/someorg/someimage:v1.0",
		},
		{
			name:              "localhost converted to local",
			imageName:         "localhost/myimage:latest",
			expectedRemote:    "local",
			expectedImage:     "myimage:latest",
			expectedIncusName: "local/myimage:latest",
		},
		{
			name:              "image with no tag gets latest",
			imageName:         "alpine",
			expectedRemote:    "docker.io",
			expectedImage:     "library/alpine:latest",
			expectedIncusName: "docker.io/library/alpine:latest",
		},
		{
			name:              "it adds library",
			imageName:         "docker.io/nginx:alpine",
			expectedRemote:    "docker.io",
			expectedImage:     "library/nginx:alpine",
			expectedIncusName: "docker.io/library/nginx:alpine",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			c := NewOfflineClient(t.Context(), "test")
			img, err := newImage(c, tt.imageName, &ImageConfig{})
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tt.expectedRemote, img.Remote())
			require.Equal(t, tt.expectedImage, img.image)
			require.Equal(t, tt.expectedIncusName, img.IncusName())
		})
	}
}

func TestImageResource_SameIncusNameReturnsSameObject(t *testing.T) {
	t.Parallel()
	c := NewOfflineClient(t.Context(), "test")

	r1, err := c.Resource(KindImage, "docker.io/nginx:alpine", &ImageConfig{})
	require.NoError(t, err)

	r2, err := c.Resource(KindImage, "docker.io/nginx:alpine", &ImageConfig{})
	require.NoError(t, err)

	require.Same(t, r1, r2, "same image name must return the same object")
}

func TestImageResource_NormalizedFormReturnsSameObject(t *testing.T) {
	t.Parallel()
	c := NewOfflineClient(t.Context(), "test")

	r1, err := c.Resource(KindImage, "docker.io/nginx:alpine", &ImageConfig{})
	require.NoError(t, err)

	r2, err := c.Resource(KindImage, "docker.io/library/nginx:alpine", &ImageConfig{})
	require.NoError(t, err)

	require.Same(t, r1, r2, "short and canonical form must return the same object")
}

func TestImageResource_ReturnsSameInstance(t *testing.T) {
	t.Parallel()
	c := NewOfflineClient(t.Context(), "test")

	r1, err := c.Resource(KindImage, "docker.io/library/busybox:latest", &ImageConfig{})
	require.NoError(t, err)

	r2, err := c.Resource(KindImage, "docker.io/library/busybox:latest", &ImageConfig{})
	require.NoError(t, err)

	require.Same(t, r1, r2)
}

func TestImageResource_DifferentNamesAreDifferent(t *testing.T) {
	t.Parallel()
	c := NewOfflineClient(t.Context(), "test")

	r1, err := c.Resource(KindImage, "docker.io/library/busybox:latest", &ImageConfig{})
	require.NoError(t, err)

	r2, err := c.Resource(KindImage, "docker.io/library/busybox:1.37", &ImageConfig{})
	require.NoError(t, err)

	require.NotSame(t, r1, r2)
}

func TestImageIncusName_MatchesInput(t *testing.T) {
	t.Parallel()
	c := NewOfflineClient(t.Context(), "test")

	r, err := c.Resource(KindImage, "docker.io/library/busybox:latest", &ImageConfig{})
	require.NoError(t, err)

	image, ok := r.(*Image)
	require.True(t, ok)
	require.Equal(t, "docker.io/library/busybox:latest", image.Name())
	require.Equal(t, "docker.io/library/busybox:latest", image.IncusName())
}

func TestImageConfig_RemoteAndImageParsed(t *testing.T) {
	t.Parallel()
	c := NewOfflineClient(t.Context(), "test")

	r, err := c.Resource(KindImage, "docker.io/library/alpine:3.18", &ImageConfig{})
	require.NoError(t, err)

	image, ok := r.(*Image)
	require.True(t, ok)
	require.Equal(t, "docker.io", image.Remote())
	require.Equal(t, "library/alpine:3.18", image.image)
}

// ----------------------------------------------------------------------------
// Ensure Tests
// ----------------------------------------------------------------------------

func TestImageEnsure(t *testing.T) {
	t.Parallel()
	skipLocal(t)
	ctx := t.Context()

	tests := []struct {
		name    string
		image   string
		opts    []Option
		wantErr bool
	}{
		{
			name:  "with create busybox",
			image: "docker.io/library/busybox:latest",
			opts:  []Option{OptionCreate()},
		},
		{
			name:  "with create github",
			image: "ghcr.io/ghcr-library/busybox:glibc",
			opts:  []Option{OptionCreate()},
		},
		{
			name:    "without create fails",
			image:   "docker.io/library/busybox:glibc",
			wantErr: true,
		},
		{
			name:    "bad image fails",
			image:   "docker.io/library/nonexistent-image-xyz123:latest",
			opts:    []Option{OptionCreate()},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			c := newRandomTestClient(t, "image-ensure-")

			r, err := c.Resource(KindImage, tt.image, &ImageConfig{})
			require.NoError(t, err)

			actionCtx := ctx

			// Bounded, so a regression fails here instead of sitting in copyToCache's retry.
			if tt.wantErr {
				var cancel context.CancelFunc

				actionCtx, cancel = context.WithTimeout(ctx, 2*time.Minute)
				defer cancel()
			}

			err = RunAction(actionCtx, r, ActionEnsure, tt.opts...)
			if tt.wantErr {
				require.Error(t, err)
				require.ErrorIs(t, err, ErrNotFound)
				require.NotErrorIs(t, err, context.DeadlineExceeded,
					"a missing image must fail fast, not sit in the retry")
				require.False(t, r.IsEnsured())
			} else {
				require.NoError(t, err)
				require.True(t, r.IsEnsured())

				image, ok := r.(*Image)
				require.True(t, ok)
				require.NotNil(t, image.State().IncusAlias)
			}
		})
	}
}

func TestImageEnsure_Idempotent(t *testing.T) {
	t.Parallel()
	skipLocal(t)
	ctx := t.Context()
	c := newRandomTestClient(t, "image-idempotent-")

	r, err := c.Resource(KindImage, "docker.io/library/busybox:latest", &ImageConfig{})
	require.NoError(t, err)

	require.NoError(t, RunAction(ctx, r, ActionEnsure, OptionCreate()))
	require.True(t, r.IsEnsured())

	require.NoError(t, RunAction(ctx, r, ActionEnsure, OptionCreate()))
	require.True(t, r.IsEnsured())
}

func TestImageEnsure_WithoutCreate_ThenWithCreate(t *testing.T) {
	t.Parallel()
	skipLocal(t)
	ctx := t.Context()
	c := newRandomTestClient(t, "image-retry-")

	r, err := c.Resource(KindImage, "docker.io/library/busybox:latest", &ImageConfig{})
	require.NoError(t, err)

	err = RunAction(ctx, r, ActionEnsure)
	require.Error(t, err)
	require.False(t, r.IsEnsured())

	err = RunAction(ctx, r, ActionEnsure, OptionCreate())
	require.NoError(t, err)
	require.True(t, r.IsEnsured())
}

func TestImageEnsure_ExistingImage_NewResource(t *testing.T) {
	t.Parallel()
	skipLocal(t)
	ctx := t.Context()
	c := newRandomTestClient(t, "image-existing-")

	r1, err := c.Resource(KindImage, "docker.io/library/busybox:latest", &ImageConfig{})
	require.NoError(t, err)
	require.NoError(t, RunAction(ctx, r1, ActionEnsure, OptionCreate()))
	require.True(t, r1.IsEnsured())

	newClient, err := c.globalClient.getProject(c.project)
	require.NoError(t, err)

	r2, err := newClient.Resource(KindImage, "docker.io/library/busybox:latest", &ImageConfig{})
	require.NoError(t, err)

	require.NoError(t, RunAction(ctx, r2, ActionEnsure))
	require.True(t, r2.IsEnsured())
	require.False(t, r2.Created(), "fetched resource should have Created() false")
}

func TestImageEnsure_ExistsOnNewClient(t *testing.T) {
	t.Parallel()
	skipLocal(t)
	ctx := t.Context()
	c := newRandomTestClient(t, "image-persist-")

	r, err := c.Resource(KindImage, "docker.io/library/busybox:latest", &ImageConfig{})
	require.NoError(t, err)
	require.NoError(t, RunAction(ctx, r, ActionEnsure, OptionCreate()))

	newClient, err := c.globalClient.getProject(c.project)
	require.NoError(t, err)

	r2, err := newClient.Resource(KindImage, "docker.io/library/busybox:latest", &ImageConfig{})
	require.NoError(t, err)

	require.NoError(t, RunAction(ctx, r2, ActionEnsure))
	require.True(t, r2.IsEnsured())
}

// ----------------------------------------------------------------------------
// Delete Tests
// ----------------------------------------------------------------------------

func TestImageDelete(t *testing.T) {
	t.Parallel()
	skipLocal(t)
	ctx := t.Context()

	tests := []struct {
		name   string
		ensure bool
	}{
		{
			name:   "image exists removed",
			ensure: true,
		},
		{
			name: "no image is noop",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			c := newRandomTestClient(t, "image-delete-")

			r, err := c.Resource(KindImage, "docker.io/library/busybox:latest", &ImageConfig{})
			require.NoError(t, err)

			if tt.ensure {
				require.NoError(t, RunAction(ctx, r, ActionEnsure, OptionCreate()))
				require.True(t, r.IsEnsured())
			}

			require.NoError(t, RunAction(ctx, r, ActionDelete, OptionForce()))

			if tt.ensure {
				alias, _, _ := c.incus.GetImageAlias(ctx, c.incusProject, r.(*Image).IncusName(), nil)
				require.Nil(t, alias, "image should be gone after Delete")
			}
		})
	}
}

func TestImageDelete_NotEnsured_NoError(t *testing.T) {
	t.Parallel()
	skipLocal(t)
	ctx := t.Context()
	c := newRandomTestClient(t, "image-delne-")

	r, err := c.Resource(KindImage, "docker.io/library/busybox:latest", &ImageConfig{})
	require.NoError(t, err)

	require.NoError(t, RunAction(ctx, r, ActionDelete))
}

// ----------------------------------------------------------------------------
// Properties Test
// ----------------------------------------------------------------------------

func TestImageProperties(t *testing.T) {
	t.Parallel()
	skipLocal(t)
	ctx := t.Context()
	c := newRandomTestClient(t, "image-props-")

	r, err := c.Resource(KindImage, "ghcr.io/lxc/incus-compose/ic-healthd:latest", &ImageConfig{})
	require.NoError(t, err)

	require.NoError(t, RunAction(ctx, r, ActionEnsure, OptionCreate()))

	image, ok := r.(*Image)
	require.True(t, ok)
	assert.Equal(t, "ghcr.io/lxc/incus-compose/ic-healthd:latest", image.Name())
	assert.Equal(t, "/", image.State().Cwd)
	assert.Equal(t, 65534, int(image.State().UID))
	assert.Equal(t, 65534, int(image.State().GID))
}

func TestImageFromCache(t *testing.T) {
	t.Parallel()
	skipLocal(t)
	ctx := t.Context()
	c := newRandomTestClient(t, "image-from-cache-")

	r, err := c.Resource(KindImage, "docker.io/library/alpine:3.21", &ImageConfig{})
	require.NoError(t, err)

	require.NoError(t, RunAction(ctx, r, ActionEnsure, OptionCreate()))
	require.NoError(t, RunAction(ctx, r, ActionDelete))

	img, ok := r.(*Image)
	require.True(t, ok)

	// Get should fail after delete.
	require.Error(t, img.get(ctx))

	// Create should work without a source (no panic) as we have the image in cache.
	img.source = nil
	require.NoError(t, img.create(ctx, NewOptions()))
}

func TestImagePullNever_StoreHit(t *testing.T) {
	t.Parallel()
	skipLocal(t)
	ctx := t.Context()
	c := newRandomTestClient(t, "image-pull-never-hit-")

	r, err := c.Resource(KindImage, "docker.io/library/alpine:3.21", &ImageConfig{})
	require.NoError(t, err)

	require.NoError(t, RunAction(ctx, r, ActionEnsure, OptionCreate()))
	require.NoError(t, RunAction(ctx, r, ActionDelete))

	img, ok := r.(*Image)
	require.True(t, ok)
	require.Error(t, img.get(ctx))

	// Served from the cache without ever consulting the source.
	img.source = nil
	require.NoError(t, RunAction(ctx, r, ActionEnsure, OptionCreate(), OptionPullMode(PullNever)))
}

func TestImagePullNever_StoreMiss(t *testing.T) {
	t.Parallel()
	skipLocal(t)
	ctx := t.Context()
	c := newRandomTestClient(t, "image-pull-never-miss-")

	// A name the shared test cache cannot already hold; never is expected to
	// fail without contacting the registry at all.
	name := "docker.io/library/ic-absent-" + strings.ToLower(RandString(8)) + ":latest"
	r, err := c.Resource(KindImage, name, &ImageConfig{})
	require.NoError(t, err)

	err = RunAction(ctx, r, ActionEnsure, OptionCreate(), OptionPullMode(PullNever))
	require.ErrorIs(t, err, ErrNotFound)
}

func TestImageBuild_StoreHitSkipsBuilder(t *testing.T) {
	t.Parallel()
	skipLocal(t)
	ctx := t.Context()

	// Seed the cache under the alias the build would produce.
	seed := newRandomTestClient(t, "image-build-seed-")
	r, err := seed.Resource(KindImage, "docker.io/library/alpine:3.20", &ImageConfig{})
	require.NoError(t, err)
	require.NoError(t, RunAction(ctx, r, ActionEnsure, OptionCreate()))

	// Another project wanting the same alias must copy from the cache. The
	// context does not exist, so invoking the builder would fail.
	c := newRandomTestClient(t, "image-build-cachehit-")
	b, err := c.Resource(KindImage, "docker.io/library/alpine:3.20", &ImageConfig{
		Build: &BuildConfig{Context: "/nonexistent-build-context"},
	})
	require.NoError(t, err)

	require.NoError(t, RunAction(ctx, b, ActionEnsure, OptionCreate()))
	require.True(t, b.IsEnsured())
}

// lockableImage returns an Image with its cache resolved, ready for lockStore.
func lockableImage(t *testing.T, c *Client, name string) *Image {
	t.Helper()

	r, err := c.Resource(KindImage, name, &ImageConfig{})
	require.NoError(t, err)

	img, ok := r.(*Image)
	require.True(t, ok)
	require.NoError(t, img.setupCacheAndSource())

	return img
}

func TestImageLockStore_SameAliasSerializes(t *testing.T) {
	skipLocal(t)
	ctx := t.Context()

	name := "docker.io/library/ic-lock-" + strings.ToLower(RandString(8)) + ":latest"
	a := lockableImage(t, newRandomTestClient(t, "image-lock-same-a-"), name)
	b := lockableImage(t, newRandomTestClient(t, "image-lock-same-b-"), name)

	release, err := a.lockStore(ctx)
	require.NoError(t, err)

	// The t.Fatal below returns without reaching the release further down.
	release = sync.OnceFunc(release)
	defer release()

	acquired := make(chan error, 1)
	go func() {
		releaseB, err := b.lockStore(ctx)
		if releaseB != nil {
			releaseB()
		}
		acquired <- err
	}()

	select {
	case <-acquired:
		t.Fatal("second lock acquired while the first was held")
	case <-time.After(time.Second):
	}

	release()

	select {
	case err := <-acquired:
		require.NoError(t, err)
	case <-time.After(30 * time.Second):
		t.Fatal("second lock never acquired after release")
	}
}

func TestImageLockStore_DifferentAliasesDoNotBlock(t *testing.T) {
	skipLocal(t)
	ctx := t.Context()

	suffix := strings.ToLower(RandString(8))
	a := lockableImage(t, newRandomTestClient(t, "image-lock-diff-a-"), "docker.io/library/ic-locka-"+suffix+":latest")
	b := lockableImage(t, newRandomTestClient(t, "image-lock-diff-b-"), "docker.io/library/ic-lockb-"+suffix+":latest")

	release, err := a.lockStore(ctx)
	require.NoError(t, err)
	defer release()

	acquired := make(chan error, 1)
	go func() {
		releaseB, err := b.lockStore(ctx)
		if releaseB != nil {
			releaseB()
		}
		acquired <- err
	}()

	select {
	case err := <-acquired:
		require.NoError(t, err)
	case <-time.After(30 * time.Second):
		t.Fatal("a different alias blocked on an unrelated lock")
	}
}

func TestImageEnsure_ConcurrentSameImage(t *testing.T) {
	skipLocal(t)
	ctx := t.Context()

	const workers = 4
	images := make([]*Image, workers)
	for i := range images {
		c := newRandomTestClient(t, "image-concurrent-same-")
		r, err := c.Resource(KindImage, "docker.io/library/alpine:3.21", &ImageConfig{})
		require.NoError(t, err)

		img, ok := r.(*Image)
		require.True(t, ok)
		images[i] = img
	}

	var wg sync.WaitGroup
	errs := make([]error, workers)

	for i, img := range images {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = RunAction(ctx, img, ActionEnsure, OptionCreate())
		}()
	}
	wg.Wait()

	// One alias, so every worker must end up on the same fingerprint.
	for i, err := range errs {
		require.NoError(t, err, i)
		require.True(t, images[i].IsEnsured(), i)
		require.Equal(t, images[0].State().IncusAlias.Target, images[i].State().IncusAlias.Target, i)
	}
}

func TestImageEnsure_ConcurrentDifferentImages(t *testing.T) {
	skipLocal(t)
	ctx := t.Context()

	names := []string{
		"docker.io/library/alpine:3.21",
		"docker.io/library/alpine:3.20",
		"docker.io/library/busybox:latest",
	}

	images := make([]*Image, len(names))
	for i, name := range names {
		c := newRandomTestClient(t, "image-concurrent-diff-")
		r, err := c.Resource(KindImage, name, &ImageConfig{})
		require.NoError(t, err)

		img, ok := r.(*Image)
		require.True(t, ok)
		images[i] = img
	}

	var wg sync.WaitGroup
	errs := make([]error, len(names))

	for i, img := range images {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = RunAction(ctx, img, ActionEnsure, OptionCreate())
		}()
	}
	wg.Wait()

	seen := map[string]bool{}
	for i, err := range errs {
		require.NoError(t, err, names[i])
		require.True(t, images[i].IsEnsured(), names[i])

		target := images[i].State().IncusAlias.Target
		require.False(t, seen[target], "distinct images share a fingerprint")
		seen[target] = true
	}
}

func TestImageEnsure_ProjectCopySurvivesCacheDeletion(t *testing.T) {
	skipLocal(t)
	ctx := t.Context()
	c := newRandomTestClient(t, "image-cache-pruned-")

	r, err := c.Resource(KindImage, "docker.io/library/busybox:1.33", &ImageConfig{})
	require.NoError(t, err)
	require.NoError(t, RunAction(ctx, r, ActionEnsure, OptionCreate()))

	img, ok := r.(*Image)
	require.True(t, ok)
	require.NotNil(t, img.cache)

	// Another project prunes the cache entry out from under us.
	cacheAlias, _, err := img.cache.incus.GetImageAlias(ctx, img.cache.incusProject, img.incusName, nil)
	require.NoError(t, err)

	op, err := img.cache.incus.DeleteImage(ctx, img.cache.incusProject, cacheAlias.Target)
	require.NoError(t, err)

	_, err = iclient.WaitOperation(ctx, op)
	require.NoError(t, err)

	_, _, err = img.cache.incus.GetImageAlias(ctx, img.cache.incusProject, img.incusName, nil)
	require.Error(t, err)

	// The project still holds it, so Ensure must answer from there.
	require.NoError(t, RunAction(ctx, r, ActionEnsure, OptionCreate()))
	require.True(t, r.IsEnsured())

	// A refill would mean it went looking past its own project.
	_, _, err = img.cache.incus.GetImageAlias(ctx, img.cache.incusProject, img.incusName, nil)
	require.Error(t, err, "ensure repopulated the cache instead of using the project copy")
}

func TestImagePullNever_NoCacheStoreMiss(t *testing.T) {
	skipLocal(t)
	ctx := t.Context()
	c := newRandomTestClient(t, "image-never-nocache-")
	c.imageCache = nil

	r, err := c.Resource(KindImage, "docker.io/library/alpine:3.21", &ImageConfig{})
	require.NoError(t, err)

	// Without a cache the project is the store, and it is empty.
	err = RunAction(ctx, r, ActionEnsure, OptionCreate(), OptionPullMode(PullNever))
	require.ErrorIs(t, err, ErrNotFound)
}

func TestImageCreateDirect_NoSource(t *testing.T) {
	skipLocal(t)
	ctx := t.Context()
	c := newRandomTestClient(t, "image-nosource-")
	c.imageCache = nil

	img := lockableImage(t, c, "docker.io/library/alpine:3.21")
	img.source = nil

	err := img.createDirect(ctx, NewOptions(OptionCreate()))
	require.ErrorIs(t, err, ErrImageSource)
}

func TestImageLockStore_NoCacheIsNoop(t *testing.T) {
	skipLocal(t)
	ctx := t.Context()
	c := newRandomTestClient(t, "image-lock-nocache-")
	c.imageCache = nil

	img := lockableImage(t, c, "docker.io/library/alpine:3.21")
	require.Nil(t, img.cache)

	release, err := img.lockStore(ctx)
	require.NoError(t, err)
	require.NotNil(t, release)
	release()
}

func TestImageLockStore_CustomVolumeIsSeparate(t *testing.T) {
	skipLocal(t)
	ctx := t.Context()

	name := "docker.io/library/ic-lockvol-" + strings.ToLower(RandString(8)) + ":latest"

	def := lockableImage(t, newRandomTestClient(t, "image-lockvol-def-"), name)

	other, err := newRandomTestClient(t, "image-lockvol-alt-").Resource(KindImage, name, &ImageConfig{
		LockVolume: "ic-lock-" + strings.ToLower(RandString(6)),
	})
	require.NoError(t, err)

	alt, ok := other.(*Image)
	require.True(t, ok)
	require.NoError(t, alt.setupCacheAndSource())

	release, err := def.lockStore(ctx)
	require.NoError(t, err)
	defer release()

	// Same alias, different lock volume, so the lock files cannot collide.
	acquired := make(chan error, 1)
	go func() {
		releaseAlt, err := alt.lockStore(ctx)
		if releaseAlt != nil {
			releaseAlt()
		}
		acquired <- err
	}()

	select {
	case err := <-acquired:
		require.NoError(t, err)
	case <-time.After(30 * time.Second):
		t.Fatal("a custom lock volume blocked on the default one")
	}
}

func TestImageLockStore_ConcurrentVolumeCreate(t *testing.T) {
	skipLocal(t)
	ctx := t.Context()

	// A volume name nothing has created yet, so every worker races to make it.
	// Distinct aliases keep the per-alias lock from serializing them and
	// hiding the race.
	volume := "ic-lock-" + strings.ToLower(RandString(8))
	suffix := strings.ToLower(RandString(8))

	const workers = 6
	images := make([]*Image, workers)
	for i := range images {
		c := newRandomTestClient(t, "image-lockvol-race-")
		r, err := c.Resource(KindImage, fmt.Sprintf("docker.io/library/ic-race%d-%s:latest", i, suffix), &ImageConfig{
			LockVolume: volume,
		})
		require.NoError(t, err)

		img, ok := r.(*Image)
		require.True(t, ok)
		require.NoError(t, img.setupCacheAndSource())
		images[i] = img
	}

	var wg sync.WaitGroup
	errs := make([]error, workers)
	start := make(chan struct{})

	for i, img := range images {
		wg.Add(1)
		go func() {
			defer wg.Done()

			<-start

			release, err := img.lockStore(ctx)
			if release != nil {
				release()
			}
			errs[i] = err
		}()
	}

	close(start)
	wg.Wait()

	for i, err := range errs {
		require.NoError(t, err, i)
	}
}

func TestImageBuild_NeverErrors(t *testing.T) {
	skipLocal(t)
	ctx := t.Context()
	c := newRandomTestClient(t, "image-build-never-")

	r, err := c.Resource(KindImage, "localhost/ic-build-never-"+strings.ToLower(RandString(8))+":latest", &ImageConfig{
		Build: &BuildConfig{Context: "/nonexistent-build-context"},
	})
	require.NoError(t, err)

	err = RunAction(ctx, r, ActionEnsure, OptionCreate(), OptionBuild(BuildInfo{Mode: BuildNever}))
	require.Error(t, err)
	require.False(t, r.IsEnsured())
}

func TestImageBuild_WithoutCreateDoesNotBuild(t *testing.T) {
	skipLocal(t)
	ctx := t.Context()
	c := newRandomTestClient(t, "image-build-nocreate-")

	// The context does not exist, so a build attempt would fail differently.
	r, err := c.Resource(KindImage, "localhost/ic-build-nocreate-"+strings.ToLower(RandString(8))+":latest", &ImageConfig{
		Build: &BuildConfig{Context: "/nonexistent-build-context"},
	})
	require.NoError(t, err)

	err = RunAction(ctx, r, ActionEnsure)
	require.ErrorIs(t, err, ErrNotFound)
	require.False(t, r.IsEnsured())
}

func TestImageBuild_ForceIgnoresStoreHit(t *testing.T) {
	skipLocal(t)
	ctx := t.Context()

	// Seed the cache under the alias a build would produce.
	seed := newRandomTestClient(t, "image-build-force-seed-")
	s, err := seed.Resource(KindImage, "docker.io/library/alpine:3.20", &ImageConfig{})
	require.NoError(t, err)
	require.NoError(t, RunAction(ctx, s, ActionEnsure, OptionCreate()))

	// BuildForce must reach the builder despite the store hit, and the
	// context does not exist, so it has to fail.
	c := newRandomTestClient(t, "image-build-force-")
	b, err := c.Resource(KindImage, "docker.io/library/alpine:3.20", &ImageConfig{
		Build: &BuildConfig{Context: "/nonexistent-build-context"},
	})
	require.NoError(t, err)

	err = RunAction(ctx, b, ActionEnsure, OptionCreate(), OptionBuild(BuildInfo{Mode: BuildForce}))
	require.Error(t, err)
	require.Contains(t, err.Error(), "building container image")
}

func TestImageNoCache(t *testing.T) {
	t.Parallel()
	skipLocal(t)
	ctx := t.Context()
	c := newRandomTestClient(t, "image-no-cache-")
	c.imageCache = nil

	r, err := c.Resource(KindImage, "docker.io/library/busybox:latest", &ImageConfig{})
	require.NoError(t, err)

	require.NoError(t, RunAction(ctx, r, ActionEnsure, OptionCreate()))
}

// The refresh deletes the image between resolving its source and pulling it
// back, so anything cleared there is read from the registry a second time.
func TestImageClearStateKeepsWhatTheSourceHeld(t *testing.T) {
	t.Parallel()
	c := NewOfflineClient(t.Context(), "test")

	r, err := c.Resource(KindImage, "docker.io/library/busybox:latest", &ImageConfig{})
	require.NoError(t, err)

	img, ok := r.(*Image)
	require.True(t, ok)

	img.updateState(func(s *ImageState) {
		s.IncusAlias = &incusApi.ImageAliasesEntry{Name: "docker.io/library/busybox:latest"}
		s.ETag = "an-etag"
		s.Size = 42
		s.Cmd = "sh"
		s.SourceFingerprint = "b0a1"
	})

	img.clearState()

	state := img.State()
	require.False(t, img.IsEnsured())
	assert.Empty(t, state.ETag)
	assert.Zero(t, state.Size)

	assert.Equal(t, "b0a1", state.SourceFingerprint)
	assert.Equal(t, "sh", state.Cmd, "the config describes the content, not the fetch")
}

func TestImageResolveSource(t *testing.T) {
	t.Parallel()
	skipLocal(t)
	ctx := t.Context()
	c := newRandomTestClient(t, "image-resolve-source-")

	r, err := c.Resource(KindImage, "docker.io/library/alpine:3.22", &ImageConfig{})
	require.NoError(t, err)

	require.NoError(t, RunAction(ctx, r, ActionEnsure, OptionCreate()))

	deletes := 0
	c.AddHookAfter(func(_ context.Context, action Action, r Resource, _ Options, err error) error {
		if action == ActionDelete && r.Kind() == KindImage {
			deletes++
		}

		return err
	})

	require.NoError(t, RunAction(ctx, r, ActionEnsure, OptionCreate(), OptionResolveSource()))

	img, ok := r.(*Image)
	require.True(t, ok)

	state := img.State()
	require.NotNil(t, state.IncusAlias)
	require.NotEmpty(t, state.SourceFingerprint)

	// The tag has not moved, so what the registry resolves to must be the
	// fingerprint the stored image already carries. incusd hashes the layer
	// digests rather than the manifest, and any other shape here would read as
	// changed on every run.
	assert.Equal(t, state.IncusAlias.Target, state.SourceFingerprint)
	assert.Zero(t, deletes, "resolving a source deletes nothing")
}

func TestImageDeleteCache(t *testing.T) {
	t.Parallel()
	skipLocal(t)

	tests := []struct {
		name   string
		image  string
		opts   []Option
		cached bool
	}{
		{
			name:   "a plain delete leaves the cached copy",
			image:  "docker.io/library/busybox:1.35",
			cached: true,
		},
		{
			name:  "OptionCache takes the cached copy with it",
			image: "docker.io/library/busybox:1.34",
			opts:  []Option{OptionCache()},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ctx := t.Context()

			// A cache of its own: deleting out of the shared one would pull an
			// image from under whatever else is copying it at that moment.
			cache := newRandomTestClient(t, "image-delete-store-")
			c := newRandomTestClient(t, "image-delete-cache-")

			r, err := c.Resource(KindImage, tt.image, &ImageConfig{CacheClient: cache})
			require.NoError(t, err)

			require.NoError(t, RunAction(ctx, r, ActionEnsure, OptionCreate()))

			_, _, err = cache.incus.GetImageAlias(ctx, cache.incusProject, tt.image, nil)
			require.NoError(t, err, "the ensure should have left %q in the cache", tt.image)

			require.NoError(t, RunAction(ctx, r, ActionDelete, tt.opts...))

			_, _, err = cache.incus.GetImageAlias(ctx, cache.incusProject, tt.image, nil)
			if tt.cached {
				require.NoError(t, err, "a plain delete must leave the cache alone")

				return
			}

			require.Error(t, err, "the cached copy must be gone")
		})
	}
}

// ----------------------------------------------------------------------------
// Hook Tests
// ----------------------------------------------------------------------------

func TestImageHooks(t *testing.T) {
	t.Parallel()
	skipLocal(t)
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
					if action == ActionEnsure && r.Kind() == KindImage {
						called = true
					}
					return err
				})
				r, err := c.Resource(KindImage, "docker.io/library/busybox:latest", &ImageConfig{})
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
					if action == ActionEnsure && r.Kind() == KindImage {
						called = true
					}
					return err
				})
				r, err := c.Resource(KindImage, "docker.io/library/busybox:latest", &ImageConfig{})
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
					if action == ActionEnsure && r.Kind() == KindImage {
						receivedErr = err
					}
					return err
				})
				r, err := c.Resource(KindImage, "docker.io/library/nonexistent:latest", &ImageConfig{})
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
					if r.Name() == "docker.io/library/abort-me:latest" {
						return ErrAborted
					}
					return err
				})
				r, err := c.Resource(KindImage, "docker.io/library/abort-me:latest", &ImageConfig{})
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
				r, err := c.Resource(KindImage, "docker.io/library/nonexistent:latest", &ImageConfig{})
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
				r, err := c.Resource(KindImage, "docker.io/library/busybox:latest", &ImageConfig{})
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
			c := newRandomTestClient(t, "image-hook-")
			tt.run(t, c)
		})
	}
}
