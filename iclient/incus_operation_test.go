package iclient

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/lxc/incus/v7/shared/api"
	"github.com/stretchr/testify/require"
)

// operationServer answers /1.0/events over a websocket and every other path
// with whatever the handler returns, so a test can drive an operation's life.
type operationServer struct {
	recorder
	subscribe chan struct{}
	events    chan api.Operation
}

// newOperationServer returns a Connection and the server behind it. The async
// reply carries op; later updates go through send.
func newOperationServer(t *testing.T, op api.Operation) (*Connection, *operationServer) {
	t.Helper()

	s := &operationServer{
		subscribe: make(chan struct{}, 8),
		events:    make(chan api.Operation, 8),
	}

	upgrader := websocket.Upgrader{}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.add(r)

		if strings.HasSuffix(r.URL.Path, "/events") {
			socket, err := upgrader.Upgrade(w, r, nil)
			if err != nil {
				return
			}

			defer func() { _ = socket.Close() }()

			s.subscribe <- struct{}{}

			for update := range s.events {
				metadata, err := json.Marshal(update)
				if err != nil {
					return
				}

				event, err := json.Marshal(api.Event{Type: api.EventTypeOperation, Metadata: metadata})
				if err != nil {
					return
				}

				err = socket.WriteMessage(websocket.TextMessage, event)
				if err != nil {
					return
				}
			}

			return
		}

		if r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/operations/") {
			metadata, _ := json.Marshal(op)
			_, _ = io.WriteString(w, `{"type":"sync","status_code":200,"metadata":`+string(metadata)+`}`)

			return
		}

		metadata, _ := json.Marshal(op)
		_, _ = io.WriteString(w,
			`{"type":"async","status_code":100,"operation":"/1.0/operations/`+op.ID+`","metadata":`+string(metadata)+`}`)
	}))

	t.Cleanup(func() {
		close(s.events)
		server.Close()
	})

	conn, err := NewConnection(&ConfigRemoteInfo{
		Name:  "operations",
		Addrs: []string{server.URL},
	})
	require.NoError(t, err)

	return conn, s
}

func running(id string) api.Operation {
	return api.Operation{ID: id, StatusCode: api.Running, Status: "Running"}
}

// TestIncusAsyncOperationSubscribesFirst is the ordering guarantee: the event
// listener has to be open before the request goes out, or an operation that
// finishes at once is never reported.
func TestIncusAsyncOperationSubscribesFirst(t *testing.T) {
	t.Parallel()

	conn, server := newOperationServer(t, running("op-1"))

	updates, err := conn.DeleteInstance(t.Context(), "myproject", "web-1")
	require.NoError(t, err)
	require.NotNil(t, updates)

	seen := server.all()
	require.GreaterOrEqual(t, len(seen), 2)
	require.Equal(t, "/1.0/events", seen[0].url.Path, "the listener must open before the request")
	require.Equal(t, "/1.0/instances/web-1", seen[1].url.Path)
}

// TestIncusAsyncOperationListensOnTheCallsProject is the whole reason project
// is threaded down to async: incusd drops an operation event whose project is
// not the listener's, so a listener anywhere else leaves the caller blocked on
// updates that never come.
func TestIncusAsyncOperationListensOnTheCallsProject(t *testing.T) {
	t.Parallel()

	conn, server := newOperationServer(t, running("op-1"))

	_, err := conn.DeleteInstance(t.Context(), "second", "web-1")
	require.NoError(t, err)

	seen := server.all()
	require.GreaterOrEqual(t, len(seen), 2)

	require.Equal(t, "/1.0/events", seen[0].url.Path)
	require.Equal(t, "second", seen[0].url.Query().Get("project"),
		"the listener must be scoped to the project the operation runs in")
	require.Equal(t, "second", seen[1].url.Query().Get("project"))
}

// TestIncusAsyncOperationFirstValue: the caller gets the operation as accepted
// before it finishes, which is what ExecInstance needs for its fds secrets.
func TestIncusAsyncOperationFirstValue(t *testing.T) {
	t.Parallel()

	started := running("op-1")
	started.Metadata = map[string]any{"fds": map[string]any{"0": "secret"}}

	conn, _ := newOperationServer(t, started)

	updates, err := conn.CreateInstance(t.Context(), "myproject", api.InstancesPost{Name: "web-1"})
	require.NoError(t, err)

	select {
	case first := <-updates:
		require.Equal(t, "op-1", first.ID)
		require.NotNil(t, first.Metadata["fds"], "the accepted operation carries its metadata")
	case <-time.After(5 * time.Second):
		t.Fatal("no first value")
	}
}

