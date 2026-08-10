package client

import (
	"archive/tar"
	"io"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/stretchr/testify/require"
	"go.yaml.in/yaml/v4"
)

func TestBuildMetadataTar(t *testing.T) {
	t.Parallel()
	r, err := buildMetadataTar("local/myproject-myservice:latest", "x86_64", nil)
	require.NoError(t, err)

	tr := tar.NewReader(r)
	hdr, err := tr.Next()
	require.NoError(t, err)
	require.Equal(t, "metadata.yaml", hdr.Name)

	data, err := io.ReadAll(tr)
	require.NoError(t, err)

	var meta struct {
		Architecture string            `yaml:"architecture"`
		CreationDate int64             `yaml:"creation_date"`
		ExpiryDate   int64             `yaml:"expiry_date"`
		Properties   map[string]string `yaml:"properties"`
	}
	require.NoError(t, yaml.Unmarshal(data, &meta))

	require.Equal(t, "oci", meta.Properties["type"])
	require.Equal(t, "x86_64", meta.Architecture)
	require.Greater(t, meta.CreationDate, int64(0))
	require.Equal(t, int64(0), meta.ExpiryDate)
}

func TestBuildMetadataTarWithoutConfigJSON(t *testing.T) {
	t.Parallel()
	r, err := buildMetadataTar("test-image", "x86_64", nil)
	require.NoError(t, err)

	tr := tar.NewReader(r)
	_, err = tr.Next()
	require.NoError(t, err)

	_, err = tr.Next()
	require.ErrorIs(t, err, io.EOF)
}

func TestBuildMetadataTarWithConfigJSON(t *testing.T) {
	t.Parallel()
	configJSON := []byte(`{"ociVersion":"1.2.0","process":{"args":["/bin/sh"]}}`)
	r, err := buildMetadataTar("test-image", "x86_64", configJSON)
	require.NoError(t, err)

	tr := tar.NewReader(r)

	hdr, err := tr.Next()
	require.NoError(t, err)
	require.Equal(t, "metadata.yaml", hdr.Name)

	hdr, err = tr.Next()
	require.NoError(t, err)
	require.Equal(t, "config.json", hdr.Name)

	data, err := io.ReadAll(tr)
	require.NoError(t, err)
	require.Equal(t, configJSON, data)

	_, err = tr.Next()
	require.ErrorIs(t, err, io.EOF)
}

func TestOptionBuild(t *testing.T) {
	t.Parallel()
	tests := []struct {
		mode BuildMode
	}{
		{BuildAuto},
		{BuildForce},
		{BuildNever},
	}
	for _, tt := range tests {
		opts := NewOptions(OptionBuild(BuildInfo{Mode: tt.mode}))
		require.Equal(t, tt.mode, opts.Build.Mode)
	}
}

func TestIncusArchToPlatform(t *testing.T) {
	t.Parallel()
	tests := []struct {
		arch     string
		platform string
	}{
		{"x86_64", "linux/amd64"},
		{"i686", "linux/386"},
		{"aarch64", "linux/arm64"},
		{"armv7l", "linux/arm/v7"},
		{"armv6l", "linux/arm/v6"},
		{"ppc64le", "linux/ppc64le"},
		{"s390x", "linux/s390x"},
		{"riscv64", "linux/riscv64"},
	}
	for _, tt := range tests {
		platform, ok := incusArchToPlatform(tt.arch)
		require.True(t, ok)
		require.Equal(t, tt.platform, platform)
	}
}

func TestPlatformToIncusArch(t *testing.T) {
	t.Parallel()
	arch, ok := platformToIncusArch("linux/amd64", []string{"x86_64", "i686"})
	require.True(t, ok)
	require.Equal(t, "x86_64", arch)

	_, ok = platformToIncusArch("linux/arm64", []string{"x86_64", "i686"})
	require.False(t, ok)
}

