package http

import (
	"log/slog"
	"net/http/httptest"
	"testing"
	"time"

	incusapi "github.com/lxc/incus/v7/shared/api"
	"github.com/stretchr/testify/assert"

	"github.com/lxc/incus-compose/ievent/iutil"
)

// send hands one event to Handle, stamped the way the source would.
func send(p *Plugin, action string, chain iutil.ChainState) {
	p.Handle(iutil.NewEvent(time.Now(), action, "", "", "").WithChainState(chain))
}

// ready asks /ready and returns the status it answered.
func ready(t *testing.T, p *Plugin) int {
	t.Helper()

	w := httptest.NewRecorder()
	p.serveReady(w, httptest.NewRequestWithContext(t.Context(), "GET", "/ready", nil))

	return w.Code
}

// TestReadinessLatchesRatherThanLevels checks readiness latches on the chain state instead of tracking it as a level.
func TestReadinessLatchesRatherThanLevels(t *testing.T) {
	t.Parallel()

	p := New(slog.Default())
	p.next = func(_ *iutil.Event) {}

	send(p, iutil.ActionConnected, iutil.ChainCold)
	assert.Equal(t, 503, ready(t, p), "connected is not published")

	send(p, iutil.ActionSweepEnd, iutil.ChainWarm)
	assert.Equal(t, 200, ready(t, p), "the first round is what there is to serve")

	// A round still running carries no chain-state change at all.
	send(p, incusapi.EventLifecycleInstanceUpdated, iutil.ChainWarm)
	assert.Equal(t, 200, ready(t, p), "a round is not an outage")

	send(p, iutil.ActionSweepEnd, iutil.ChainWarm)
	assert.Equal(t, 200, ready(t, p), "and it is still ready after one")

	// The stream going is the one thing that does clear readiness.
	send(p, iutil.ActionDisconnected, iutil.ChainCold)
	assert.Equal(t, 503, ready(t, p), "a lost stream is unready")
}

// TestReadinessNamesWhichHalfIsMissing distinguishes a disconnected stream from a chain that has read nothing.
func TestReadinessNamesWhichHalfIsMissing(t *testing.T) {
	t.Parallel()

	p := New(slog.Default())
	p.next = func(_ *iutil.Event) {}

	w := httptest.NewRecorder()
	p.serveReady(w, httptest.NewRequestWithContext(t.Context(), "GET", "/ready", nil))
	assert.Contains(t, w.Body.String(), "not connected")

	send(p, iutil.ActionConnected, iutil.ChainCold)

	w = httptest.NewRecorder()
	p.serveReady(w, httptest.NewRequestWithContext(t.Context(), "GET", "/ready", nil))
	assert.Contains(t, w.Body.String(), "nothing published yet")
}
