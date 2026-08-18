package client

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"

	"github.com/kballard/go-shellquote"
	incusApi "github.com/lxc/incus/v7/shared/api"
	"github.com/lxc/incus/v7/shared/osarch"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2/content"
	"oras.land/oras-go/v2/registry/remote"

	"github.com/lxc/incus-compose/iclient"
)

// dockerManifestList is docker's spelling of an OCI image index.
const dockerManifestList = "application/vnd.docker.distribution.manifest.list.v2+json"

// ociStoreConfig writes an image's OCI config into its properties.
func (r *Image) ociStoreConfig(ctx context.Context, server *iclient.Connection, fingerprint string, config *ocispec.ImageConfig) error {
	img, eTag, err := server.GetImage(ctx, fingerprint, nil)
	if err != nil {
		return fmt.Errorf("getting image for property update: %w", err)
	}

	if config == nil {
		_, stored := img.Properties["oci.volumes"]
		if stored || r.source == nil || r.source.info.Protocol != "oci" {
			return nil
		}

		config, err = r.ociFetchConfig(ctx, img.Architecture)
		if err != nil {
			r.client.LogWarn("Cannot read the image config from its registry", "resource", r, "error", err)

			return nil
		}
	}

	// A named USER lands as 0 here; oci.uid takes nothing else, and only the
	// image's own /etc/passwd resolves it, a project away from this one.
	var uid, gid uint64

	if config.User != "" {
		user, group, _ := strings.Cut(config.User, ":")
		uid, _ = strconv.ParseUint(user, 10, 32)
		gid, _ = strconv.ParseUint(group, 10, 32)
	}

	cwd := config.WorkingDir
	if cwd == "" {
		cwd = "/"
	}

	props := maps.Clone(img.Properties)
	if props == nil {
		props = map[string]string{}
	}

	// The presence of oci.volumes is what marks the config as already read.
	volumes := strings.Join(slices.Sorted(maps.Keys(config.Volumes)), ",")
	if volumes == "" {
		volumes = ","
	}

	props["oci.uid"] = strconv.FormatUint(uid, 10)
	props["oci.gid"] = strconv.FormatUint(gid, 10)
	props["oci.entrypoint"] = shellquote.Join(config.Entrypoint...)
	props["oci.cmd"] = shellquote.Join(config.Cmd...)
	props["oci.cwd"] = cwd
	props["oci.volumes"] = volumes

	if config.User != "" {
		props[OCIUserKey] = config.User
	}

	err = server.UpdateImage(ctx, fingerprint, incusApi.ImagePut{
		AutoUpdate: img.AutoUpdate,
		Properties: props,
		Public:     img.Public,
		ExpiresAt:  img.ExpiresAt,
		Profiles:   img.Profiles,
	}, eTag)
	if err != nil {
		return fmt.Errorf("storing OCI config as image properties: %w", err)
	}

	return nil
}

// ociFetchConfig reads the image's config from its registry.
func (r *Image) ociFetchConfig(ctx context.Context, arch string) (*ocispec.ImageConfig, error) {
	repo, err := iclient.NewRepository(r.source.info, r.image)
	if err != nil {
		return nil, err
	}

	desc, rc, err := repo.FetchReference(ctx, repo.Reference.ReferenceOrDefault())
	if err != nil {
		return nil, fmt.Errorf("fetching %s: %w", repo.Reference, err)
	}

	// ReadAll checks the bytes against the descriptor's size and digest.
	body, err := content.ReadAll(rc, desc)

	_ = rc.Close()

	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", repo.Reference, err)
	}

	if desc.MediaType == ocispec.MediaTypeImageIndex || desc.MediaType == dockerManifestList {
		desc, err = ociPickManifest(body, arch)
		if err != nil {
			return nil, err
		}

		body, err = ociFetchDescriptor(ctx, repo, desc)
		if err != nil {
			return nil, fmt.Errorf("fetching the %s manifest of %s: %w", arch, repo.Reference, err)
		}
	}

	var manifest ocispec.Manifest

	err = json.Unmarshal(body, &manifest)
	if err != nil {
		return nil, fmt.Errorf("decoding the manifest of %s: %w", repo.Reference, err)
	}

	// docker's config media type differs from OCI's, the document does not.
	body, err = ociFetchDescriptor(ctx, repo, manifest.Config)
	if err != nil {
		return nil, fmt.Errorf("fetching the config of %s: %w", repo.Reference, err)
	}

	var image ocispec.Image

	err = json.Unmarshal(body, &image)
	if err != nil {
		return nil, fmt.Errorf("decoding the config of %s: %w", repo.Reference, err)
	}

	return &image.Config, nil
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

// ociPickManifest chooses the index entry built for arch.
func ociPickManifest(index []byte, arch string) (ocispec.Descriptor, error) {
	var idx ocispec.Index

	err := json.Unmarshal(index, &idx)
	if err != nil {
		return ocispec.Descriptor{}, fmt.Errorf("decoding the image index: %w", err)
	}

	want, err := osarch.ArchitectureID(arch)
	if err != nil {
		return ocispec.Descriptor{}, ErrNoPlatform.WithText(arch).Wrap(err)
	}

	for _, manifest := range idx.Manifests {
		if manifest.Platform == nil || manifest.Platform.OS != "linux" {
			continue
		}

		// Incus' alias table makes "amd64" and "x86_64" one architecture.
		got, err := osarch.ArchitectureID(manifest.Platform.Architecture)
		if err == nil && got == want {
			return manifest, nil
		}
	}

	return ocispec.Descriptor{}, ErrNoPlatform.WithText(arch)
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