func TestBuildArgs_Podman(t *testing.T) {
	t.Parallel()
	cfg := &BuildConfig{
		Context:    "/path/to/ctx",
		Dockerfile: "Containerfile",
		Args:       map[string]string{"FOO": "bar"},
		Platform:   "linux/amd64",
		Target:     "runtime",
		NoCache:    true,
		Pull:       false,
	}
	args := buildArgs(true, cfg, "ic-compose-build-test", "/tmp/out.tar")
	require.Contains(t, args, "build")
	require.Contains(t, args, "--tag")
	require.Contains(t, args, "ic-compose-build-test")
	require.Contains(t, args, "/path/to/ctx")
	require.Contains(t, args, "--file")
	require.Contains(t, args, "Containerfile")
	require.Contains(t, args, "--platform")
	require.Contains(t, args, "linux/amd64")
	require.Contains(t, args, "--target")
	require.Contains(t, args, "runtime")
	require.Contains(t, args, "--build-arg")
	require.Contains(t, args, "FOO=bar")
	require.Contains(t, args, "--no-cache")
	require.Contains(t, args, "type=tar,dest=/tmp/out.tar")
	require.NotContains(t, args, "buildx")
}

func TestBuildConfigWithInlineDockerfile(t *testing.T) {
	t.Parallel()
	cfg, cleanup, err := buildConfigWithInlineDockerfile(&BuildConfig{
		Context:          "/path/to/ctx",
		DockerfileInline: "FROM docker.io/alpine:latest\n",
	})
	require.NoError(t, err)
	defer cleanup()

	require.NotEmpty(t, cfg.Dockerfile)
	data, err := os.ReadFile(cfg.Dockerfile)
	require.NoError(t, err)
	require.Equal(t, "FROM docker.io/alpine:latest\n", string(data))
}

func TestBuildConfigWithInlineDockerfileRejectsDockerfile(t *testing.T) {
	t.Parallel()
	_, _, err := buildConfigWithInlineDockerfile(&BuildConfig{
		Dockerfile:       "Dockerfile",
		DockerfileInline: "FROM docker.io/alpine:latest\n",
	})
	require.Error(t, err)
}

func TestPutEnv(t *testing.T) {
	t.Parallel()

	env := putEnv(slices.Clone(ociDefaultEnv), "PATH=/opt/bin", true)
	require.Equal(t, []string{"PATH=/opt/bin", "TERM=xterm"}, env)

	env = putEnv(env, "TERM=dumb", false)
	require.Equal(t, []string{"PATH=/opt/bin", "TERM=xterm"}, env)

	env = putEnv(env, "HOME=/root", false)
	require.Equal(t, []string{"PATH=/opt/bin", "TERM=xterm", "HOME=/root"}, env)

	env = putEnv(env, "NOTANASSIGNMENT", true)
	require.Equal(t, []string{"PATH=/opt/bin", "TERM=xterm", "HOME=/root"}, env)
}

func writeRootfsTar(t *testing.T, files map[string]string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "rootfs.tar")
	f, err := os.Create(path)
	require.NoError(t, err)
	defer func() { require.NoError(t, f.Close()) }()

	tw := tar.NewWriter(f)
	for name, content := range files {
		require.NoError(t, tw.WriteHeader(&tar.Header{
			Name: name,
			Mode: 0o644,
			Size: int64(len(content)),
		}))
		_, err = tw.Write([]byte(content))
		require.NoError(t, err)
	}
	require.NoError(t, tw.Close())

	return path
}

func TestRootfsHome(t *testing.T) {
	t.Parallel()

	passwd := "root:x:0:0:root:/root:/bin/sh\nnobody:x:65534:65534:nobody:/:/sbin/nologin\napp:x:1000:1000::/home/app:/bin/sh\n"

	tests := []struct {
		name  string
		files map[string]string
		uid   uint64
		home  string
	}{
		{"root", map[string]string{"etc/passwd": passwd}, 0, "/root"},
		{"named user", map[string]string{"etc/passwd": passwd}, 1000, "/home/app"},
		{"dot prefixed entry", map[string]string{"./etc/passwd": passwd}, 0, "/root"},
		{"no entry for uid", map[string]string{"etc/passwd": passwd}, 42, ""},
		{"no passwd file", map[string]string{"etc/hosts": "127.0.0.1 localhost\n"}, 0, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			home, err := rootfsHome(writeRootfsTar(t, tt.files), tt.uid)
			require.NoError(t, err)
			require.Equal(t, tt.home, home)
		})
	}
}

func TestBuildArgs_Docker(t *testing.T) {
	t.Parallel()
	cfg := &BuildConfig{
		Context: "/path/to/ctx",
		Pull:    true,
	}
	args := buildArgs(false, cfg, "ic-compose-build-test", "/tmp/out.tar")
	require.Contains(t, args, "buildx")
	require.Contains(t, args, "build")
	require.Contains(t, args, "--pull")
	require.NotContains(t, args, "--no-cache")
}
