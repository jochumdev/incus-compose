package client

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/avast/retry-go/v5"
	"github.com/distribution/reference"
	incusApi "github.com/lxc/incus/v7/shared/api"
	"github.com/pkg/sftp"

	"github.com/lxc/incus-compose/iclient"
)

// PullMode controls when an image is refreshed from its source.
type PullMode int

const (
	// PullMissing contacts the source only when the store has no copy (default).
	PullMissing PullMode = iota
	// PullAlways refreshes from the source even when the store has a copy.
	PullAlways
	// PullNever never contacts the source; a store miss is an error.
	PullNever
)

// DefaultCacheProject is the Incus project images are cached in.
const DefaultCacheProject = "incus-compose-cache"

// DefaultLockVolume is the storage volume holding the per-alias image locks.
const DefaultLockVolume = "ic-image-lock"

// imageLockStale is how long an image lock may go unrefreshed before another
// caller reaps it; the holder heartbeats at a third of it.
const imageLockStale = 2 * time.Minute

// ImageConfig contains the source and cache configuration for an image.
type ImageConfig struct {
	// CacheClient is the project-scoped client to use as cache (for library
	// users). Takes precedence over CacheProject.
	CacheClient *Client

	// CacheProject is the project name to use as cache (for CLI users).
	// The project will be created if it doesn't exist.
	// Ignored if CacheClient is set.
	CacheProject string

	// LockVolume names the storage volume in the cache project holding the
	// per-alias locks. Empty means DefaultLockVolume.
	LockVolume string

	// Build, when set, marks this image as locally built rather than pulled
	// from a registry. Ensure will shell out to podman/docker instead of
	// calling CopyImage.
	Build *BuildConfig

	// A list of service dependencies for log output.
	Services []string
}

// GetConfig returns the configuration.
func (c *ImageConfig) GetConfig() any {
	return c
}

var _ Config = (*ImageConfig)(nil)

// Image represents an OCI or native Incus image copied to the Incus image cache.
type Image struct {
	*BaseResource

	client    *Client
	Config    ImageConfig
	incusName string
	created   bool

	// mu serializes the actions; two workers may share one resource object.
	// Nothing the actions call may take it again - it is not reentrant.
	mu sync.Mutex

	// remote is the domain part of the image reference
	remote string

	// image is the image reference without the remote prefix
	image string

	// cache is the resolved client for caching, nil when caching is off
	cache *Client

	// source is where incusd fetches this image from.
	source *imageSource

	// nativeIncus indicates this is a native Incus image (protocol "incus")
	// rather than an OCI image (protocol "oci").
	nativeIncus bool

	// state is swapped whole, so a reader never sees a half-updated image.
	state atomic.Pointer[ImageState]

	// sftpMu guards the reader instance below, and is held across its creation
	// so a second caller waits rather than starting one of its own.
	sftpMu   sync.Mutex
	sftpName string
	sftpConn *sftp.Client
}

// imageSource is where incusd fetches an image from. A registry that is not a
// configured remote gets an info synthesized from its well-known address, so
// every source is described the same way.
type imageSource struct {
	info *iclient.ConfigRemoteInfo

	// conn is set for a native incus remote only; anything else incusd resolves itself.
	conn *iclient.Connection
}

// serverURL is the address with the registry login put back in, the only way
// incusd can be handed one. incusd logs this URL, so build it here and nowhere
// else.
func (s *imageSource) serverURL() string {
	addr := s.info.Addrs[0]

	if s.info.Username == "" && s.info.Password == "" {
		return addr
	}

	parsed, err := url.Parse(addr)
	if err != nil {
		return addr
	}

	parsed.User = url.UserPassword(s.info.Username, s.info.Password)

	return parsed.String()
}

