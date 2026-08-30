package client

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net/url"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/avast/retry-go/v5"
	"github.com/distribution/reference"
	incusApi "github.com/lxc/incus/v7/shared/api"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
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

	// Platform is the OCI platform the image is wanted for, for example
	// linux/arm64. Empty means the architecture of the server we are connected
	// to. Build.Platform takes precedence for a built image.
	Platform string

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

	// arch is the Incus architecture name this image is wanted for, and
	// platform its registry spelling. Resolved together, never separately.
	arch     string
	platform string

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

	// SourceFingerprint is what the source held at the last OptionResolveSource,
	// in the shape Incus fingerprints by. Empty when nothing resolved it, and
	// set means the OCI fields above came from the source rather than the image.
	SourceFingerprint string
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

// resolvePlatform reads a platform request, answering the cache alias key and
// the Incus architecture behind it.
func (r *Image) resolvePlatform(platform string) (string, string, error) {
	key, arch, ok := platformKey(platform)
	if ok {
		return key, arch, nil
	}

	if r.Config.Build != nil {
		return "", "", ErrNoPlatform.WithText("unsupported build platform " + platform)
	}

	return "", "", ErrNoPlatform.WithText("unsupported platform " + platform)
}

// requestedPlatform is the platform this image was configured for. A build
// states it under build.platforms, which compose-go keeps apart from
// service.platform, and the builder's is the one that decides the bytes.
func (r *Image) requestedPlatform() string {
	if r.Config.Build != nil && r.Config.Build.Platform != "" {
		return r.Config.Build.Platform
	}

	return r.Config.Platform
}

// cacheAlias is the alias the store holds this image under. The platform is
// part of the key because a stored alias resolves to one fingerprint, while the
// store is shared across projects and, on a cluster, across architectures.
func (r *Image) cacheAlias() string {
	return r.incusName + "/" + r.platform
}

// resolveArch fixes the architecture this image is wanted for. An empty
// platform takes the connected server's own, which is the one architecture we
// know is runnable without asking where the instance will land.
func (r *Image) resolveArch(ctx context.Context) error {
	if r.arch != "" {
		return nil
	}

	platform := r.requestedPlatform()
	if platform != "" {
		key, arch, err := r.resolvePlatform(platform)
		if err != nil {
			return err
		}

		r.arch, r.platform = arch, key

		return nil
	}

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

	r.arch, r.platform = server.Environment.Architectures[0], archPlatform(server.Environment.Architectures[0])

	return nil
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

// clearState forgets the fetch, not the content: the OCI config and the
// resolved source outlive an image a refresh deletes in order to re-pull it.
func (r *Image) clearState() {
	r.updateState(func(s *ImageState) {
		s.IncusAlias = nil
		s.ETag = ""
		s.Size = 0
	})
}

// updateState swaps in a copy carrying f's edits. The actions are serialized by
// mu, so the read-modify-write cannot lose a concurrent one.
func (r *Image) updateState(f func(s *ImageState)) {
	next := *r.state.Load()
	f(&next)
	r.state.Store(&next)
}

// AddService records a compose service that uses this image. One image often
// serves several services, and Client.Resource hands them all the same object -
// which two services wanting different architectures of it cannot share.
func (r *Image) AddService(name string, platform string) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	want := r.requestedPlatform()
	_, _, valid := platformKey(platform)

	switch {
	// The conflict is between services, so the first one has nobody to
	// disagree with - its own platform is already r's.
	case len(r.Config.Services) == 0 || platform == "":
		// Nothing to settle: no preference.
	case want == "" || !valid:
		// The earlier services had no preference, or this one names something
		// resolveArch has to reject. Either way this value decides: services
		// are loaded in map order, so an unusable one that did not take hold
		// here would be reported or silently dropped run by run.
		r.Config.Platform = platform
	case samePlatform(want, platform):
		// The same one twice.
	default:
		return ErrPlatformConflict.WithText(fmt.Sprintf(
			"%s as %q and as %q", r.incusName, want, platform))
	}

	r.Config.Services = append(r.Config.Services, name)

	return nil
}

