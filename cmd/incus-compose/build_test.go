package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/lxc/incus-compose/client"
	"github.com/lxc/incus-compose/internal/testlib"
)

func skipIfNoBuilder(t *testing.T) {
	t.Helper()
	if override := os.Getenv("INCUS_COMPOSE_BUILD_BUILDER"); override != "" {
		if _, err := exec.LookPath(override); err != nil {
			t.Skipf("Skipping: INCUS_COMPOSE_BUILD_BUILDER=%q not found", override)
		}
		return
	}
	if _, err := exec.LookPath("podman"); err == nil {
		return
	}
	if _, err := exec.LookPath("docker"); err == nil {
		return
	}
	t.Skip("Skipping: podman or docker not found")
}

func TestBuildCommandWithBuildFixture(t *testing.T) {
	testlib.SkipE2E(t)
	testlib.SkipLocal(t)
	skipIfNoBuilder(t)
	t.Parallel()

	ctx := t.Context()
	pn := t.Name()
	fixture := testlib.Fixture(t, "with-build", "compose.yaml")

	testlib.CleanupCompose(t, pn, "-f", fixture, "down", "--project")

	_, err := testlib.RunCompose(ctx, t, pn, "", nil, "-f", fixture, "build")
	require.NoError(t, err)

	c := projectClient(ctx, t, pn)

	r, err := c.Resource(client.KindImage, "localhost/app:latest", &client.ImageConfig{})
	require.NoError(t, err)
	require.NoError(t, client.RunAction(ctx, r, client.ActionEnsure))

	r, err = c.Resource(client.KindImage, "localhost/app2:latest", &client.ImageConfig{})
	require.NoError(t, err)
	require.NoError(t, client.RunAction(ctx, r, client.ActionEnsure))
}

func TestBuildCommandWithServiceFilter(t *testing.T) {
	testlib.SkipE2E(t)
	testlib.SkipLocal(t)
	skipIfNoBuilder(t)
	t.Parallel()

	ctx := t.Context()
	pn := t.Name()
	dir := testlib.WriteTempFiles(t, map[string]string{
		"Dockerfile": `FROM docker.io/alpine:latest AS runtime
RUN echo "built by incus-compose"
`,
		"compose.yaml": `services:
  app:
    build:
      no_cache: true
      context: .
      target: runtime
  app2:
    build:
      no_cache: true
      context: .
      dockerfile_inline: |
        FROM docker.io/alpine:latest
        RUN echo "built inline by incus-compose"
`})
	compose := filepath.Join(dir, "compose.yaml")

	testlib.CleanupCompose(t, pn, "-f", compose, "down", "--project")

	_, err := testlib.RunCompose(ctx, t, pn, "", nil, "-f", compose, "build", "app")
	require.NoError(t, err)

	c := projectClient(ctx, t, pn)
	r, err := c.Resource(client.KindImage, "localhost/app:latest", &client.ImageConfig{})
	require.NoError(t, err)
	require.NoError(t, client.RunAction(ctx, r, client.ActionEnsure))

	r, err = c.Resource(client.KindImage, "localhost/app2:latest", &client.ImageConfig{})
	require.NoError(t, err)
	require.Error(t, client.RunAction(ctx, r, client.ActionEnsure))
}

// TestE2EBuildImageEnvironment pins the built image to the environment.* keys
// Incus derives itself when it unpacks a pulled OCI image.
func TestE2EBuildImageEnvironment(t *testing.T) {
	testlib.SkipE2E(t)
	testlib.SkipLocal(t)
	skipIfNoBuilder(t)
	t.Parallel()

	ctx := t.Context()
	pn := t.Name()
	dir := testlib.WriteTempFiles(t, map[string]string{
		"Dockerfile": `FROM docker.io/alpine:latest
ENV GREETING=hello
ENV PATH=/opt/bin:/usr/bin
`,
		"compose.yaml": `services:
  app:
    build:
      no_cache: true
      context: .
`})
	compose := filepath.Join(dir, "compose.yaml")

	testlib.CleanupCompose(t, pn, "-f", compose, "down", "--project")

	_, err := testlib.RunCompose(ctx, t, pn, "", nil, "-f", compose, "up", "--detach", "--no-start", "--no-healthd")
	require.NoError(t, err)

	c := projectClient(ctx, t, pn)
	conn, err := c.Connection()
	require.NoError(t, err)

	inst, _, err := conn.GetInstance(ctx, "app-1", nil)
	require.NoError(t, err)

	require.Equal(t, "hello", inst.Config["environment.GREETING"])
	require.Equal(t, "/opt/bin:/usr/bin", inst.Config["environment.PATH"])
	require.Equal(t, "/root", inst.Config["environment.HOME"])
	require.Equal(t, "xterm", inst.Config["environment.TERM"])
}

