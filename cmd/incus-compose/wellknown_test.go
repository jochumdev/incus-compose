package main

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/lxc/incus-compose/internal/testlib"
)

func TestWellKnownRegistryQuayIO(t *testing.T) {
	t.Parallel()
	testlib.SkipLocal(t)

	ctx := t.Context()
	pn := t.Name()

	dir := testlib.WriteTempFiles(t, map[string]string{
		"compose.yaml": `services:
  hello:
    image: quay.io/podman/hello
`,
	})
	compose := filepath.Join(dir, "compose.yaml")

	t.Cleanup(func() {
		_, _ = testlib.RunCompose(context.Background(), t, pn, "", nil, "-f", compose, "down", "--project")
	})

	_, err := testlib.RunCompose(ctx, t, pn, "", nil, "-f", compose, "pull", "--no-healthd", "hello")
	require.NoError(t, err)
}

func TestWellKnownRegistryMCR(t *testing.T) {
	t.Parallel()
	testlib.SkipLocal(t)

	ctx := t.Context()
	pn := t.Name()

	dir := testlib.WriteTempFiles(t, map[string]string{
		"compose.yaml": `services:
  hello:
    image: mcr.microsoft.com/azurelinux/busybox:1.36
`,
	})
	compose := filepath.Join(dir, "compose.yaml")

	t.Cleanup(func() {
		_, _ = testlib.RunCompose(context.Background(), t, pn, "", nil, "-f", compose, "down", "--project")
	})

	_, err := testlib.RunCompose(ctx, t, pn, "", nil, "-f", compose, "pull", "--no-healthd", "hello")
	require.NoError(t, err)
}