// ImageState is what the last fetch read back from Incus.
type ImageState struct {
	// IncusAlias is nil until the image is ensured.
	IncusAlias *incusApi.ImageAliasesEntry
	ETag       string

	// OCI metadata extracted from the image (empty/0 for native Incus images).
	UID uint64
	GID uint64

	// OCIUser is the image's own USER verbatim, which UID/GID cannot hold when
	// it names a user instead of numbering one.
	OCIUser string

	Entrypoint string
	Cmd        string
	Cwd        string
	Volumes    []string

	// Size is the total image size in bytes as reported by the source server,
	// resolved best-effort before a download. 0 when unknown.
	Size int64
}

// newImage returns an existing Image resource or creates a new one.
// The name should be a Docker-style image reference or native Incus reference (remote:image).
func newImage(c *Client, name string, configGetter Config) (*Image, error) {
	if configGetter == nil {
		return nil, ErrUnknownConfig.WithKindName(KindImage, name)
	}

	cConfig, ok := configGetter.GetConfig().(*ImageConfig)
	if !ok {
		return nil, ErrUnknownConfig.WithKindName(KindImage, name)
	}
	configCopy := *cConfig
	config := &configCopy

	var remote, image, incusName string

	// Try to parse as native Incus format first: "remote:image/path"
	// This takes precedence if CliConfig is provided and remote exists in the config.
	if strings.Contains(name, ":") {
		parts := strings.SplitN(name, ":", 2)
		remoteName := parts[0]

		if _, ok := c.globalClient.cliConfig.Remotes[remoteName]; ok {
			remote = remoteName
			image = parts[1]
			incusName = name
		}
	}

	// If not resolved as native, try Docker/OCI reference
	if incusName == "" {
		ref, err := reference.ParseDockerRef(name)
		if err != nil {
			return nil, ErrInvalidFormat.WithKindName(KindImage, name).Wrap(err)
		}

		originalDomain := reference.Domain(ref)
		remote = originalDomain
		if remote == "localhost" {
			// Handle podman style "localhost" images.
			remote = "local"
		}

		image, _ = strings.CutPrefix(ref.String(), originalDomain+"/")
		incusName = remote + "/" + image
	}

	img := &Image{
		BaseResource: NewBaseResource(KindImage, name, PriorityImage),
		client:       c,
		incusName:    incusName,
		Config:       *config,
		remote:       remote,
		image:        image,
	}

	// Every accessor dereferences this, so it must never be nil.
	img.state.Store(&ImageState{})

	return img, nil
}

// String is for debugging.
func (r *Image) String() string {
	return fmt.Sprintf("%v(%v)", r.kind, r.incusName)
}

// IncusName returns the image alias name used in Incus.
func (r *Image) IncusName() string {
	return r.incusName
}

// IsEnsured returns true if the image has been fetched/copied to cache.
func (r *Image) IsEnsured() bool {
	return r.State().IncusAlias != nil
}

// State returns the image state as of the last fetch. It is replaced whole,
// never written into, so the result stays consistent for as long as it is held.
func (r *Image) State() *ImageState {
	return r.state.Load()
}

// clearState forgets the fetched image.
func (r *Image) clearState() {
	r.state.Store(&ImageState{})
}

// updateState swaps in a copy carrying f's edits. The actions are serialized by
// mu, so the read-modify-write cannot lose a concurrent one.
func (r *Image) updateState(f func(s *ImageState)) {
	next := *r.state.Load()
	f(&next)
	r.state.Store(&next)
}

// AddService records a compose service that uses this image. One image often
// serves several services, and Client.Resource hands them all the same object.
func (r *Image) AddService(name string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.Config.Services = append(r.Config.Services, name)
}

// Created returns true if the image was created during the last Ensure call.
func (r *Image) Created() bool {
	return r.created
}

// Status returns the image status: "Unknown" or "Cached".
func (r *Image) Status() string {
	if r.IsEnsured() {
		return "Cached"
	}
	return "Unknown"
}

// Remote returns the image remote.
func (r *Image) Remote() string {
	return r.remote
}

// Size returns the total image size in bytes as reported by the source server,
// or 0 when unknown. It is resolved best-effort before a download starts.
func (r *Image) Size() int64 {
	return r.State().Size
}

