package client

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/kballard/go-shellquote"
	incusApi "github.com/lxc/incus/v7/shared/api"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2/content"
	"oras.land/oras-go/v2/registry/remote"

	"github.com/lxc/incus-compose/iclient"
	"github.com/lxc/incus-compose/shared"
)

// dockerManifestList is docker's spelling of an OCI image index.
const dockerManifestList = "application/vnd.docker.distribution.manifest.list.v2+json"

// ociImage is an image document whose config keeps the fields ocispec drops.
type ociImage struct {
	ocispec.Image

	Config ociImageConfig `json:"config"`
}

// ociImageConfig is an image config plus HEALTHCHECK, which the OCI spec has no
// field for and every builder writes anyway, OCI media type or not.
type ociImageConfig struct {
	ocispec.ImageConfig

	Healthcheck *ociHealthcheck `json:"Healthcheck,omitempty"`
}

// ociHealthcheck is docker's HealthConfig as it appears in an image config.
// Absent fields mean docker's defaults rather than zero, so they stay unwritten.
type ociHealthcheck struct {
	Test          []string
	Interval      time.Duration
	Timeout       time.Duration
	StartPeriod   time.Duration
	StartInterval time.Duration
	Retries       int
}

// Disabled reports HEALTHCHECK NONE, and an image with no check at all.
func (h *ociHealthcheck) Disabled() bool {
	return h == nil || len(h.Test) == 0 || h.Test[0] == "NONE"
}

// ociStoreConfig writes an image's OCI config into its properties.
func (r *Image) ociStoreConfig(ctx context.Context, server *iclient.Connection, project string, fingerprint string, config *ociImageConfig) error {
	img, eTag, err := server.GetImage(ctx, project, fingerprint, nil)
	if err != nil {
		return ErrNotFound.WithText("the image the properties belong to").Wrap(err)
	}

	state := &ImageState{}

	if config != nil {
		ociStateFromConfig(state, config)
	} else {
		// The presence of oci.volumes is what marks the config as already read.
		_, stored := img.Properties["oci.volumes"]
		if stored || r.source == nil || r.source.info.Protocol != "oci" {
			return nil
		}

		if r.State().SourceFingerprint != "" {
			// A refresh read it, deciding whether to re-pull this image.
			state = r.State()
		} else {
			_, _, fetched, err := r.ociResolveSource(ctx, r.platform)
			if err != nil {
				r.client.LogWarn("Cannot read the image config from its registry", "resource", r, "error", err)

				return nil
			}

			ociStateFromConfig(state, fetched)
		}
	}

	props := maps.Clone(img.Properties)
	if props == nil {
		props = map[string]string{}
	}

	ociWriteProperties(state, props)

	err = server.UpdateImage(ctx, project, fingerprint, incusApi.ImagePut{
		AutoUpdate: img.AutoUpdate,
		Properties: props,
		Public:     img.Public,
		ExpiresAt:  img.ExpiresAt,
		Profiles:   img.Profiles,
	}, eTag)
	if err != nil {
		return ErrCreate.WithText("storing the OCI config as image properties").Wrap(err)
	}

	return nil
}

