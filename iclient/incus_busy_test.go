package iclient

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/lxc/incus/v7/shared/api"
	"github.com/stretchr/testify/require"

	"github.com/lxc/incus-compose/internal/testlib"
)

// busyMessage is what incusd sends, verbatim from operationlock. The quotes
// around the action are upstream's, so they have to be escaped on the wire.
const (
	busyMessage     = `Instance is busy running a "start" operation`
	busyMessageJSON = `Instance is busy running a \"start\" operation`
)

// TestIncusBusyErrorOnASyncResponse: the lock is reported as a plain message,
// so the only thing that makes it matchable is the wrapping done here.
func TestIncusBusyErrorOnASyncResponse(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w,
			`{"type":"error","error_code":500,"error":"`+busyMessageJSON+`"}`)
	}))
	t.Cleanup(server.Close)

	conn, err := NewConnection(&ConfigRemoteInfo{Name: "busy", Addrs: []string{server.URL}})
	require.NoError(t, err)

	_, _, err = conn.GetInstanceState(t.Context(), "web-1")

	require.ErrorIs(t, err, ErrInstanceBusy)
	require.Contains(t, err.Error(), busyMessage, "the server's own wording must survive")
}

// TestIncusBusyErrorOnAnOperation is the case that actually happens: Incus
// takes the lock in the driver, so the request is accepted and the operation
// is what fails.
func TestIncusBusyErrorOnAnOperation(t *testing.T) {
	t.Parallel()

	last := api.Operation{
		ID:         "op-1",
		Status:     "Failure",
		StatusCode: api.Failure,
		Err:        busyMessage,
	}

	updates := make(chan api.Operation, 1)
	updates <- last
	close(updates)

	_, err := WaitOperation(t.Context(), updates)

	require.ErrorIs(t, err, ErrInstanceBusy)
}

// TestIncusBusyErrorLeavesOtherFailuresAlone guards the match: everything that
// is not the lock has to stay unmatchable, or a caller retries forever.
func TestIncusBusyErrorLeavesOtherFailuresAlone(t *testing.T) {
	t.Parallel()

	updates := make(chan api.Operation, 1)
	updates <- api.Operation{ID: "op-1", StatusCode: api.Failure, Err: "No such object"}
	close(updates)

	_, err := WaitOperation(t.Context(), updates)

	require.Error(t, err)
	require.NotErrorIs(t, err, ErrInstanceBusy)
}

// TestIncusWaitInstanceBusyHonoursTheContext: the caller's deadline is the only
// bound on the loop. The holder is a project-scoped resource URL, which a match
// on the bare path would report as free.
func TestIncusWaitInstanceBusyHonoursTheContext(t *testing.T) {
	t.Parallel()

	// A task operation that never reaches a final state, so the lock is never freed.
	const running = `{"id":"op-1","class":"task","status_code":103,
		"resources":{"instances":["/1.0/instances/web-1?project=myproject"]}}`

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		body := `{"running":[` + running + `]}`
		if strings.HasSuffix(r.URL.Path, "/wait") {
			body = running
		}

		_, _ = io.WriteString(w, `{"type":"sync","status_code":200,"metadata":`+body+`}`)
	}))
	t.Cleanup(server.Close)

	conn, err := NewConnection(&ConfigRemoteInfo{
		Name:    "busy",
		Addrs:   []string{server.URL},
		Project: "myproject",
	})
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()

	err = conn.WaitInstanceBusy(ctx, "web-1")

	require.ErrorIs(t, err, context.DeadlineExceeded)
}

// TestIncusWaitInstanceBusyReturnsWhenNothingHoldsIt covers the common path,
// where the instance is free and the call is one listing.
func TestIncusWaitInstanceBusyReturnsWhenNothingHoldsIt(t *testing.T) {
	t.Parallel()

	// An operation on a different instance, so the filter has something to reject.
	conn, seen := recordingServer(t, `{"running":[{
		"id":"op-1","class":"task","status_code":103,
		"resources":{"instances":["/1.0/instances/other-1"]}}]}`)

	require.NoError(t, conn.WaitInstanceBusy(t.Context(), "web-1"))
	require.Len(t, seen.all(), 1, "a free instance costs one listing and no operation wait")
}

// TestE2EWaitInstanceBusyWaitsOutAnOperation is the contract against a real
// server: it returns only once the operation holding the instance is done.
func TestE2EWaitInstanceBusyWaitsOutAnOperation(t *testing.T) {
	testlib.SkipE2E(t)
	t.Parallel()

	ctx := t.Context()
	conn := testConnection(t)

	project := testProject(t, conn, "iclient-busy")
	projectConn := conn.WithProject(project)

	const name = "busy-1"

	testInstance(t, projectConn, name, nil)

	// Fired without waiting, so the lock is held while the call below runs.
	_, err := projectConn.UpdateInstanceState(ctx, name,
		api.InstanceStatePut{Action: "stop", Force: true, Timeout: -1}, "")
	require.NoError(t, err)

	require.NoError(t, projectConn.WaitInstanceBusy(ctx, name))

	// An instance still stopping answers "Invalid PID -1" rather than a status.
	state, _, err := projectConn.GetInstanceState(ctx, name)
	require.NoError(t, err,
		"WaitInstanceBusy returned while the stop was still running, leaving the instance mid-transition")
	require.Equal(t, "Stopped", state.Status,
		"WaitInstanceBusy returned before the stop it should have waited for finished")
}
