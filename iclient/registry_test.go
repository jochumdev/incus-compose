package iclient

import (
	"context"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"oras.land/oras-go/v2/registry/remote/auth"
	"oras.land/oras-go/v2/registry/remote/retry"
)

// transportOfRepo digs out the transport NewRepository built.
func transportOfRepo(t *testing.T, client any) *http.Transport {
	t.Helper()

	authClient, ok := client.(*auth.Client)
	require.True(t, ok)

	retryTransport, ok := authClient.Client.Transport.(*retry.Transport)
	require.True(t, ok)

	transport, ok := retryTransport.Base.(*http.Transport)
	require.True(t, ok)

	return transport
}

func TestNewRepositoryReference(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		name       string
		addr       string
		image      string
		registry   string
		host       string
		repository string
		reference  string
		plainHTTP  bool
	}{
		{
			name:       "a well-known registry resolves to Docker Hub's real host",
			addr:       "https://docker.io",
			image:      "library/redis:alpine",
			registry:   "docker.io",
			host:       "registry-1.docker.io",
			repository: "library/redis",
			reference:  "alpine",
		},
		{
			name:       "a mirror is reached instead of the registry it stands in for",
			addr:       "https://docker-registry.example.com",
			image:      "library/redis:alpine",
			registry:   "docker-registry.example.com",
			host:       "docker-registry.example.com",
			repository: "library/redis",
			reference:  "alpine",
		},
		{
			name:       "a port survives",
			addr:       "https://registry.example.com:5000",
			image:      "myorg/myapp:v1.0",
			registry:   "registry.example.com:5000",
			host:       "registry.example.com:5000",
			repository: "myorg/myapp",
			reference:  "v1.0",
		},
		{
			name:       "an http address asks for plain HTTP",
			addr:       "http://registry.example.com",
			image:      "myorg/myapp:v1.0",
			registry:   "registry.example.com",
			host:       "registry.example.com",
			repository: "myorg/myapp",
			reference:  "v1.0",
			plainHTTP:  true,
		},
		{
			name:       "a digest is a reference too",
			addr:       "https://ghcr.io",
			image:      "myorg/myapp@sha256:0000000000000000000000000000000000000000000000000000000000000000",
			registry:   "ghcr.io",
			host:       "ghcr.io",
			repository: "myorg/myapp",
			reference:  "sha256:0000000000000000000000000000000000000000000000000000000000000000",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			repo, err := NewRepository(&ConfigRemoteInfo{
				Name:     "reg",
				Addrs:    []string{tt.addr},
				Protocol: "oci",
			}, tt.image)
			require.NoError(t, err)

			assert.Equal(t, tt.registry, repo.Reference.Registry)
			assert.Equal(t, tt.host, repo.Reference.Host())
			assert.Equal(t, tt.repository, repo.Reference.Repository)
			assert.Equal(t, tt.reference, repo.Reference.Reference)
			assert.Equal(t, tt.plainHTTP, repo.PlainHTTP)
		})
	}
}

func TestNewRepositoryRejects(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		name  string
		info  *ConfigRemoteInfo
		image string
		errs  error
	}{
		{
			name:  "an incus remote is not a registry",
			info:  &ConfigRemoteInfo{Name: "ict", Addrs: []string{"https://127.0.0.1:8443"}, Protocol: "incus"},
			image: "library/redis:alpine",
			errs:  ErrRegistryProtocol,
		},
		{
			name:  "a simplestreams remote is not a registry",
			info:  &ConfigRemoteInfo{Name: "images", Addrs: []string{"https://images.example.com"}, Protocol: "simplestreams"},
			image: "library/redis:alpine",
			errs:  ErrRegistryProtocol,
		},
		{
			name:  "a registry with no address",
			info:  &ConfigRemoteInfo{Name: "reg", Protocol: "oci"},
			image: "library/redis:alpine",
			errs:  ErrConnectionNoAddress,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			repo, err := NewRepository(tt.info, tt.image)
			require.ErrorIs(t, err, tt.errs)
			assert.Nil(t, repo)
		})
	}
}

func TestNewRepositoryRejectsBadImage(t *testing.T) {
	t.Parallel()

	for _, image := range []string{"", "Library/Redis:alpine", "library/redis:not a tag", "/library/redis"} {
		t.Run(image, func(t *testing.T) {
			t.Parallel()

			repo, err := NewRepository(&ConfigRemoteInfo{
				Name:     "reg",
				Addrs:    []string{"https://registry.example.com"},
				Protocol: "oci",
			}, image)
			require.Error(t, err)
			assert.Nil(t, repo)
		})
	}
}