// ociResolveSource reads the manifest built for platform, picking it out of the
// index when the reference is multi-arch, and answers the three questions it
// holds: the manifest digest to pin a pull to, the fingerprint Incus would give
// the image, and the image's config.
func (r *Image) ociResolveSource(ctx context.Context, platform string) (string, string, *ociImageConfig, error) {
	repo, err := iclient.NewRepository(r.source.info, r.image)
	if err != nil {
		return "", "", nil, err
	}

	desc, rc, err := repo.FetchReference(ctx, repo.Reference.ReferenceOrDefault())
	if err != nil {
		return "", "", nil, ErrImageSource.WithText("fetching " + repo.Reference.String()).Wrap(err)
	}

	// ReadAll checks the bytes against the descriptor's size and digest.
	body, err := content.ReadAll(rc, desc)

	_ = rc.Close()

	if err != nil {
		return "", "", nil, ErrImageSource.WithText("reading " + repo.Reference.String()).Wrap(err)
	}

	indexed := desc.MediaType == ocispec.MediaTypeImageIndex || desc.MediaType == dockerManifestList
	if indexed {
		desc, err = ociPickManifest(body, platform)
		if err != nil {
			return "", "", nil, err
		}

		body, err = ociFetchDescriptor(ctx, repo, desc)
		if err != nil {
			return "", "", nil, ErrImageSource.WithText(fmt.Sprintf("fetching the %s manifest of %s", platform, repo.Reference)).Wrap(err)
		}
	}

	var manifest ocispec.Manifest

	err = json.Unmarshal(body, &manifest)
	if err != nil {
		return "", "", nil, ErrInvalidFormat.WithText("the manifest of " + repo.Reference.String()).Wrap(err)
	}

	// incusd hashes the layer digests skopeo reports rather than the manifest
	// digest, so anything else compares as changed on every run.
	h := sha256.New()
	for _, layer := range manifest.Layers {
		h.Write([]byte(layer.Digest))
	}

	// docker's config media type differs from OCI's, the document does not.
	body, err = ociFetchDescriptor(ctx, repo, manifest.Config)
	if err != nil {
		return "", "", nil, ErrImageSource.WithText("fetching the config of " + repo.Reference.String()).Wrap(err)
	}

	var image ociImage

	err = json.Unmarshal(body, &image)
	if err != nil {
		return "", "", nil, ErrInvalidFormat.WithText("the config of " + repo.Reference.String()).Wrap(err)
	}

	// A single-manifest reference was matched against nothing on the way here,
	// so its own config is the only thing that says what it is. Pinning it
	// unchecked would put one architecture in the store under another's alias,
	// where every later run in every project finds it.
	if !indexed {
		got := ociPlatform(&image.Platform)
		if image.OS != "linux" || got != platform {
			return "", "", nil, ErrNoPlatform.WithText(fmt.Sprintf(
				"%s is %s, not linux/%s",
				repo.Reference, cmp.Or(image.OS, "an image of no OS")+"/"+cmp.Or(got, "no architecture Incus runs"), platform))
		}
	}

	return desc.Digest.String(), fmt.Sprintf("%x", h.Sum(nil)), &image.Config, nil
}

// ociFetchDescriptor reads one manifest or blob the registry already described.
func ociFetchDescriptor(ctx context.Context, repo *remote.Repository, desc ocispec.Descriptor) ([]byte, error) {
	rc, err := repo.Fetch(ctx, desc)
	if err != nil {
		return nil, err
	}

	defer func() { _ = rc.Close() }()

	return content.ReadAll(rc, desc)
}

// ociPickManifest chooses the index entry built for platform, which is the
// registry spelling without the OS.
func ociPickManifest(index []byte, platform string) (ocispec.Descriptor, error) {
	var idx ocispec.Index

	err := json.Unmarshal(index, &idx)
	if err != nil {
		return ocispec.Descriptor{}, ErrInvalidFormat.WithText("the image index").Wrap(err)
	}

	for _, manifest := range idx.Manifests {
		if manifest.Platform == nil || manifest.Platform.OS != "linux" {
			continue
		}

		if ociPlatform(manifest.Platform) == platform {
			return manifest, nil
		}
	}

	return ocispec.Descriptor{}, ErrNoPlatform.WithText(platform)
}

// ociPlatform is an index entry's platform in the spelling the cache alias
// uses. An architecture Incus cannot run answers "", which matches nothing.
func ociPlatform(p *ocispec.Platform) string {
	spec := p.Architecture
	if p.Variant != "" {
		spec += "/" + p.Variant
	}

	key, _, ok := platformKey(spec)
	if !ok {
		return ""
	}

	return key
}

