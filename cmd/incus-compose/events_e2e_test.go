package main

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lxc/incus-compose/internal/testlib"
)

// TestE2EEventsRejects covers the two arguments events turns down before it
// shells out.
func TestE2EEventsRejects(t *testing.T) {
	t.Parallel()
	testlib.SkipE2E(t)

	ctx := t.Context()
	pn := t.Name()

	dir := testlib.WriteTempFiles(t, map[string]string{
		"compose.yaml": `services:
  web:
    image: docker.io/alpine:edge
`,
	})
	compose := filepath.Join(dir, "compose.yaml")

	// errLogged carries the error the CLI already reported, so what reaches the
	// user is the logged line and never the error's own text.
	_, err := testlib.RunCompose(ctx, t, pn, "", nil, "-f", compose, "events", "web")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "incus monitor cannot filter by service")

	_, err = testlib.RunCompose(ctx, t, pn, "", nil, "-f", compose, "events", "--format", "xml")
	require.Error(t, err)
	assert.Contains(t, err.Error(), `unknown format "xml"`)
}

// TestE2ETopRejectsServiceArgs is all of top that can be asserted without a
// terminal: incus top renders nothing when stdout is not a TTY, and refreshes
// until interrupted rather than exiting.
func TestE2ETopRejectsServiceArgs(t *testing.T) {
	t.Parallel()
	testlib.SkipE2E(t)

	ctx := t.Context()
	pn := t.Name()

	dir := testlib.WriteTempFiles(t, map[string]string{
		"compose.yaml": `services:
  web:
    image: docker.io/alpine:edge
`,
	})
	compose := filepath.Join(dir, "compose.yaml")

	_, err := testlib.RunCompose(ctx, t, pn, "", nil, "-f", compose, "top", "web")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "incus top cannot filter by service")
}