// NativeIncus returns true if this is a native Incus image.
func (r *Image) NativeIncus() bool {
	return r.nativeIncus
}

// Ensure retrieves an existing image from cache or copies it if Create option is set.
// With the Pull option, a cached image is refreshed from its source registry.
// When ImageConfig.Build is set the image is built locally via podman/docker.
func (r *Image) Ensure(ctx context.Context, opts ...Option) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	args := NewOptions(opts...)

	_, err := r.client.Connection()
	if err != nil {
		return err
	}

	err = r.setupCacheAndSource()
	if err != nil {
		_ = r.client.hookBefore(ctx, ActionEnsure, r, args, nil)

		return r.client.hookAfter(ctx, ActionEnsure, r, args, err)
	}

	if r.Config.Build != nil {
		return r.ensureBuild(ctx, args)
	}

	// Refreshing takes the same path, create() drops the stale copy under the lock.
	if args.Pull == PullAlways {
		_ = r.get(ctx)

		err = r.client.hookBefore(ctx, ActionEnsure, r, args, nil)
		if err != nil {
			return err
		}

		err = r.create(ctx, args)

		return r.client.hookAfter(ctx, ActionEnsure, r, args, err)
	}

	err = r.client.hookBefore(ctx, ActionEnsure, r, args, nil)
	if err != nil {
		return err
	}

	// Try to get existing image
	err = r.get(ctx)
	if err == nil {
		err = r.client.hookAfter(ctx, ActionEnsure, r, args, err)

		return err
	}

	if !args.Create || !errors.Is(err, ErrNotFound) {
		err = r.client.hookAfter(ctx, ActionEnsure, r, args, err)

		return err
	}

	err = r.create(ctx, args)
	err = r.client.hookAfter(ctx, ActionEnsure, r, args, err)

	return err
}

func (r *Image) setupCacheAndSource() error {
	// Resolve cache: CacheClient > CacheProject > default imageCache which might be nil
	if r.cache == nil {
		if r.Config.CacheClient != nil {
			r.cache = r.Config.CacheClient
		} else if r.Config.CacheProject != "" {
			cacheClient, err := r.client.globalClient.EnsureProject(r.Config.CacheProject, EnsureProjectWithCreate())
			if err != nil {
				return fmt.Errorf("ensuring cache project %s: %w", r.Config.CacheProject, err)
			}
			r.cache = cacheClient
		} else {
			r.cache = r.client.imageCache
		}
	}

	// Resolve source image server
	if r.source == nil {
		source, err := r.resolveSource()
		if err != nil {
			r.client.LogWarn("Failed to get an image server for", "resource", r, "error", err)
		} else {
			r.source = source
			r.nativeIncus = source.info.Protocol == "incus"
		}
	}

	return nil
}

// resolveSource reads where this image's remote lives.
func (r *Image) resolveSource() (*imageSource, error) {
	config := r.client.globalClient.cliConfig

	_, known := config.Remotes[r.remote]
	if !known {
		url, ok := WellKnownRegistries[r.remote]
		if !ok {
			return nil, ErrImageSource.WithText("no remote named " + r.remote)
		}

		// Nowhere to configure a credentials helper, so this one is anonymous.
		return &imageSource{info: &iclient.ConfigRemoteInfo{
			Name:     r.remote,
			Addrs:    []string{url},
			Protocol: "oci",
			Public:   true,
		}}, nil
	}

	info, err := config.RemoteInfos(r.remote)
	if err != nil {
		return nil, err
	}

	source := &imageSource{info: info}

	if info.Protocol != "incus" {
		return source, nil
	}

	source.conn, err = iclient.NewConnection(info)
	if err != nil {
		return nil, err
	}

	return source, nil
}

// pullRequest is the copy incusd performs on its own; we only say where the source is.
func (r *Image) pullRequest(fingerprint string) incusApi.ImagesPost {
	return incusApi.ImagesPost{
		Aliases: []incusApi.ImageAlias{{Name: r.incusName}},
		Source: &incusApi.ImagesPostSource{
			ImageSource: incusApi.ImageSource{
				Server:      r.source.serverURL(),
				Protocol:    r.source.info.Protocol,
				Certificate: r.source.info.ServerCert,
			},
			Type:        "image",
			Mode:        "pull",
			Fingerprint: fingerprint,
			Project:     r.source.info.Project,
		},
	}
}