// TestIncusAsyncOperationClosesOnTerminal: waiting is ranging until the close,
// and the last value is the outcome.
func TestIncusAsyncOperationClosesOnTerminal(t *testing.T) {
	t.Parallel()

	conn, server := newOperationServer(t, running("op-1"))

	updates, err := conn.UpdateInstanceState(t.Context(), "myproject", "web-1", api.InstanceStatePut{Action: "start"}, "")
	require.NoError(t, err)

	<-server.subscribe

	server.events <- api.Operation{ID: "op-1", StatusCode: api.Failure, Status: "Failure", Err: "boom"}

	seen := []api.Operation{}
	for update := range updates {
		seen = append(seen, update)
	}

	require.GreaterOrEqual(t, len(seen), 2)

	last := seen[len(seen)-1]
	require.True(t, last.StatusCode.IsFinal())
	require.Equal(t, "boom", last.Err, "the outcome arrives as the last value, not a second error")
}

// TestIncusAsyncOperationIgnoresOthers: the event stream carries every
// operation, so updates for somebody else's must not leak into this channel.
func TestIncusAsyncOperationIgnoresOthers(t *testing.T) {
	t.Parallel()

	conn, server := newOperationServer(t, running("op-1"))

	updates, err := conn.DeleteInstance(t.Context(), "myproject", "web-1")
	require.NoError(t, err)

	<-server.subscribe
	<-updates

	server.events <- running("op-other")
	server.events <- api.Operation{ID: "op-1", StatusCode: api.Success, Status: "Success"}

	select {
	case update := <-updates:
		require.Equal(t, "op-1", update.ID, "another operation's update leaked in")
		require.True(t, update.StatusCode.IsFinal())
	case <-time.After(5 * time.Second):
		t.Fatal("the operation never completed")
	}
}

// TestIncusAsyncOperationAlreadyFinished: an operation the server reports as
// done in its own reply closes at once rather than waiting for an event.
func TestIncusAsyncOperationAlreadyFinished(t *testing.T) {
	t.Parallel()

	conn, _ := newOperationServer(t, api.Operation{ID: "op-1", StatusCode: api.Success, Status: "Success"})

	updates, err := conn.DeleteInstance(t.Context(), "myproject", "web-1")
	require.NoError(t, err)

	seen := 0

	for range updates {
		seen++
	}

	require.Equal(t, 1, seen, "a finished operation reports once and closes")
}

func TestIncusListenOperationCatchesUp(t *testing.T) {
	t.Parallel()

	conn, server := newOperationServer(t, api.Operation{ID: "op-1", StatusCode: api.Success, Status: "Success"})

	updates, err := conn.ListenOperation(t.Context(), "myproject", api.Operation{ID: "op-1"})
	require.NoError(t, err)

	seen := 0

	for range updates {
		seen++
	}

	require.Equal(t, 1, seen, "the catch-up read reports an operation that already finished")

	requests := server.all()
	require.Equal(t, "/1.0/events", requests[0].url.Path, "subscribe before the catch-up read")
	require.Equal(t, "/1.0/operations/op-1", requests[1].url.Path)
}

func TestIncusCancelOperation(t *testing.T) {
	t.Parallel()

	conn, seen := recordingServer(t, `{}`)

	require.NoError(t, conn.CancelOperation(t.Context(), "myproject", api.Operation{ID: "op-1"}))

	req := seen.all()[0]
	require.Equal(t, http.MethodDelete, req.method)
	require.Equal(t, "/1.0/operations/op-1?project=myproject", req.uri())
}

func TestIncusGetOperationsFlattens(t *testing.T) {
	t.Parallel()

	conn, seen := recordingServer(t, `{"running":[{"id":"a"}],"success":[{"id":"b"},{"id":"c"}]}`)

	operations, err := conn.GetOperations(t.Context(), "myproject")
	require.NoError(t, err)

	// The collection is grouped by status; the caller wants one list.
	require.Len(t, operations, 3)

	// Without recursion the entries would be URLs, not operations.
	require.Equal(t, []string{"/1.0/operations?project=myproject&recursion=1"}, seen.uris())
}

func TestIncusOperationsAgainstRealIncus(t *testing.T) {
	skipLocal(t)
	t.Parallel()

	conn := testConnection(t)

	operations, err := conn.GetOperations(t.Context(), "myproject")
	require.NoError(t, err)

	for _, op := range operations {
		require.NotEmpty(t, op.ID)
	}
}
