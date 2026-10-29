package client

import (
	"fmt"
	"testing"
	"time"

	incusApi "github.com/lxc/incus/v7/shared/api"
	"github.com/stretchr/testify/require"
)

func TestEmitProgress(t *testing.T) {
	t.Parallel()

	r := newMockResource("web", KindInstance, 0, true)

	t.Run("forwards to handler", func(t *testing.T) {
		t.Parallel()

		var got Progress
		var gotAction Action
		var gotResource Resource
		gc := &GlobalClient{progressHandler: func(a Action, res Resource, _ Options, p Progress) {
			gotAction, gotResource, got = a, res, p
		}}

		want := Progress{Percent: -1, Text: "Waiting for dependency db"}
		gc.emitProgress(ActionStart, r, Options{}, want)

		require.Equal(t, ActionStart, gotAction)
		require.Same(t, r, gotResource)
		require.Equal(t, want, got)
	})

	t.Run("no handler is a no-op", func(t *testing.T) {
		t.Parallel()

		gc := &GlobalClient{}
		require.NotPanics(t, func() {
			gc.emitProgress(ActionStart, r, Options{}, Progress{Percent: -1, Text: "x"})
		})
	})
}

func TestParsePercent(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   string
		want int
	}{
		{"native rootfs", "rootfs: 42% (3.10MB/s)", 42},
		{"native complete", "metadata: 100% (876B/s)", 100},
		{"leading zero progress", "rootfs: 0% (0B/s)", 0},
		{"oci status text", "Retrieving OCI image from registry", -1},
		{"oci tarball bytes", "Generating rootfs tarball: 12.3MB (4.1MB/s)", -1},
		{"empty", "", -1},
		{"bare percent sign", "%", -1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			require.Equal(t, tt.want, parsePercent(tt.in))
		})
	}
}

func TestDetectOVN_Requires75Extension(t *testing.T) {
	t.Parallel()

	gc := &GlobalClient{}
	supported, err := gc.DetectOVN()
	require.NoError(t, err)
	require.False(t, supported)
}

// TestDetectOVNMatchesProbe holds the cheap checks to the throwaway OVN network
// they skip: CI runs it against servers with OVN and without.
func TestDetectOVNMatchesProbe(t *testing.T) {
	t.Parallel()
	skipLocal(t)
	ctx := t.Context()

	gc, err := NewTestClient(testContext(t))
	require.NoError(t, err)

	got, err := gc.DetectOVN()
	require.NoError(t, err)

	conn, err := gc.Connection()
	require.NoError(t, err)

	name := fmt.Sprintf("ic-probe-test-%x", time.Now().UnixNano())
	createErr := conn.CreateNetwork(ctx, "default", incusApi.NetworksPost{
		Name:       name,
		Type:       "ovn",
		NetworkPut: incusApi.NetworkPut{Config: map[string]string{"network": "none"}},
	}, nil)
	if createErr == nil {
		require.NoError(t, conn.DeleteNetwork(ctx, "default", name))
	}

	require.Equal(t, createErr == nil, got, "probe error: %v", createErr)
}