// sourceFingerprint is what to ask the source for - the reference itself, unless a
// native remote lets us resolve it.
func (r *Image) sourceFingerprint(ctx context.Context) (string, error) {
	if !r.NativeIncus() {
		return r.image, nil
	}

	alias, _, err := r.source.conn.GetImageAlias(ctx, r.image, nil)
	if err != nil {
		return "", ErrNotFound.WithText("image not found on source").Wrap(err)
	}

	image, _, err := r.source.conn.GetImage(ctx, alias.Target, nil)
	if err != nil {
		return "", ErrNotFound.WithText("resolved alias not found on source").Wrap(err)
	}

	r.updateState(func(s *ImageState) { s.Size = image.Size })

	return image.Fingerprint, nil
}

func (r *Image) get(ctx context.Context) error {
	conn, err := r.client.Connection()
	if err != nil {
		return err
	}

	// Check if image alias exists in cache
	alias, eTag, err := conn.GetImageAlias(ctx, r.incusName, nil)
	if err != nil {
		r.clearState()
		return ErrNotFound.Wrap(err)
	}

	next := &ImageState{IncusAlias: alias, ETag: eTag}

	img, _, err := conn.GetImage(ctx, alias.Target, nil)
	if err == nil {
		next.Size = img.Size
		ociReadProperties(next, img.Properties)
	}

	r.state.Store(next)

	return nil
}

// deleteCached deletes the image from the cache and the project.
func (r *Image) deleteCached(ctx context.Context, args Options) error {
	err := r.client.hookAfter(ctx, ActionEnsure, r, args, nil)
	if err != nil {
		return err
	}

	err = r.client.hookBefore(ctx, ActionDelete, r, args, nil)
	if err != nil {
		return err
	}

	// Only a native remote can be asked whether it still holds the same image.
	var sourceAlias *incusApi.ImageAliasesEntry
	if r.source != nil && r.source.conn != nil {
		sourceAlias, _, err = r.source.conn.GetImageAlias(ctx, r.image, nil)

		// Image not found on the source.
		if err != nil && r.State().IncusAlias == nil {
			r.client.LogDebug("Image not found on the source", "resource", r)
			return r.client.hookAfter(ctx, ActionDelete, r, args, nil)
		}
	}

	if r.cache != nil {
		cacheAlias, _, err := r.cache.incus.GetImageAlias(ctx, r.incusName, nil)
		if err == nil && (sourceAlias == nil || sourceAlias.Target != cacheAlias.Target) {
			r.client.LogDebug("Deleting from cache", "resource", r)
			op, err := r.cache.incus.DeleteImage(ctx, cacheAlias.Target)

			// On the cache the error is ignored.
			if err = r.client.hookOperation(ctx, ActionDelete, r, args, op, err); err != nil {
				r.client.LogDebug("Deleting stale cache image for refresh", "error", err)
			}
		} else {
			r.client.LogDebug("Image not found on the cache or it is recent", "resource", r)
		}
	}

	err = r.get(ctx)
	if err != nil {
		// Project doesn't have the image, ignore this.
		return r.client.hookAfter(ctx, ActionDelete, r, args, nil)
	}

	r.client.LogDebug("Deleting from project", "resource", r)
	conn, err := r.client.Connection()
	if err != nil {
		return err
	}

	op, err := conn.DeleteImage(ctx, r.State().IncusAlias.Target)
	if err = r.client.hookOperation(ctx, ActionEnsure, r, args, op, err); err != nil {
		r.client.LogDebug("deleting stale project image for refresh", "error", err)
		return r.client.hookAfter(ctx, ActionDelete, r, args, err)
	}

	err = r.client.hookAfter(ctx, ActionDelete, r, args, err)
	if err != nil {
		return err
	}

	err = r.client.hookBefore(ctx, ActionEnsure, r, args, nil)
	if err != nil {
		return err
	}

	return nil
}

