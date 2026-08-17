package main

import (
	"encoding/json"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"

	incusApi "github.com/lxc/incus/v7/shared/api"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lxc/incus-compose/client"
	"github.com/lxc/incus-compose/internal/testlib"
)

// TestE2ECommandReplacesImageCmd pins compose `command:` against an image
// carrying both an ENTRYPOINT and a CMD.
func TestE2ECommandReplacesImageCmd(t *testing.T) {
	t.Parallel()
	testlib.SkipLocal(t)
	testlib.SkipE2E(t)

	ctx := t.Context()
	pn := t.Name()
	compose := testlib.Fixture(t, "with-command", "compose.yaml")

	testlib.CleanupCompose(t, pn, "-f", compose, "down", "--project")

	_, err := testlib.RunCompose(ctx, t, pn, "", nil, "-f", compose, "up", "--detach")
	require.NoError(t, err)

	entrypoint, err := testlib.RunCompose(ctx, t, pn, "", nil, "-f", compose, "incus", "config", "get", "cache-1", "oci.entrypoint")
	require.NoError(t, err)
	assert.Equal(t, "docker-entrypoint.sh sh", strings.TrimSpace(entrypoint))

	// Only running because that argv is one memcached's entrypoint can exec.
	stdout, err := testlib.RunCompose(ctx, t, pn, "", nil, "-f", compose, "ps", "--format", "json")
	require.NoError(t, err)

	var services []struct {
		Service string `json:"service"`
		Status  string `json:"status"`
	}
	require.NoError(t, json.Unmarshal([]byte(stdout), &services))
	require.Len(t, services, 1)
	assert.Equal(t, "cache", services[0].Service)
	assert.Equal(t, "Running", services[0].Status)
}

// TestE2EImageCarriesTheOCISplit checks the properties.
func TestE2EImageCarriesTheOCISplit(t *testing.T) {
	t.Parallel()
	testlib.SkipLocal(t)
	testlib.SkipE2E(t)

	ctx := t.Context()
	pn := t.Name()
	compose := testlib.Fixture(t, "with-command", "compose.yaml")

	testlib.CleanupCompose(t, pn, "-f", compose, "down", "--project")

	_, err := testlib.RunCompose(ctx, t, pn, "", nil, "-f", compose, "pull")
	require.NoError(t, err)

	stdout, err := testlib.RunCompose(ctx, t, pn, "", nil, "-f", compose, "incus", "image", "list", "--format=json")
	require.NoError(t, err)

	var images []struct {
		Properties map[string]string `json:"properties"`
	}
	require.NoError(t, json.Unmarshal([]byte(stdout), &images))
	require.Len(t, images, 1)

	props := images[0].Properties
	assert.Equal(t, "docker-entrypoint.sh", props["oci.entrypoint"])
	assert.Equal(t, "memcached", props["oci.cmd"])

	// Incus keeps no property whose value is empty, so memcached declaring no
	// VOLUME leaves no oci.volumes at all.
	assert.NotContains(t, props, "oci.volumes")
}

// TestE2EUpgradesAnAgedCacheEntry pins that upgrading incus-compose over an
// existing cache stays seamless.
func TestE2EUpgradesAnAgedCacheEntry(t *testing.T) {
	t.Parallel()
	testlib.SkipLocal(t)
	testlib.SkipE2E(t)

	ctx := t.Context()
	alias := "docker.io/library/memcached:1.6.21-alpine"
	compose := filepath.Join(testlib.WriteTempFiles(t, map[string]string{
		"compose.yaml": "services:\n  cache:\n    image: " + alias + "\n    command: sh\n",
	}), "compose.yaml")

	// Seeding and using the cache are separate projects: a project already
	// holding the image never reads the entry being aged.
	seed := t.Name() + "Seed"
	pn := t.Name()

	testlib.CleanupCompose(t, seed, "-f", compose, "down", "--project")
	testlib.CleanupCompose(t, pn, "-f", compose, "down", "--project")

	// pull is PullAlways, so this leaves a current entry to age by hand.
	_, err := testlib.RunCompose(ctx, t, seed, "", nil, "-f", compose, "pull")
	require.NoError(t, err)

	cacheProject := "incus-compose-tests-cache"
	if p, ok := os.LookupEnv("INCUS_COMPOSE_IMAGE_CACHE"); ok {
		cacheProject = p
	}

	gc, err := client.NewTestClient(ctx)
	require.NoError(t, err)

	conn, err := gc.Connection()
	require.NoError(t, err)

	cache := conn.WithProject(cacheProject)

	cached, _, err := cache.GetImageAlias(ctx, alias, nil)
	require.NoError(t, err, "the pull above should have left %q in the cache", alias)

	img, eTag, err := cache.GetImage(ctx, cached.Target, nil)
	require.NoError(t, err)

	// What an incus-compose before the split left behind.
	props := maps.Clone(img.Properties)
	props["oci.entrypoint"] = "docker-entrypoint.sh memcached"
	delete(props, "oci.cmd")
	delete(props, "oci.volumes")

	require.NoError(t, cache.UpdateImage(ctx, cached.Target, incusApi.ImagePut{
		AutoUpdate: img.AutoUpdate,
		Properties: props,
		Public:     img.Public,
		ExpiresAt:  img.ExpiresAt,
		Profiles:   img.Profiles,
	}, eTag))

	_, err = testlib.RunCompose(ctx, t, pn, "", nil, "-f", compose, "up", "--detach")
	require.NoError(t, err)

	entrypoint, err := testlib.RunCompose(ctx, t, pn, "", nil, "-f", compose, "incus", "config", "get", "cache-1", "oci.entrypoint")
	require.NoError(t, err)
	assert.Equal(t, "docker-entrypoint.sh sh", strings.TrimSpace(entrypoint),
		"an aged cache entry still resolved command: the old way")

	// And the entry itself is current again, so the next project reuses it.
	img, _, err = cache.GetImage(ctx, cached.Target, nil)
	require.NoError(t, err)
	assert.Equal(t, "docker-entrypoint.sh", img.Properties["oci.entrypoint"])
	assert.Equal(t, "memcached", img.Properties["oci.cmd"])
}
