package client

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lxc/incus-compose/iclient"
)

func TestOCIReadProperties(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		name  string
		props map[string]string
		want  ImageState
	}{
		{
			name: "the split an image carries today",
			props: map[string]string{
				"oci.uid":        "65534",
				"oci.gid":        "65534",
				"oci.entrypoint": "docker-entrypoint.sh",
				"oci.cmd":        "redis-server",
				"oci.cwd":        "/data",
				"oci.volumes":    "/data,/var/log",
			},
			want: ImageState{
				UID: 65534, GID: 65534,
				Entrypoint: "docker-entrypoint.sh",
				Cmd:        "redis-server",
				Cwd:        "/data",
				Volumes:    []string{"/data", "/var/log"},
			},
		},
		{
			// Written before the split: one merged entrypoint, no cmd, no
			// volumes. Concatenating either spelling gives the same argv.
			name: "an image stored before the split",
			props: map[string]string{
				"oci.uid":        "0",
				"oci.gid":        "0",
				"oci.entrypoint": "docker-entrypoint.sh redis-server",
				"oci.cwd":        "/data",
			},
			want: ImageState{
				Entrypoint: "docker-entrypoint.sh redis-server",
				Cwd:        "/data",
			},
		},
		{
			name:  "a native Incus image carries none of it",
			props: map[string]string{},
			want:  ImageState{},
		},
		{
			name:  "an empty volume list is no volumes, not one empty path",
			props: map[string]string{"oci.volumes": ""},
			want:  ImageState{},
		},
		{
			name:  "the marker an image with no VOLUME carries is no volumes either",
			props: map[string]string{"oci.volumes": ","},
			want:  ImageState{},
		},
		{
			name:  "a value that is not a number is left at zero",
			props: map[string]string{"oci.uid": "nobody"},
			want:  ImageState{},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := ImageState{}
			ociReadProperties(&got, tt.props)

			assert.Equal(t, tt.want, got)
		})
	}
}

// A config is flattened into the state, stored as properties, and read back out
// of them by a later fetch, so the three have to agree.
func TestOCIStateRoundTrip(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		name   string
		config ocispec.ImageConfig
		want   ImageState
	}{
		{
			name: "a config declaring everything",
			config: ocispec.ImageConfig{
				User:       "999:998",
				Entrypoint: []string{"docker-entrypoint.sh"},
				Cmd:        []string{"redis-server", "--appendonly", "yes"},
				WorkingDir: "/data",
				Volumes:    map[string]struct{}{"/var/log": {}, "/data": {}},
			},
			want: ImageState{
				UID: 999, GID: 998,
				OCIUser:    "999:998",
				Entrypoint: "docker-entrypoint.sh",
				Cmd:        "redis-server --appendonly yes",
				Cwd:        "/data",
				Volumes:    []string{"/data", "/var/log"},
			},
		},
		{
			name:   "an image declaring nothing still gets a working directory",
			config: ocispec.ImageConfig{},
			want:   ImageState{Cwd: "/"},
		},
		{
			// oci.uid takes a number, so a name survives only in its own key.
			name:   "a named user leaves the ids at zero",
			config: ocispec.ImageConfig{User: "nobody"},
			want:   ImageState{OCIUser: "nobody", Cwd: "/"},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := ImageState{}
			ociStateFromConfig(&got, &tt.config)
			assert.Equal(t, tt.want, got)

			props := map[string]string{}
			ociWriteProperties(&got, props)

			back := ImageState{}
			ociReadProperties(&back, props)
			assert.Equal(t, tt.want, back, "what is written to properties must read back the same")
		})
	}
}