func (r *Image) copyToCache(ctx context.Context, args Options) (*incusApi.ImageAliasesEntry, error) {
	if r.source == nil {
		return nil, ErrImageSource.WithText("not configured")
	}

	fingerprint, err := r.sourceFingerprint(ctx)
	if err != nil {
		return nil, err
	}

	op, err := r.cache.incus.CreateImage(ctx, r.pullRequest(fingerprint), nil)
	if err == nil {
		err = r.client.hookOperation(ctx, ActionEnsure, r, args, op, nil)
	}

	if err != nil {
		r.client.LogWarn("Copy to cache failed", "resource", r, "error", err)

		return nil, ErrNotFound.Wrap(err)
	}

	// Retry fetch for up to 5 minutes, this is required because multiple workers may try to copy it.
	// A fixed delay, the default backoff would turn these ten attempts into hours.
	cacheAlias, err := retry.NewWithData[*incusApi.ImageAliasesEntry](
		retry.Context(ctx),
		retry.Attempts(10),
		retry.Delay(30*time.Second),
		retry.DelayType(retry.FixedDelay),
		retry.LastErrorOnly(true),
	).Do(func() (*incusApi.ImageAliasesEntry, error) {
		alias, _, err := r.cache.incus.GetImageAlias(ctx, r.incusName, nil)
		return alias, err
	})
	if err != nil {
		return nil, ErrNotFound.WithText("on cache after copy").Wrap(err)
	}

	// Extract oci informations with a temporary instance.
	err = r.ociStoreConfig(ctx, r.cache.incus, cacheAlias.Target, nil)
	if err != nil {
		return nil, ErrCreate.WithText("extracting OCI config from the image").Wrap(err)
	}

	return cacheAlias, nil
}

// copyToProject is hop B: copy the cached image into the active project,
// carrying the OCI properties extracted when it landed in the cache.
func (r *Image) copyToProject(ctx context.Context, args Options, cacheAlias *incusApi.ImageAliasesEntry) error {
	img, _, err := r.cache.incus.GetImage(ctx, cacheAlias.Target, nil)
	if err != nil {
		return ErrCreate.WithText("cannot resolve the image from cache after copy")
	}

	r.updateState(func(s *ImageState) {
		s.Size = img.Size
		ociReadProperties(s, img.Properties)
	})

	conn, err := r.client.Connection()
	if err != nil {
		return err
	}

	op, err := conn.CopyImage(ctx, r.cache.incus, cacheAlias.Target, &iclient.ImageCopyArgs{
		Aliases: []incusApi.ImageAlias{{Name: r.incusName}},
		Mode:    "pull",
	})

	err = r.client.hookOperation(ctx, ActionEnsure, r, args, op, err)
	if err != nil {
		return ErrCreate.WithText("project image").Wrap(err)
	}

	return r.get(ctx)
}

// lockStore takes the per-alias lock in the cache, returning a release func.
// Without a cache the store is the project, which nobody else writes to, so
// there is nothing to serialize against.
func (r *Image) lockStore(ctx context.Context) (func(), error) {
	if r.cache == nil {
		return func() {}, nil
	}

	name := r.Config.LockVolume
	if name == "" {
		name = DefaultLockVolume
	}

	// The lock volume lives in the cache project, not the one being built into,
	// so every failure here names it.
	where := fmt.Sprintf("lock volume %q in project %q", name, r.cache.project)

	res, err := r.cache.Resource(KindStorageVolume, name, &StorageVolumeConfig{})
	if err != nil {
		return nil, ErrCreate.WithText("getting the image " + where).Wrap(err)
	}

	vol, ok := res.(*StorageVolume)
	if !ok {
		return nil, ErrUnknownResource.WithText(name)
	}

	err = RunAction(ctx, vol, ActionEnsure, OptionCreate())
	if err != nil {
		return nil, ErrCreate.WithText("ensuring the image " + where).Wrap(err)
	}

	sc, err := vol.SFTP(ctx)
	if err != nil {
		return nil, ErrCreate.WithText("connecting to the image " + where).Wrap(err)
	}

	lock, err := vol.Lock(ctx, sc, fmt.Sprintf("%x", sha256.Sum256([]byte(r.incusName))), imageLockStale)
	if err != nil {
		r.client.WarnError(sc.Close, "Failed to close the image lock connection")
		return nil, ErrCreate.WithText("taking the image lock in the " + where).Wrap(err)
	}

	return func() {
		r.client.WarnError(lock.Unlock, "Failed to release the image lock")
		r.client.WarnError(sc.Close, "Failed to close the image lock connection")
	}, nil
}

