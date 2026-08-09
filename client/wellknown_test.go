package client

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/lxc/incus-compose/iclient"
)

// wellKnownImage builds an Image directly, bypassing the resource store.
func wellKnownImage(t *testing.T, remote, image string) *Image {
	t.Helper()

	return &Image{
		BaseResource: NewBaseResource(KindImage, remote+"/"+image, PriorityImage),
		client:       NewOfflineClient(t.Context(), "wellknown"),
		incusName:    remote + "/" + image,
		remote:       remote,
		image:        image,
	}
}

func TestWellKnownRegistryResolves(t *testing.T) {
	img := wellKnownImage(t, "ghcr.io", "lxc/incus-compose/ic-healthd:latest")

	delete(img.client.Global().CliConfig().Remotes, "ghcr.io")

	source, err := img.resolveSource()
	require.NoError(t, err)

	require.Equal(t, "https://ghcr.io", source.server)
	require.Equal(t, "oci", source.protocol)
	require.Nil(t, source.conn, "a registry is pointed at, never dialed")
}

func TestWellKnownRegistrySkipsUnknown(t *testing.T) {
	img := wellKnownImage(t, "unknown.registry.example.com", "image:tag")

	_, err := img.resolveSource()
	require.ErrorIs(t, err, ErrImageSource)
}

func TestWellKnownRegistryConfiguredWins(t *testing.T) {
	img := wellKnownImage(t, "ghcr.io", "something:latest")

	img.client.Global().CliConfig().Remotes["ghcr.io"] = iclient.ConfigRemote{
		Addrs:    []string{"https://custom.example.com"},
		Protocol: "oci",
		Public:   true,
	}

	source, err := img.resolveSource()
	require.NoError(t, err)

	require.Equal(t, "https://custom.example.com", source.server)
	require.Equal(t, "oci", source.protocol)
}