func TestOCIPickManifest(t *testing.T) {
	t.Parallel()

	index, err := json.Marshal(ocispec.Index{
		Manifests: []ocispec.Descriptor{
			{
				Digest:   digest.FromString("arm64"),
				Platform: &ocispec.Platform{OS: "linux", Architecture: "arm64", Variant: "v8"},
			},
			{
				Digest:   digest.FromString("windows"),
				Platform: &ocispec.Platform{OS: "windows", Architecture: "amd64"},
			},
			{
				Digest:   digest.FromString("armv5"),
				Platform: &ocispec.Platform{OS: "linux", Architecture: "arm", Variant: "v5"},
			},
			{
				Digest:   digest.FromString("armv6"),
				Platform: &ocispec.Platform{OS: "linux", Architecture: "arm", Variant: "v6"},
			},
			{
				Digest:   digest.FromString("amd64"),
				Platform: &ocispec.Platform{OS: "linux", Architecture: "amd64"},
			},
			{
				Digest:   digest.FromString("armv7"),
				Platform: &ocispec.Platform{OS: "linux", Architecture: "arm", Variant: "v7"},
			},
			{
				Digest: digest.FromString("attestation"),
			},
		},
	})
	require.NoError(t, err)

	t.Run("amd64", func(t *testing.T) {
		t.Parallel()

		desc, err := ociPickManifest(index, "amd64")
		require.NoError(t, err)
		assert.Equal(t, digest.FromString("amd64"), desc.Digest)
	})

	// The registry spells out a variant Incus' architecture table folds away.
	t.Run("arm64 matches the entry carrying v8", func(t *testing.T) {
		t.Parallel()

		desc, err := ociPickManifest(index, "arm64")
		require.NoError(t, err)
		assert.Equal(t, digest.FromString("arm64"), desc.Digest)
	})

	// Every entry is architecture "arm"; only the variant tells them apart, and
	// picking either for the other ships code the CPU may not run. Incus has no
	// ARMv5, so v5 is the one that folds if anything does - busybox lists it
	// first, which is what a first-match picker would return for v6.
	t.Run("arm variants are distinguished", func(t *testing.T) {
		t.Parallel()

		for _, tt := range []struct{ platform, want string }{
			{"arm/v5", "armv5"},
			{"arm/v6", "armv6"},
			{"arm/v7", "armv7"},
		} {
			desc, err := ociPickManifest(index, tt.platform)
			require.NoError(t, err, tt.platform)
			assert.Equal(t, digest.FromString(tt.want), desc.Digest, tt.platform)
		}
	})

	t.Run("an architecture the image was not built for", func(t *testing.T) {
		t.Parallel()

		_, err := ociPickManifest(index, "s390x")
		require.ErrorIs(t, err, ErrNoPlatform)
	})

	t.Run("an architecture Incus does not know", func(t *testing.T) {
		t.Parallel()

		_, err := ociPickManifest(index, "pdp11")
		require.ErrorIs(t, err, ErrNoPlatform)
	})
}

// ociTestRegistry serves one image over the Distribution API, counting what it
// was asked for so a caller can tell a second read from a memoised one.
func ociTestRegistry(t *testing.T, blobs map[digest.Digest][]byte, tag digest.Digest) (*httptest.Server, *atomic.Int64) {
	t.Helper()

	requests := &atomic.Int64{}

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)

		want := tag
		if d := digest.Digest(ociPathDigest(r.URL.Path)); d != "" {
			want = d
		}

		body, ok := blobs[want]
		if !ok {
			w.WriteHeader(http.StatusNotFound)

			return
		}

		w.Header().Set("Content-Type", ociContentType(body))
		w.Header().Set("Docker-Content-Digest", want.String())
		_, _ = w.Write(body)
	})

	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	return server, requests
}

// ociPathDigest returns the sha256 reference a request names, if it names one.
func ociPathDigest(path string) string {
	for i := len(path) - 1; i >= 0; i-- {
		if path[i] == '/' {
			if rest := path[i+1:]; len(rest) > 7 && rest[:7] == "sha256:" {
				return rest
			}

			return ""
		}
	}

	return ""
}

// ociContentType reads the media type back out of what is being served.
func ociContentType(body []byte) string {
	var probe struct {
		MediaType string `json:"mediaType"`
	}

	_ = json.Unmarshal(body, &probe)
	if probe.MediaType != "" {
		return probe.MediaType
	}

	return ocispec.MediaTypeImageConfig
}

// ociTestImage is an Image with just enough set to reach a registry.
func ociTestImage(addr string) *Image {
	return &Image{
		image: "library/redis:alpine",
		source: &imageSource{info: &iclient.ConfigRemoteInfo{
			Name:     "reg",
			Addrs:    []string{addr},
			Protocol: "oci",
		}},
	}
}