// create materializes the image. With a cache that is hop A under the per-alias
// lock, then hop B outside it; without one the source is copied straight into
// the project.
func (r *Image) create(ctx context.Context, args Options) error {
	if r.cache == nil {
		return r.createDirect(ctx, args)
	}

	release, err := r.lockStore(ctx)
	if err != nil {
		return err
	}

	cacheAlias, err := r.materialize(ctx, args)
	release()
	if err != nil {
		return err
	}

	return r.copyToProject(ctx, args, cacheAlias)
}

// materialize is hop A: make sure the cache holds the alias, returning it.
func (r *Image) materialize(ctx context.Context, args Options) (*incusApi.ImageAliasesEntry, error) {
	if args.Pull == PullAlways {
		err := r.deleteCached(ctx, args)
		if err != nil {
			return nil, err
		}

		r.clearState()
	}

	cacheAlias, _, err := r.cache.incus.GetImageAlias(ctx, r.incusName, nil)
	if err == nil {
		// An entry cached before the split still concatenates entrypoint and
		// command, so reading the config again upgrades it where it lies.
		err = r.ociStoreConfig(ctx, r.cache.incus, cacheAlias.Target, nil)
		if err != nil {
			return nil, err
		}

		return cacheAlias, nil
	}

	if args.Pull == PullNever {
		return nil, ErrNotFound.WithText("pull policy is never")
	}

	cacheAlias, cacheErr := r.copyToCache(ctx, args)
	if cacheErr != nil && errors.Is(cacheErr, ErrNotFound) {
		// A concurrent copy may still have published the alias.
		cacheAlias, _, err = r.cache.incus.GetImageAlias(ctx, r.incusName, nil)
		if err != nil {
			return nil, ErrNotFound.WithText("on cache and source").Wrap(cacheErr)
		}

		// Extract oci informations with a temporary instance.
		err = r.ociStoreConfig(ctx, r.cache.incus, cacheAlias.Target, nil)
		if err != nil {
			return nil, ErrCreate.WithText("extracting OCI config from the image").Wrap(err)
		}
	} else if cacheErr != nil {
		return nil, cacheErr
	}

	return cacheAlias, nil
}

// createDirect copies the source straight into the project, used when no cache
// is configured.
func (r *Image) createDirect(ctx context.Context, args Options) error {
	// Without a cache the project is the store, and Ensure already missed it.
	if args.Pull == PullNever {
		return ErrNotFound.WithText("pull policy is never")
	}

	if r.source == nil {
		return ErrImageSource.WithText("not configured")
	}

	fingerprint, err := r.sourceFingerprint(ctx)
	if err != nil {
		return err
	}

	conn, err := r.client.Connection()
	if err != nil {
		return err
	}

	op, err := conn.CreateImage(ctx, r.pullRequest(fingerprint), nil)
	if err != nil {
		r.client.LogWarn("Creating a copy operation failed", "resource", r, "error", err)
	} else {
		// Wait for copy to complete
		err = r.client.hookOperation(ctx, ActionEnsure, r, args, op, err)
		if err != nil {
			r.client.LogWarn("Copy to project failed", "resource", r, "error", err)
		}
	}

	targetAlias, _, err := conn.GetImageAlias(ctx, r.incusName, nil)
	if err != nil {
		return ErrNotFound.WithText("on project after copy").Wrap(err)
	}

	// Extract oci informations with a temporary instance.
	err = r.ociStoreConfig(ctx, conn, targetAlias.Target, nil)
	if err != nil {
		return ErrCreate.WithText("extracting OCI config from the image").Wrap(err)
	}

	return r.get(ctx)
}