func TestNewRepositoryCredentials(t *testing.T) {
	t.Parallel()

	t.Run("the resolved login becomes the registry credential", func(t *testing.T) {
		t.Parallel()

		repo, err := NewRepository(&ConfigRemoteInfo{
			Name:     "reg",
			Addrs:    []string{"https://registry.example.com"},
			Protocol: "oci",
			Username: "user",
			Password: "s3cret",
		}, "myorg/myapp:v1.0")
		require.NoError(t, err)

		client, ok := repo.Client.(*auth.Client)
		require.True(t, ok)
		require.NotNil(t, client.Credential)

		cred, err := client.Credential(context.Background(), "registry.example.com")
		require.NoError(t, err)
		assert.Equal(t, "user", cred.Username)
		assert.Equal(t, "s3cret", cred.Password)
	})

	// auth.Client maps docker.io to registry-1.docker.io before it looks a
	// credential up, so a credential filed under docker.io would never be found.
	t.Run("a Docker Hub credential is filed under the host auth asks for", func(t *testing.T) {
		t.Parallel()

		repo, err := NewRepository(&ConfigRemoteInfo{
			Name:     "docker.io",
			Addrs:    []string{"https://docker.io"},
			Protocol: "oci",
			Username: "user",
			Password: "s3cret",
		}, "library/redis:alpine")
		require.NoError(t, err)

		client, ok := repo.Client.(*auth.Client)
		require.True(t, ok)

		cred, err := client.Credential(context.Background(), "registry-1.docker.io")
		require.NoError(t, err)
		assert.Equal(t, "user", cred.Username)
	})

	// A password in Addrs would reach the logs and our own error strings, so it
	// is refused rather than quietly ignored.
	t.Run("an address carrying credentials is refused", func(t *testing.T) {
		t.Parallel()

		repo, err := NewRepository(&ConfigRemoteInfo{
			Name:     "reg",
			Addrs:    []string{"https://user:s3cret@registry.example.com"},
			Protocol: "oci",
		}, "myorg/myapp:v1.0")
		require.ErrorIs(t, err, ErrRegistryAddrCredentials)
		assert.Nil(t, repo)
		assert.NotContains(t, err.Error(), "s3cret")
	})

	t.Run("no login leaves the registry anonymous", func(t *testing.T) {
		t.Parallel()

		repo, err := NewRepository(&ConfigRemoteInfo{
			Name:     "reg",
			Addrs:    []string{"https://registry.example.com"},
			Protocol: "oci",
		}, "myorg/myapp:v1.0")
		require.NoError(t, err)

		client, ok := repo.Client.(*auth.Client)
		require.True(t, ok)
		assert.Nil(t, client.Credential)
	})
}

func TestNewRepositoryTransport(t *testing.T) {
	t.Parallel()

	// Driving this through HTTPS_PROXY would prove nothing: ProxyFromEnvironment
	// reads the environment once per process, so whichever test got there first
	// decides the answer. Identity is what this package controls.
	t.Run("a corporate proxy is consulted", func(t *testing.T) {
		t.Parallel()

		repo, err := NewRepository(&ConfigRemoteInfo{
			Name:     "reg",
			Addrs:    []string{"https://registry.example.com"},
			Protocol: "oci",
		}, "myorg/myapp:v1.0")
		require.NoError(t, err)

		transport := transportOfRepo(t, repo.Client)
		require.NotNil(t, transport.Proxy)
		assert.Equal(t,
			reflect.ValueOf(http.ProxyFromEnvironment).Pointer(),
			reflect.ValueOf(transport.Proxy).Pointer(),
		)
	})

	// An operation long-poll needs the hour incusTransport carries; a registry
	// answering metadata does not.
	t.Run("a registry does not wait an hour for a header", func(t *testing.T) {
		t.Parallel()

		repo, err := NewRepository(&ConfigRemoteInfo{
			Name:     "reg",
			Addrs:    []string{"https://registry.example.com"},
			Protocol: "oci",
		}, "myorg/myapp:v1.0")
		require.NoError(t, err)

		transport := transportOfRepo(t, repo.Client)
		assert.Equal(t, registryResponseHeaderTimeout, transport.ResponseHeaderTimeout)
	})

	t.Run("the user agent reaches the registry", func(t *testing.T) {
		t.Parallel()

		repo, err := NewRepository(&ConfigRemoteInfo{
			Name:      "reg",
			Addrs:     []string{"https://registry.example.com"},
			Protocol:  "oci",
			UserAgent: "incus-compose/test",
		}, "myorg/myapp:v1.0")
		require.NoError(t, err)

		client, ok := repo.Client.(*auth.Client)
		require.True(t, ok)
		assert.Equal(t, "incus-compose/test", client.Header.Get("User-Agent"))
	})
}

