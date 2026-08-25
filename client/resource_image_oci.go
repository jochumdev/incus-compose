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

	"github.com/kballard/go-shellquote"
	incusApi "github.com/lxc/incus/v7/shared/api"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2/content"
	"oras.land/oras-go/v2/registry/remote"

	"github.com/lxc/incus-compose/iclient"
)

// dockerManifestList is docker's spelling of an OCI image index.
const dockerManifestList = "application/vnd.docker.distribution.manifest.list.v2+json"

// ociStoreConfig writes an image's OCI config into its properties.
func (r *Image) ociStoreConfig(ctx context.Context, server *iclient.Connection, project string, fingerprint string, config *ocispec.ImageConfig) error {
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
func (r *Image) ociResolveSource(ctx context.Context, platform string) (string, string, *ocispec.ImageConfig, error) {
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

	var image ocispec.Image

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
func ociStateFromConfig(s *ImageState, config *ocispec.ImageConfig) {
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
}