// ensureBuild handles the Ensure lifecycle for locally-built images. It does not
// touch the remote-pull machinery (source image server, cache project).
func (r *Image) ensureBuild(ctx context.Context, args Options) error {
	if err := r.client.hookBefore(ctx, ActionEnsure, r, args, nil); err != nil {
		return err
	}

	err := r.get(ctx)
	if err == nil && args.Build.Mode != BuildForce {
		return r.client.hookAfter(ctx, ActionEnsure, r, args, nil)
	}

	if err != nil && args.Build.Mode == BuildNever {
		return r.client.hookAfter(ctx, ActionEnsure, r, args, errors.New("image is missing and building is disabled"))
	}

	if err != nil && !args.Create {
		return r.client.hookAfter(ctx, ActionEnsure, r, args,
			ErrCreate.WithText("the built image is missing and creating it was not requested").Wrap(err))
	}

	r.client.LogDebug("Taking the image build lock", "resource", r)

	release, err := r.lockStore(ctx)
	if err != nil {
		return r.client.hookAfter(ctx, ActionEnsure, r, args, err)
	}
	defer release()

	// Hop A: the store already holds it, so copy rather than rebuild.
	if r.cache != nil && args.Build.Mode != BuildForce {
		cacheAlias, _, storeErr := r.cache.incus.GetImageAlias(ctx, r.incusName, nil)
		if storeErr == nil {
			r.client.LogDebug("Copying the built image from the cache", "resource", r)

			return r.client.hookAfter(ctx, ActionEnsure, r, args, r.copyToProject(ctx, args, cacheAlias))
		}
	}

	r.clearState()

	r.client.LogDebug("Building image", "resource", r, "context", r.Config.Build.Context)

	err = r.buildImage(ctx, r.client, args)

	return r.client.hookAfter(ctx, ActionEnsure, r, args, err)
}

