package client

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// A resource whose state is not seeded segfaults on the first IsEnsured(), far
// from the constructor that forgot it.
func TestResourceStateIsSeeded(t *testing.T) {
	c := NewOfflineClient(t.Context(), "state-test")

	cases := []struct {
		kind   Kind
		name   string
		config Config
	}{
		{KindProfile, "p", &ProfileConfig{}},
		{KindImage, "docker.io/library/alpine:latest", &ImageConfig{}},
		{KindNetwork, "n", &NetworkConfig{}},
		{KindStorageVolume, "v", &StorageVolumeConfig{}},
		{KindInstance, "i", &InstanceConfig{}},
	}

	for _, tc := range cases {
		t.Run(string(tc.kind), func(t *testing.T) {
			r, err := c.Resource(tc.kind, tc.name, tc.config)
			require.NoError(t, err)

			require.False(t, r.IsEnsured())
		})
	}
}