// ociStateFromConfig reads an image's own OCI config into s.
func ociStateFromConfig(s *ImageState, config *ociImageConfig) {
	// A named USER lands as 0 here; oci.uid takes nothing else, and only the
	// image's own /etc/passwd resolves it, a project away from this one.
	s.UID, s.GID = 0, 0

	if config.User != "" {
		user, group, _ := strings.Cut(config.User, ":")
		s.UID, _ = strconv.ParseUint(user, 10, 32)
		s.GID, _ = strconv.ParseUint(group, 10, 32)
	}

	s.OCIUser = config.User
	s.Entrypoint = shellquote.Join(config.Entrypoint...)
	s.Cmd = shellquote.Join(config.Cmd...)
	s.Volumes = slices.Sorted(maps.Keys(config.Volumes))

	s.Cwd = config.WorkingDir
	if s.Cwd == "" {
		s.Cwd = "/"
	}

	// NONE and no check at all both mean the image's is not to be used, so only
	// one of the two ever reaches an instance.
	s.Healthcheck = config.Healthcheck
	if s.Healthcheck.Disabled() {
		s.Healthcheck = nil
	}
}

// ociHealthConfig writes an image's own HEALTHCHECK into the instance config.
func ociHealthConfig(config map[string]string, hc *ociHealthcheck) {
	if hc.Disabled() {
		return
	}

	// Marshaling a []string cannot fail.
	test, _ := json.Marshal(hc.Test)

	config[shared.HealthEnabledKey] = "true"
	config[HealthKeyPrefix+"test"] = string(test)

	// An absent field is docker's default rather than zero, so it stays
	// unwritten and ic-healthd's own default applies.
	if hc.Interval > 0 {
		config[HealthKeyPrefix+"interval"] = hc.Interval.String()
	}

	if hc.Timeout > 0 {
		config[HealthKeyPrefix+"timeout"] = hc.Timeout.String()
	}

	if hc.StartPeriod > 0 {
		config[HealthKeyPrefix+"start_period"] = hc.StartPeriod.String()
	}

	if hc.StartInterval > 0 {
		config[HealthKeyPrefix+"start_interval"] = hc.StartInterval.String()
	}

	if hc.Retries > 0 {
		config[HealthKeyPrefix+"retries"] = strconv.Itoa(hc.Retries)
	}
}

// ociWriteProperties writes s' oci.* values into props, mirroring
// ociReadProperties.
func ociWriteProperties(s *ImageState, props map[string]string) {
	props["oci.uid"] = strconv.FormatUint(s.UID, 10)
	props["oci.gid"] = strconv.FormatUint(s.GID, 10)
	props["oci.entrypoint"] = s.Entrypoint
	props["oci.cmd"] = s.Cmd
	props["oci.cwd"] = s.Cwd

	// The presence of oci.volumes is what marks the config as already read, so
	// an image declaring none still has to carry the key.
	props["oci.volumes"] = ","
	if len(s.Volumes) > 0 {
		props["oci.volumes"] = strings.Join(s.Volumes, ",")
	}

	if s.OCIUser != "" {
		props[OCIUserKey] = s.OCIUser
	}

	delete(props, "oci.healthcheck")

	if s.Healthcheck != nil {
		// Marshaling a struct of strings, ints and durations cannot fail.
		blob, _ := json.Marshal(s.Healthcheck)
		props["oci.healthcheck"] = string(blob)
	}
}

// ociReadProperties reads oci.* values into s. An image cached without oci.cmd
// has both concatenated in oci.entrypoint, which is the same argv either way.
func ociReadProperties(s *ImageState, props map[string]string) {
	uid, err := strconv.ParseUint(props["oci.uid"], 10, 32)
	if err == nil {
		s.UID = uid
	}

	gid, err := strconv.ParseUint(props["oci.gid"], 10, 32)
	if err == nil {
		s.GID = gid
	}

	s.OCIUser = props[OCIUserKey]
	s.Entrypoint = props["oci.entrypoint"]
	s.Cmd = props["oci.cmd"]
	s.Cwd = props["oci.cwd"]

	s.Volumes = nil

	for _, at := range strings.Split(props["oci.volumes"], ",") {
		if at != "" {
			s.Volumes = append(s.Volumes, at)
		}
	}

	s.Healthcheck = nil

	blob := props["oci.healthcheck"]
	if blob == "" {
		return
	}

	hc := &ociHealthcheck{}

	err = json.Unmarshal([]byte(blob), hc)
	if err == nil {
		s.Healthcheck = hc
	}
}