// buildImage shells out to the detected container builder, imports the rootfs
// into Incus as a split (metadata + rootfs) image, and records the alias.
func (r *Image) buildImage(ctx context.Context, c *Client, args Options) error {
	conn, err := r.client.Connection()
	if err != nil {
		return err
	}

	server, _, err := conn.GetServer(ctx)
	if err != nil {
		return ErrCreate.WithText("getting Incus server info").Wrap(err)
	}
	if len(server.Environment.Architectures) == 0 {
		return ErrCreate.WithText("Incus server has no supported architectures")
	}

	buildCfg := *r.Config.Build
	incusArch := server.Environment.Architectures[0]
	if buildCfg.Platform != "" {
		var ok bool
		incusArch, ok = platformToIncusArch(buildCfg.Platform, server.Environment.Architectures)
		if !ok {
			return ErrCreate.WithText("unsupported build platform " + buildCfg.Platform)
		}
	} else {
		platform, ok := incusArchToPlatform(incusArch)
		if !ok {
			return ErrCreate.WithText("unsupported Incus architecture " + incusArch)
		}
		buildCfg.Platform = platform
	}

	builder, err := buildDetectBuilder(args.Build.PreferredBuilder)
	if err != nil {
		return ErrCreate.WithText("no container builder").Wrap(err)
	}

	rootfs, configJSON, ociCfg, err := buildRootfs(ctx, r.client, builder, &buildCfg, c.Global().Stdout(), c.Global().Stderr())
	if err != nil {
		return ErrCreate.WithText("building container image").Wrap(err)
	}
	defer r.client.WarnError(rootfs.Close, "Failure during close")

	meta, err := buildMetadataTar(r.incusName, incusArch, configJSON)
	if err != nil {
		return ErrCreate.WithText("building image metadata").Wrap(err)
	}

	// Without a usable cache the project is the import target, and hop B is a no-op.
	cached := r.cache != nil && !buildCfg.NoCache
	target := conn
	targetName := "project"
	if cached {
		target = r.cache.incus
		targetName = "cache"
	}

	stale, _, err := target.GetImageAlias(ctx, r.incusName, nil)
	if err == nil {
		err = deleteImage(ctx, target, stale.Target)
		if err != nil {
			return ErrCreate.WithText("while removing the image from the " + targetName).Wrap(err)
		}
	}

	op, err := target.CreateImage(ctx, incusApi.ImagesPost{
		Aliases: []incusApi.ImageAlias{{Name: r.incusName}},
	}, &iclient.ImageCreateArgs{
		MetaFile:   meta,
		MetaName:   "metadata.tar",
		RootfsFile: rootfs,
		RootfsName: "rootfs.tar",
	})
	err = r.client.hookOperation(ctx, ActionEnsure, r, args, op, err)
	if err != nil {
		return ErrCreate.WithText("importing built image on " + targetName).Wrap(err)
	}

	built, eTag, err := target.GetImageAlias(ctx, r.incusName, nil)
	if err != nil {
		return ErrCreate.WithText("fetching alias after build").Wrap(err)
	}

	// The builder already said what the entrypoint and command are, so a built
	// image needs no registry to read its own config back from.
	err = r.ociStoreConfig(ctx, target, built.Target, ociCfg)
	if err != nil {
		return err
	}

	r.updateState(func(s *ImageState) {
		s.IncusAlias = built
		s.ETag = eTag
	})
	r.created = true

	if cached {
		projectAlias, _, aliasErr := conn.GetImageAlias(ctx, r.incusName, nil)
		if aliasErr == nil {
			err = deleteImage(ctx, conn, projectAlias.Target)
			if err != nil {
				return ErrCreate.WithText("while removing the image from the project").Wrap(err)
			}
		}

		err = r.copyToProject(ctx, args, built)
		if err != nil {
			return err
		}

		r.created = true
	}

	r.client.LogInfo("Built image for", "image", r.incusName, "services", r.Config.Services)

	return nil
}

// deleteImage removes an image and waits for the removal to land.
func deleteImage(ctx context.Context, conn *iclient.Connection, fingerprint string) error {
	op, err := conn.DeleteImage(ctx, fingerprint)
	if err != nil {
		return err
	}

	_, err = iclient.WaitOperation(ctx, op)

	return err
}

// Delete removes the per-project copy of the image from the active project.
func (r *Image) Delete(ctx context.Context, opts ...Option) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if !r.IsEnsured() {
		r.clearState()

		r.client.resources.Remove(r)
		return nil
	}

	if err := r.get(ctx); err != nil {
		// Already gone server side
		r.client.resources.Remove(r)
		return err
	}

	options := NewOptions(opts...)

	if err := r.client.hookBefore(ctx, ActionDelete, r, options, nil); err != nil {
		r.clearState()

		r.client.resources.Remove(r)
		return err
	}

	// Resolve the per-project copy in the active project (not the cache). A
	// missing alias means nothing was copied here, so there is nothing to do.
	conn, err := r.client.Connection()
	if err != nil {
		return err
	}

	alias, _, err := conn.GetImageAlias(ctx, r.incusName, nil)
	if err != nil || alias == nil {
		r.clearState()

		r.client.resources.Remove(r)

		return r.client.hookAfter(ctx, ActionDelete, r, options, err)
	}

	op, err := conn.DeleteImage(ctx, alias.Target)

	err = r.client.hookOperation(ctx, ActionDelete, r, options, op, err)
	r.clearState()

	r.client.resources.Remove(r)
	return r.client.hookAfter(ctx, ActionDelete, r, options, err)
}

var (
	_ Resource   = (*Image)(nil)
	_ EnsureAble = (*Image)(nil)
	_ DeleteAble = (*Image)(nil)
)