func TestOCIResolveSource(t *testing.T) {
	t.Parallel()

	config, err := json.Marshal(ocispec.Image{
		Platform: ocispec.Platform{OS: "linux", Architecture: "amd64"},
		Config: ocispec.ImageConfig{
			Entrypoint: []string{"docker-entrypoint.sh"},
			Cmd:        []string{"redis-server"},
			WorkingDir: "/data",
			User:       "999:999",
			Volumes:    map[string]struct{}{"/data": {}},
		},
	})
	require.NoError(t, err)

	configDigest := digest.FromBytes(config)

	manifest, err := json.Marshal(ocispec.Manifest{
		MediaType: ocispec.MediaTypeImageManifest,
		Config: ocispec.Descriptor{
			MediaType: ocispec.MediaTypeImageConfig,
			Digest:    configDigest,
			Size:      int64(len(config)),
		},
	})
	require.NoError(t, err)

	manifestDigest := digest.FromBytes(manifest)

	index, err := json.Marshal(ocispec.Index{
		MediaType: ocispec.MediaTypeImageIndex,
		Manifests: []ocispec.Descriptor{
			{
				MediaType: ocispec.MediaTypeImageManifest,
				Digest:    manifestDigest,
				Size:      int64(len(manifest)),
				Platform:  &ocispec.Platform{OS: "linux", Architecture: "amd64"},
			},
		},
	})
	require.NoError(t, err)

	indexDigest := digest.FromBytes(index)

	blobs := map[digest.Digest][]byte{
		indexDigest:    index,
		manifestDigest: manifest,
		configDigest:   config,
	}

	t.Run("a multi-arch tag is followed to the right manifest", func(t *testing.T) {
		t.Parallel()

		server, _ := ociTestRegistry(t, blobs, indexDigest)

		_, _, got, err := ociTestImage(server.URL).ociResolveSource(t.Context(), "amd64")
		require.NoError(t, err)

		assert.Equal(t, &ocispec.ImageConfig{
			User:       "999:999",
			Entrypoint: []string{"docker-entrypoint.sh"},
			Cmd:        []string{"redis-server"},
			WorkingDir: "/data",
			Volumes:    map[string]struct{}{"/data": {}},
		}, got)
	})

	// A refresh needs both, and the config hangs off the manifest the fingerprint
	// was hashed from, so asking for them separately would walk the index twice.
	t.Run("the fingerprint and the config come from one walk", func(t *testing.T) {
		t.Parallel()

		server, requests := ociTestRegistry(t, blobs, indexDigest)

		_, fingerprint, config, err := ociTestImage(server.URL).ociResolveSource(t.Context(), "amd64")
		require.NoError(t, err)
		require.NotEmpty(t, fingerprint)
		require.NotNil(t, config)

		assert.Equal(t, int64(3), requests.Load(), "the index, the manifest it names, and the config blob")
	})

	// A single-arch tag answers with the manifest itself, no index to follow.
	t.Run("a single-arch tag is read directly", func(t *testing.T) {
		t.Parallel()

		server, _ := ociTestRegistry(t, blobs, manifestDigest)

		_, _, got, err := ociTestImage(server.URL).ociResolveSource(t.Context(), "amd64")
		require.NoError(t, err)

		assert.Equal(t, []string{"redis-server"}, got.Cmd)
	})

	t.Run("an architecture the image was not built for", func(t *testing.T) {
		t.Parallel()

		server, _ := ociTestRegistry(t, blobs, indexDigest)

		_, _, _, err := ociTestImage(server.URL).ociResolveSource(t.Context(), "s390x")
		require.ErrorIs(t, err, ErrNoPlatform)
	})

	// Nothing matched a single-arch tag against anything, so pinning it
	// unchecked would store one architecture under another's cache alias.
	t.Run("a single-arch tag of the wrong architecture", func(t *testing.T) {
		t.Parallel()

		server, _ := ociTestRegistry(t, blobs, manifestDigest)

		_, _, _, err := ociTestImage(server.URL).ociResolveSource(t.Context(), "arm64")
		require.ErrorIs(t, err, ErrNoPlatform)
	})

	// Nothing matched the architecture either, and a windows manifest carries
	// an architecture that compares equal to a linux one.
	t.Run("a single-arch tag built for another OS", func(t *testing.T) {
		t.Parallel()

		win, err := json.Marshal(ocispec.Image{
			Platform: ocispec.Platform{OS: "windows", Architecture: "amd64"},
			Config:   ocispec.ImageConfig{User: "nobody"},
		})
		require.NoError(t, err)

		winConfigDigest := digest.FromBytes(win)

		winManifest, err := json.Marshal(ocispec.Manifest{
			MediaType: ocispec.MediaTypeImageManifest,
			Config: ocispec.Descriptor{
				MediaType: ocispec.MediaTypeImageConfig,
				Digest:    winConfigDigest,
				Size:      int64(len(win)),
			},
		})
		require.NoError(t, err)

		winManifestDigest := digest.FromBytes(winManifest)

		server, _ := ociTestRegistry(t, map[digest.Digest][]byte{
			winManifestDigest: winManifest,
			winConfigDigest:   win,
		}, winManifestDigest)

		_, _, _, err = ociTestImage(server.URL).ociResolveSource(t.Context(), "amd64")
		require.ErrorIs(t, err, ErrNoPlatform)
	})

	// A named user needs the rootfs' /etc/passwd to resolve, so it lands as root.
	t.Run("an image declaring almost nothing", func(t *testing.T) {
		t.Parallel()

		bare, err := json.Marshal(ocispec.Image{
			Platform: ocispec.Platform{OS: "linux", Architecture: "amd64"},
			Config:   ocispec.ImageConfig{User: "nobody"},
		})
		require.NoError(t, err)

		bareConfigDigest := digest.FromBytes(bare)

		bareManifest, err := json.Marshal(ocispec.Manifest{
			MediaType: ocispec.MediaTypeImageManifest,
			Config: ocispec.Descriptor{
				MediaType: ocispec.MediaTypeImageConfig,
				Digest:    bareConfigDigest,
				Size:      int64(len(bare)),
			},
		})
		require.NoError(t, err)

		bareManifestDigest := digest.FromBytes(bareManifest)

		server, _ := ociTestRegistry(t, map[digest.Digest][]byte{
			bareManifestDigest: bareManifest,
			bareConfigDigest:   bare,
		}, bareManifestDigest)

		_, _, got, err := ociTestImage(server.URL).ociResolveSource(t.Context(), "amd64")
		require.NoError(t, err)

		assert.Equal(t, &ocispec.ImageConfig{User: "nobody"}, got)
	})
}
