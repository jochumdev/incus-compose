package iclient

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lxc/incus/v7/shared/api"
	"github.com/stretchr/testify/require"

	"github.com/lxc/incus-compose/testlib"
)

// TestIncusConsoleInstanceRefusesWithoutOutput: there is nowhere to put the
// stream, so this is refused rather than opening sockets and dropping it.
func TestIncusConsoleInstanceRefusesWithoutOutput(t *testing.T) {
	t.Parallel()

	conn, seen := recordingServer(t, `{}`)

	_, err := conn.ConsoleInstance(t.Context(), "web-1", api.InstanceConsolePost{}, nil)
	require.Error(t, err)
	require.Empty(t, seen.all(), "it must refuse before asking the server")

	_, err = conn.ConsoleInstance(t.Context(), "web-1", api.InstanceConsolePost{}, &InstanceConsoleArgs{})
	require.Error(t, err)
	require.Empty(t, seen.all())
}

// rawServer answers with a body that is not an API envelope, the way the
// console log endpoint does.
func rawServer(t *testing.T, status int, body string) *Connection {
	t.Helper()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)

		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(server.Close)

	conn, err := NewConnection(&ConfigRemoteInfo{
		Name:    "raw",
		Addrs:   []string{server.URL},
		Project: "myproject",
	})
	require.NoError(t, err)

	return conn
}

// TestIncusGetInstanceConsoleLogRaw: the console log is plain bytes, so it must
// come back undecoded rather than through the envelope reader.
func TestIncusGetInstanceConsoleLogRaw(t *testing.T) {
	t.Parallel()

	conn := rawServer(t, http.StatusOK, "line one\nline two\n")

	reader, err := conn.GetInstanceConsoleLog(t.Context(), "web-1")
	require.NoError(t, err)

	defer func() { _ = reader.Close() }()

	body, err := io.ReadAll(reader)
	require.NoError(t, err)
	require.Equal(t, "line one\nline two\n", string(body))
}

// TestIncusGetInstanceConsoleLogError: a failure still answers with an envelope,
// so it has to reach the caller as the same StatusError any other call produces.
func TestIncusGetInstanceConsoleLogError(t *testing.T) {
	t.Parallel()

	conn := rawServer(t, http.StatusNotFound,
		`{"type":"error","error_code":404,"error":"Instance not found"}`)

	_, err := conn.GetInstanceConsoleLog(t.Context(), "nope")
	require.Error(t, err)
	require.True(t, api.StatusErrorCheck(err, 404), "want a 404 StatusError, got %v", err)
	require.Contains(t, err.Error(), "Instance not found")
}

func TestIncusGetInstanceConsoleLogURL(t *testing.T) {
	t.Parallel()

	conn, seen := recordingServer(t, `{}`)

	reader, err := conn.GetInstanceConsoleLog(t.Context(), "web-1")
	require.NoError(t, err)
	_ = reader.Close()

	require.Equal(t, []string{"/1.0/instances/web-1/console?project=myproject"}, seen.uris())
}

// TestIncusConsoleInstanceAgainstRealIncus reads a running instance's console,
// which is what `incus-compose logs` is built on.
func TestIncusConsoleInstanceAgainstRealIncus(t *testing.T) {
	testlib.SkipE2E(t)
	t.Parallel()

	ctx := t.Context()
	conn := testConnection(t)

	project := testProject(t, conn, "iclient-console")
	projectConn := conn.WithProject(project)

	const name = "console-1"

	// Something that keeps writing, so the assertion below cannot pass on an
	// instance that produced nothing.
	testInstance(t, projectConn, name, map[string]string{
		"oci.entrypoint": "sh -c 'while true; do echo iclient-console-marker; sleep 1; done'",
	})

	// The buffer holds whatever was written before the attach, so this is the
	// same stream read two ways.
	reader, err := projectConn.GetInstanceConsoleLog(ctx, name)
	require.NoError(t, err)

	_, err = io.ReadAll(reader)
	require.NoError(t, err)
	require.NoError(t, reader.Close())

	output := &syncBuffer{}

	attachCtx, detach := context.WithTimeout(ctx, 30*time.Second)
	defer detach()

	updates, err := projectConn.ConsoleInstance(attachCtx, name, api.InstanceConsolePost{Force: true},
		&InstanceConsoleArgs{Output: output})
	require.NoError(t, err)

	require.Eventually(t, func() bool {
		return strings.Contains(output.String(), "iclient-console-marker")
	}, 20*time.Second, 250*time.Millisecond, "the console stream carried nothing")

	detach()

	_, _ = WaitOperation(context.Background(), updates)
}

// syncBuffer collects the console stream, which is written from the drain
// goroutine while the test reads it.
type syncBuffer struct {
	mu   sync.Mutex
	data []byte
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.data = append(b.data, p...)

	return len(p), nil
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()

	return string(b.data)
}