// TestE2EUpBuildRecreates pins that --build recreates the instances whose image
// it rebuilt, and leaves every other service running as it was.
func TestE2EUpBuildRecreates(t *testing.T) {
	testlib.SkipE2E(t)
	testlib.SkipLocal(t)
	skipIfNoBuilder(t)
	t.Parallel()

	ctx := t.Context()
	pn := t.Name()
	dir := testlib.WriteTempFiles(t, map[string]string{
		"Dockerfile": `FROM docker.io/alpine:latest
RUN echo "built by incus-compose"
`,
		"compose.yaml": `services:
  app:
    build:
      no_cache: true
      context: .
  plain:
    image: images:alpine/edge
`})
	compose := filepath.Join(dir, "compose.yaml")

	testlib.CleanupCompose(t, pn, "-f", compose, "down", "--project")

	_, err := testlib.RunCompose(ctx, t, pn, "", nil, "-f", compose, "up", "--detach", "--no-start", "--no-healthd")
	require.NoError(t, err)

	c := projectClient(ctx, t, pn)
	conn, err := c.Connection()
	require.NoError(t, err)

	uuid := func(name string) string {
		inst, _, err := conn.GetInstance(ctx, name, nil)
		require.NoError(t, err)

		return inst.Config["volatile.uuid"]
	}

	app, plain := uuid("app-1"), uuid("plain-1")
	require.NotEmpty(t, app)
	require.NotEmpty(t, plain)

	_, err = testlib.RunCompose(ctx, t, pn, "", nil, "-f", compose, "up", "--detach", "--no-start", "--no-healthd", "--build")
	require.NoError(t, err)

	require.NotEqual(t, app, uuid("app-1"), "--build must recreate the service it rebuilt")
	require.Equal(t, plain, uuid("plain-1"), "--build must leave a service it did not rebuild alone")
}

func TestBuildCommandWithNoBuildServices(t *testing.T) {
	testlib.SkipLocal(t)
	t.Parallel()

	ctx := t.Context()
	pn := t.Name()
	fixture := testlib.Fixture(t, "simple", "compose.yaml")

	testlib.CleanupCompose(t, pn, "-f", fixture, "down", "--project")

	_, err := testlib.RunCompose(ctx, t, pn, "", nil, "-f", fixture, "build")
	require.NoError(t, err)
}

func TestBuildCommandWithNoMatchingBuildServices(t *testing.T) {
	testlib.SkipLocal(t)
	t.Parallel()

	ctx := t.Context()
	pn := t.Name()
	fixture := testlib.Fixture(t, "with-build", "compose.yaml")

	testlib.CleanupCompose(t, pn, "-f", fixture, "down", "--project")

	_, err := testlib.RunCompose(ctx, t, pn, "", nil, "-f", fixture, "build", "missing")
	require.Error(t, err)
}

func TestBuildCommandWithNonBuildServiceFilter(t *testing.T) {
	testlib.SkipLocal(t)
	t.Parallel()

	ctx := t.Context()
	pn := t.Name()
	dir := testlib.WriteTempFiles(t, map[string]string{
		"compose.yaml": `services:
  app:
    build:
      no_cache: true
      context: .
  sidecar:
    image: docker.io/nginx:alpine
`,
		"Dockerfile": "FROM docker.io/alpine:latest\n",
	})
	compose := filepath.Join(dir, "compose.yaml")

	testlib.CleanupCompose(t, pn, "-f", compose, "down", "--project")

	_, err := testlib.RunCompose(ctx, t, pn, "", nil, "-f", compose, "build", "sidecar")
	require.Error(t, err)
}

func TestBuildCommandRejectsMultiplePlatforms(t *testing.T) {
	testlib.SkipLocal(t)
	t.Parallel()

	ctx := t.Context()
	pn := t.Name()
	dir := testlib.WriteTempFiles(t, map[string]string{
		"compose.yaml": `services:
  app:
    build:
      context: .
      no_cache: true
      platforms:
        - linux/amd64
        - linux/arm64
`,
		"Dockerfile": "FROM docker.io/alpine:latest\n",
	})
	compose := filepath.Join(dir, "compose.yaml")

	// down reads the same rejected compose file, so take the project directly.
	deleteProjectOnCleanup(t, pn)

	_, err := testlib.RunCompose(ctx, t, pn, "", nil, "-f", compose, "build")
	require.Error(t, err)
	require.Contains(t, err.Error(), "build.platforms with multiple platforms is not supported")
}

func TestBuildCommandRejectsUnsupportedPlatform(t *testing.T) {
	testlib.SkipLocal(t)
	t.Parallel()

	ctx := t.Context()
	pn := t.Name()
	dir := testlib.WriteTempFiles(t, map[string]string{
		"compose.yaml": `services:
  app:
    build:
      context: .
      no_cache: true
      platforms:
        - linux/unsupported
`,
		"Dockerfile": "FROM docker.io/alpine:latest\n",
	})
	compose := filepath.Join(dir, "compose.yaml")

	testlib.CleanupCompose(t, pn, "-f", compose, "down", "--project")

	_, err := testlib.RunCompose(ctx, t, pn, "", nil, "-f", compose, "build")
	require.Error(t, err)
	require.Contains(t, err.Error(), "unsupported build platform linux/unsupported")
}

func TestBuildCommandReportsMissingBuilder(t *testing.T) {
	testlib.SkipLocal(t)

	ctx := t.Context()
	pn := t.Name()
	dir := testlib.WriteTempFiles(t, map[string]string{
		"compose.yaml": `services:
  app:
    build:
      context: .
      no_cache: true
`,
		"Dockerfile": "FROM docker.io/alpine:latest\n",
	})
	compose := filepath.Join(dir, "compose.yaml")

	testlib.CleanupCompose(t, pn, "-f", compose, "down", "--project")

	_, err := testlib.RunCompose(ctx, t, pn, "", nil,
		"-f", compose, "build", "--builder", "ic-unknown-builder")
	require.Error(t, err)
	require.Contains(t, err.Error(), "no container builder")
}