// samePlatform reports whether two platform requests are known to name one
// manifest. One that does not parse answers true, because it is not a
// disagreement: resolveArch reports it, at a point where down can still tear
// the project down.
func samePlatform(a string, b string) bool {
	keyA, _, okA := platformKey(a)
	keyB, _, okB := platformKey(b)

	if !okA || !okB {
		return true
	}

	return keyA == keyB
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
// When ImageConfig.Build is set the image is built locally via podman/docker.
func (r *Image) Ensure(ctx context.Context, opts ...Option) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	args := NewOptions(opts...)

	_, err := r.client.Connection()
	if err != nil {
		return err
	}

	err = r.setupCacheAndSource(ctx)
	if err != nil {
		_ = r.client.hookBefore(ctx, ActionEnsure, r, args, nil)

		return r.client.hookAfter(ctx, ActionEnsure, r, args, err)
	}

	if r.Config.Build != nil {
		return r.ensureBuild(ctx, args)
	}

	err = r.client.hookBefore(ctx, ActionEnsure, r, args, nil)
	if err != nil {
		return err
	}

	// Try to get existing image
	err = r.get(ctx)
	if err == nil {
		if args.ResolveSource {
			r.readSource(ctx)
		}

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

func (r *Image) setupCacheAndSource(ctx context.Context) error {
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

	err := r.resolveArch(ctx)
	if err != nil {
		return err
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

// pullRequest is the copy incusd performs on its own; we only say where the
// source is and what to alias the result as.
func (r *Image) pullRequest(alias string, fingerprint string) incusApi.ImagesPost {
	return incusApi.ImagesPost{
		Aliases: []incusApi.ImageAlias{{Name: alias}},
		Source: &incusApi.ImagesPostSource{
			ImageSource: incusApi.ImageSource{
				Server:      r.source.serverURL(),
				Protocol:    r.source.info.Protocol,
				Certificate: r.source.info.ServerCert,
			},
			Type:        "image",
			Mode:        "pull",
			Fingerprint: fingerprint,
		},
	}
}

// pullSource is what to ask the source for, plus the OCI config when reading
// the registry answered that too.
//
// An OCI reference is pinned to the manifest built for this image's platform.
// The registry serves one per architecture behind a single tag, and incusd
// resolves an unpinned reference with skopeo on whichever cluster member
// handles the pull - which is how the store ends up holding one member's
// architecture for everybody.
func (r *Image) pullSource(ctx context.Context) (string, *ocispec.ImageConfig, error) {
	if r.NativeIncus() {
		fingerprint, err := r.sourceFingerprint(ctx)

		return fingerprint, nil, err
	}

	if r.source.info.Protocol != "oci" {
		return r.image, nil, nil
	}

	digest, fingerprint, config, err := r.ociResolveSource(ctx, r.platform)
	if err != nil {
		return "", nil, err
	}

	r.updateState(func(s *ImageState) {
		s.SourceFingerprint = fingerprint
		ociStateFromConfig(s, config)
	})

	name, _, _ := strings.Cut(r.image, "@")

	return name + "@" + digest, config, nil
}

// verifyArch rejects a stored image whose architecture is not the one asked
// for, and deletes it. An OCI pull is pinned to an exact manifest and needs no
// check; every other source resolves the reference server-side, and answers
// with the architecture of the member that happened to serve the request.
//
// The check can only run once the image is stored, so leaving a rejected one
// behind would put it under the platform's alias for good: the store is shared
// across projects, and a hit there is answered without asking again.
func (r *Image) verifyArch(ctx context.Context, conn *iclient.Connection, project string, fingerprint string) error {
	if r.source != nil && r.source.info.Protocol == "oci" {
		return nil
	}

	img, _, err := conn.GetImage(ctx, project, fingerprint, nil)
	if err != nil {
		return ErrNotFound.WithText("the image just stored").Wrap(err)
	}

	if img.Architecture == r.arch {
		return nil
	}

	rejected := ErrNoPlatform.WithText(fmt.Sprintf(
		"%s resolved to %s, not %s; only an OCI source can be asked for one architecture",
		r.image, img.Architecture, r.arch))

	err = deleteImage(ctx, conn, project, fingerprint)
	if err != nil {
		return errors.Join(rejected, ErrDelete.WithText("the image of the wrong architecture").Wrap(err))
	}

	return rejected
}

// sourceFingerprint is what to ask the source for - the reference itself, unless a
// native remote lets us resolve it.
func (r *Image) sourceFingerprint(ctx context.Context) (string, error) {
	if !r.NativeIncus() {
		return r.image, nil
	}

	alias, _, err := r.source.conn.GetImageAlias(ctx, "", r.image, nil)
	if err != nil {
		return "", ErrNotFound.WithText("image not found on source").Wrap(err)
	}

	image, _, err := r.source.conn.GetImage(ctx, "", alias.Target, nil)
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
	alias, eTag, err := conn.GetImageAlias(ctx, r.client.incusProject, r.incusName, nil)
	if err != nil {
		r.clearState()
		return ErrNotFound.Wrap(err)
	}

	next := &ImageState{IncusAlias: alias, ETag: eTag}

	img, _, err := conn.GetImage(ctx, r.client.incusProject, alias.Target, nil)
	if err == nil {
		// The project alias carries no platform, so a service that changed its
		// platform finds the previous run's copy here. Answering it would make
		// the change a no-op until something else deleted the image.
		if !r.projectArchOK(img.Architecture) {
			r.clearState()

			return ErrNotFound.WithText(fmt.Sprintf("the project holds %s, not %s", img.Architecture, r.arch))
		}

		next.Size = img.Size
		ociReadProperties(next, img.Properties)
	}

	r.state.Store(next)

	return nil
}

// projectArchOK reports whether an image already in the project is the one this
// resource wants.
//
// incusd derives a pulled OCI image's architecture from the manifest's
// architecture field, which carries no variant, so every 32-bit arm image it
// pulls is recorded as armv6l whichever variant was pinned. Comparing against
// that is what the stored label can support: it catches amd64 against arm64,
// and cannot tell arm/v6 from arm/v7.
//
// A built image is labeled by buildMetadataTar instead, which writes r.arch
// unfolded, so the allowance is wrong for one and must not apply.
func (r *Image) projectArchOK(stored string) bool {
	if r.arch == "" || stored == "" {
		return true
	}

	want := r.arch
	if r.Config.Build == nil && r.source != nil && r.source.info.Protocol == "oci" {
		base, _, _ := strings.Cut(r.platform, "/")

		labeled, ok := platformArch(base)
		if ok {
			want = labeled
		}
	}

	return stored == want
}

// readSource records what the source holds now, so a caller can tell it apart
// from what is stored. A source it cannot reach leaves the state empty rather
// than failing: a client that cannot see the registry its server pulls from
// still has a usable image.
func (r *Image) readSource(ctx context.Context) {
	if r.source == nil {
		return
	}

	if r.NativeIncus() {
		fingerprint, err := r.sourceFingerprint(ctx)
		if err != nil {
			r.client.LogWarn("Cannot resolve the image on its remote", "resource", r, "error", err)

			return
		}

		r.updateState(func(s *ImageState) { s.SourceFingerprint = fingerprint })

		return
	}

	if r.source.info.Protocol != "oci" {
		return
	}

	// An index holds one manifest per architecture, and the one to compare
	// against is the one this image was pulled for.
	_, fingerprint, config, err := r.ociResolveSource(ctx, r.platform)
	if err != nil {
		r.client.LogWarn("Cannot resolve the image on its registry", "resource", r, "error", err)

		return
	}

	r.updateState(func(s *ImageState) {
		s.SourceFingerprint = fingerprint
		ociStateFromConfig(s, config)
	})
}

func (r *Image) copyToCache(ctx context.Context, args Options) (*incusApi.ImageAliasesEntry, error) {
	if r.source == nil {
		return nil, ErrImageSource.WithText("not configured")
	}

	fingerprint, ociCfg, err := r.pullSource(ctx)
	if err != nil {
		if errors.Is(err, ErrNoPlatform) {
			return nil, err
		}

		// Anything else is a store miss like any other, so materialize still
		// gets to check whether a concurrent writer filled it.
		return nil, ErrNotFound.Wrap(err)
	}

	op, err := r.cache.incus.CreateImage(ctx, r.cache.incusProject, r.pullRequest(r.cacheAlias(), fingerprint), nil)
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
		alias, _, err := r.cache.incus.GetImageAlias(ctx, r.cache.incusProject, r.cacheAlias(), nil)
		return alias, err
	})
	if err != nil {
		return nil, ErrNotFound.WithText("on cache after copy").Wrap(err)
	}

	err = r.verifyArch(ctx, r.cache.incus, r.cache.incusProject, cacheAlias.Target)
	if err != nil {
		return nil, err
	}

	// Extract oci informations with a temporary instance.
	err = r.ociStoreConfig(ctx, r.cache.incus, r.cache.incusProject, cacheAlias.Target, ociCfg)
	if err != nil {
		return nil, ErrCreate.WithText("extracting OCI config from the image").Wrap(err)
	}

	return cacheAlias, nil
}

// copyToProject is hop B: copy the cached image into the active project,
// carrying the OCI properties extracted when it landed in the cache.
func (r *Image) copyToProject(ctx context.Context, args Options, cacheAlias *incusApi.ImageAliasesEntry) error {
	img, _, err := r.cache.incus.GetImage(ctx, r.cache.incusProject, cacheAlias.Target, nil)
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

	// The project alias carries no platform, so the copy has nowhere to land
	// while the previous platform's image still holds the name.
	stale, _, err := conn.GetImageAlias(ctx, r.client.incusProject, r.incusName, nil)
	if err == nil && stale.Target != cacheAlias.Target {
		err = deleteImage(ctx, conn, r.client.incusProject, stale.Target)
		if err != nil {
			return ErrCreate.WithText("while removing the previous project image").Wrap(err)
		}
	}

	op, err := conn.CopyImage(ctx, r.client.incusProject, r.cache.incus, r.cache.incusProject, cacheAlias.Target, &iclient.ImageCopyArgs{
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

	lock, err := vol.Lock(ctx, sc, fmt.Sprintf("%x", sha256.Sum256([]byte(r.cacheAlias()))), imageLockStale)
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
	cacheAlias, _, err := r.cache.incus.GetImageAlias(ctx, r.cache.incusProject, r.cacheAlias(), nil)
	if err == nil {
		// An entry cached before the split still concatenates entrypoint and
		// command, so reading the config again upgrades it where it lies.
		err = r.ociStoreConfig(ctx, r.cache.incus, r.cache.incusProject, cacheAlias.Target, nil)
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
		cacheAlias, _, err = r.cache.incus.GetImageAlias(ctx, r.cache.incusProject, r.cacheAlias(), nil)
		if err != nil {
			return nil, ErrNotFound.WithText("on cache and source").Wrap(cacheErr)
		}

		// Extract oci informations with a temporary instance.
		err = r.ociStoreConfig(ctx, r.cache.incus, r.cache.incusProject, cacheAlias.Target, nil)
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

	fingerprint, ociCfg, err := r.pullSource(ctx)
	if err != nil {
		return err
	}

	conn, err := r.client.Connection()
	if err != nil {
		return err
	}

	op, err := conn.CreateImage(ctx, r.client.incusProject, r.pullRequest(r.incusName, fingerprint), nil)
	if err != nil {
		r.client.LogWarn("Creating a copy operation failed", "resource", r, "error", err)
	} else {
		// Wait for copy to complete
		err = r.client.hookOperation(ctx, ActionEnsure, r, args, op, err)
		if err != nil {
			r.client.LogWarn("Copy to project failed", "resource", r, "error", err)
		}
	}

	targetAlias, _, err := conn.GetImageAlias(ctx, r.client.incusProject, r.incusName, nil)
	if err != nil {
		return ErrNotFound.WithText("on project after copy").Wrap(err)
	}

	err = r.verifyArch(ctx, conn, r.client.incusProject, targetAlias.Target)
	if err != nil {
		return err
	}

	// Extract oci informations with a temporary instance.
	err = r.ociStoreConfig(ctx, conn, r.client.incusProject, targetAlias.Target, ociCfg)
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

	// Hop A: the store already holds it, so copy rather than rebuild.
	if r.cache != nil && args.Build.Mode != BuildForce {
		cacheAlias, _, storeErr := r.cache.incus.GetImageAlias(ctx, r.cache.incusProject, r.cacheAlias(), nil)
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

	// The builder produces the rootfs here and this server has to import it, so
	// a build cannot target an architecture the server does not run.
	if !slices.Contains(server.Environment.Architectures, r.arch) {
		platform := r.Config.Build.Platform
		if platform == "" {
			platform = r.platform
		}

		return ErrNoPlatform.WithText("unsupported build platform " + platform)
	}

	// A platform the user wrote goes to the builder verbatim, since builders
	// take platforms Incus has no architecture for and the string is theirs to
	// get right. Only one we infer has to come from a set we know it accepts.
	buildCfg := *r.Config.Build
	if buildCfg.Platform == "" {
		if !slices.Contains(buildArchitectures, r.arch) {
			return ErrNoPlatform.WithText("no build platform for " + r.arch)
		}

		buildCfg.Platform = "linux/" + archPlatform(r.arch)
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

	meta, err := buildMetadataTar(r.incusName, r.arch, configJSON)
	if err != nil {
		return ErrCreate.WithText("building image metadata").Wrap(err)
	}

	// Without a usable cache the project is the import target, and hop B is a no-op.
	cached := r.cache != nil && !buildCfg.NoCache

	target := imageTarget{conn: conn, project: r.client.incusProject, alias: r.incusName, logName: "project"}
	if cached {
		target = imageTarget{conn: r.cache.incus, project: r.cache.incusProject, alias: r.cacheAlias(), logName: "cache"}
	}

	built, err := r.importBuilt(ctx, args, target, meta, rootfs, ociCfg)
	if err != nil {
		return err
	}

	if cached {
		err = r.copyToProject(ctx, args, built)
		if err != nil {
			return err
		}
	}

	r.client.LogInfo("Built image for", "image", r.incusName, "services", r.Config.Services)

	return nil
}

// imageTarget is where a built image is imported, and what to call that place in an error.
type imageTarget struct {
	conn    *iclient.Connection
	project string
	alias   string
	logName string
}

// importBuilt imports a built rootfs into the store, holding the per-alias lock
// for that alone: the builder ran unlocked, so a stuck build blocks nobody else.
func (r *Image) importBuilt(ctx context.Context, args Options, target imageTarget, meta io.Reader, rootfs io.Reader, ociCfg *ocispec.ImageConfig) (*incusApi.ImageAliasesEntry, error) {
	r.client.LogDebug("Taking the image build lock", "resource", r)

	release, err := r.lockStore(ctx)
	if err != nil {
		return nil, err
	}

	defer release()

	stale, _, err := target.conn.GetImageAlias(ctx, target.project, target.alias, nil)
	if err == nil {
		err = deleteImage(ctx, target.conn, target.project, stale.Target)
		if err != nil {
			return nil, ErrCreate.WithText("while removing the image from the " + target.logName).Wrap(err)
		}
	}

	op, err := target.conn.CreateImage(ctx, target.project, incusApi.ImagesPost{
		Aliases: []incusApi.ImageAlias{{Name: target.alias}},
	}, &iclient.ImageCreateArgs{
		MetaFile:   meta,
		MetaName:   "metadata.tar",
		RootfsFile: rootfs,
		RootfsName: "rootfs.tar",
	})
	err = r.client.hookOperation(ctx, ActionEnsure, r, args, op, err)
	if err != nil {
		return nil, ErrCreate.WithText("importing built image on " + target.logName).Wrap(err)
	}

	built, eTag, err := target.conn.GetImageAlias(ctx, target.project, target.alias, nil)
	if err != nil {
		return nil, ErrCreate.WithText("fetching alias after build").Wrap(err)
	}

	// The builder already said what the entrypoint and command are, so a built
	// image needs no registry to read its own config back from.
	err = r.ociStoreConfig(ctx, target.conn, target.project, built.Target, ociCfg)
	if err != nil {
		return nil, err
	}

	r.updateState(func(s *ImageState) {
		s.IncusAlias = built
		s.ETag = eTag
	})
	r.created = true

	return built, nil
}

// deleteImage removes an image and waits for the removal to land.
func deleteImage(ctx context.Context, conn *iclient.Connection, project string, fingerprint string) error {
	op, err := conn.DeleteImage(ctx, project, fingerprint)
	if err != nil {
		return err
	}

	_, err = iclient.WaitOperation(ctx, op)

	return err
}

// deleteCached removes the store's copy, under the per-alias lock so it cannot
// land inside a concurrent pull's hop A or a build's import.
func (r *Image) deleteCached(ctx context.Context) error {
	release, err := r.lockStore(ctx)
	if err != nil {
		return err
	}

	defer release()

	alias, _, err := r.cache.incus.GetImageAlias(ctx, r.cache.incusProject, r.cacheAlias(), nil)
	if err != nil {
		return nil
	}

	return deleteImage(ctx, r.cache.incus, r.cache.incusProject, alias.Target)
}

// Delete removes the per-project copy of the image from the active project, and
// with OptionCache the store's copy as well. The cache is only known once the
// image has been ensured, so an image this process never ensured keeps it.
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

	// The store hit is authoritative for the next Ensure, so an image dropped to
	// re-fetch it has to go from both copies or the stale one is simply copied back.
	if options.Cache && r.cache != nil {
		err := r.deleteCached(ctx)
		if err != nil {
			r.clearState()

			r.client.resources.Remove(r)

			return r.client.hookAfter(ctx, ActionDelete, r, options, err)
		}
	}

	// Resolve the per-project copy in the active project (not the cache). A
	// missing alias means nothing was copied here, so there is nothing to do.
	conn, err := r.client.Connection()
	if err != nil {
		return err
	}

	alias, _, err := conn.GetImageAlias(ctx, r.client.incusProject, r.incusName, nil)
	if err != nil || alias == nil {
		r.clearState()

		r.client.resources.Remove(r)

		return r.client.hookAfter(ctx, ActionDelete, r, options, err)
	}

	op, err := conn.DeleteImage(ctx, r.client.incusProject, alias.Target)

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
