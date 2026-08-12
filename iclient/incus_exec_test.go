package iclient

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/lxc/incus/v7/shared/api"
	"github.com/stretchr/testify/require"

	"github.com/lxc/incus-compose/testlib"
)

// TestIncusExecInstanceRefusesInteractive: a PTY needs control and resize
// handling that is not implemented, so it is refused rather than half-done.
func TestIncusExecInstanceRefusesInteractive(t *testing.T) {
	t.Parallel()

	conn, seen := recordingServer(t, `{}`)

	_, err := conn.ExecInstance(t.Context(), "web-1", api.InstanceExecPost{
		Command:     []string{"true"},
		Interactive: true,
	}, nil)

	require.ErrorIs(t, err, ErrConnectionUnsupported)
	require.Empty(t, seen.all(), "it must refuse before asking the server")
}

// testInstance brings up a container of its own, carrying config.
func testInstance(t *testing.T, conn *Connection, name string, config map[string]string) {
	t.Helper()

	ctx := t.Context()

	cliConfig, err := ReadConfig("")
	require.NoError(t, err)

	registry, err := cliConfig.RemoteInfos("docker.io")
	require.NoError(t, err)

	// A fresh project's default profile carries no devices, so the root disk
	// has to be named or Incus refuses with "No root device could be found".
	pools, err := conn.GetStoragePoolNames(ctx)
	require.NoError(t, err)
	require.NotEmpty(t, pools)

	createCtx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()

	// Straight from the registry: incusd pulls the image as part of creating
	// the instance, so this needs no separate image step.
	updates, err := conn.CreateInstance(createCtx, api.InstancesPost{
		Name: name,
		Type: api.InstanceTypeContainer,
		InstancePut: api.InstancePut{
			Config: config,
			Devices: map[string]map[string]string{
				"root": {"type": "disk", "path": "/", "pool": pools[0]},
			},
		},
		Source: api.InstanceSource{
			Type:        "image",
			Alias:       "library/alpine:latest",
			Server:      registry.Addrs[0],
			Protocol:    registry.Protocol,
			Certificate: registry.ServerCert,
		},
	})
	require.NoError(t, err)

	_, err = WaitOperation(createCtx, updates)
	require.NoError(t, err)

	t.Cleanup(func() {
		// Registered after the project's cleanup, so it runs before it.
		stop, err := conn.UpdateInstanceState(context.Background(), name,
			api.InstanceStatePut{Action: "stop", Force: true, Timeout: -1}, "")
		if err == nil {
			_, _ = WaitOperation(context.Background(), stop)
		}
	})

	updates, err = conn.UpdateInstanceState(ctx, name,
		api.InstanceStatePut{Action: "start", Timeout: -1}, "")
	require.NoError(t, err)

	_, err = WaitOperation(ctx, updates)
	require.NoError(t, err)

	state, _, err := conn.GetInstanceState(ctx, name)
	require.NoError(t, err)
	require.Equal(t, "Running", state.Status)
}

// TestIncusExecInstanceAgainstRealIncus runs a command and reads its output
// and exit code, which is the whole of what healthd needs.
func TestIncusExecInstanceAgainstRealIncus(t *testing.T) {
	testlib.SkipE2E(t)
	t.Parallel()

	ctx := t.Context()
	conn := testConnection(t)

	project := testProject(t, conn, "iclient-exec")
	projectConn := conn.WithProject(project)

	const name = "exec-1"

	testInstance(t, projectConn, name, nil)

	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}

	updates, err := projectConn.ExecInstance(ctx, name, api.InstanceExecPost{
		Command: []string{"sh", "-c", "echo out; echo err >&2; exit 7"},
	}, &InstanceExecArgs{Stdout: stdout, Stderr: stderr})
	require.NoError(t, err)

	op, err := WaitOperation(ctx, updates)
	require.NoError(t, err)

	// The channel has closed, so the streams have drained: no separate wait.
	require.Contains(t, stdout.String(), "out")
	require.Contains(t, stderr.String(), "err")
	require.NotContains(t, stdout.String(), "err", "the streams must not be merged")

	code, ok := op.Metadata["return"].(float64)
	require.True(t, ok, "no exit code in the operation metadata: %+v", op.Metadata)
	require.Equal(t, 7, int(code))
}