// fakeRegistry serves one image's manifest over the Distribution API.
func fakeRegistry(t *testing.T, manifest []byte, wantAuth string) *httptest.Server {
	t.Helper()

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if wantAuth != "" && r.Header.Get("Authorization") != wantAuth {
			w.Header().Set("WWW-Authenticate", `Basic realm="registry"`)
			w.WriteHeader(http.StatusUnauthorized)

			return
		}

		if r.URL.Path != "/v2/library/redis/manifests/alpine" {
			w.WriteHeader(http.StatusNotFound)

			return
		}

		w.Header().Set("Content-Type", ocispec.MediaTypeImageManifest)
		w.Header().Set("Docker-Content-Digest", digest.FromBytes(manifest).String())
		_, _ = w.Write(manifest)
	})

	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	return server
}

// testManifest is the smallest manifest oras-go will hand back.
var testManifest = []byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","config":{"mediaType":"application/vnd.oci.image.config.v1+json","digest":"sha256:44136fa355b3678a1146ad16f7e8649e94fb4fc21fe77e8310c060f61caaff8a","size":2},"layers":[]}`)

func TestNewRepositoryFetches(t *testing.T) {
	t.Parallel()

	server := fakeRegistry(t, testManifest, "")

	repo, err := NewRepository(&ConfigRemoteInfo{
		Name:     "reg",
		Addrs:    []string{server.URL},
		Protocol: "oci",
	}, "library/redis:alpine")
	require.NoError(t, err)
	require.True(t, repo.PlainHTTP)

	desc, rc, err := repo.FetchReference(t.Context(), repo.Reference.Reference)
	require.NoError(t, err)

	defer func() { _ = rc.Close() }()

	assert.Equal(t, ocispec.MediaTypeImageManifest, desc.MediaType)
	assert.Equal(t, digest.FromBytes(testManifest).String(), desc.Digest.String())

	body, err := io.ReadAll(rc)
	require.NoError(t, err)
	assert.JSONEq(t, string(testManifest), string(body))
}

func TestNewRepositoryFetchesAuthenticated(t *testing.T) {
	t.Parallel()

	server := fakeRegistry(t, testManifest, "Basic dXNlcjpzM2NyZXQ=")

	repo, err := NewRepository(&ConfigRemoteInfo{
		Name:     "reg",
		Addrs:    []string{server.URL},
		Protocol: "oci",
		Username: "user",
		Password: "s3cret",
	}, "library/redis:alpine")
	require.NoError(t, err)

	_, rc, err := repo.FetchReference(t.Context(), repo.Reference.Reference)
	require.NoError(t, err)

	_ = rc.Close()
}

func TestNewRepositoryPinsTheServerCertificate(t *testing.T) {
	t.Parallel()

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", ocispec.MediaTypeImageManifest)
		w.Header().Set("Docker-Content-Digest", digest.FromBytes(testManifest).String())
		_, _ = w.Write(testManifest)
	})

	server := httptest.NewTLSServer(handler)
	t.Cleanup(server.Close)

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})
	require.NotNil(t, certPEM)

	t.Run("the pinned certificate is accepted", func(t *testing.T) {
		t.Parallel()

		repo, err := NewRepository(&ConfigRemoteInfo{
			Name:       "reg",
			Addrs:      []string{server.URL},
			Protocol:   "oci",
			ServerCert: string(certPEM),
		}, "library/redis:alpine")
		require.NoError(t, err)
		require.False(t, repo.PlainHTTP)

		_, rc, err := repo.FetchReference(t.Context(), repo.Reference.Reference)
		require.NoError(t, err)

		_ = rc.Close()
	})

	t.Run("an unknown certificate is refused", func(t *testing.T) {
		t.Parallel()

		repo, err := NewRepository(&ConfigRemoteInfo{
			Name:     "reg",
			Addrs:    []string{server.URL},
			Protocol: "oci",
		}, "library/redis:alpine")
		require.NoError(t, err)

		_, _, err = repo.FetchReference(t.Context(), repo.Reference.Reference)
		require.Error(t, err)
		assert.True(t, strings.Contains(err.Error(), "certificate"), "want a certificate error, got %v", err)
	})
}
