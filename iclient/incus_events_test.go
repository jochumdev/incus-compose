package iclient

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/lxc/incus/v7/shared/api"
	"github.com/stretchr/testify/require"

	"github.com/lxc/incus-compose/testlib"
)

// eventServer serves /1.0/events over a websocket, writing each event it is
// given, and records the requests it was asked for. It never pings, so a
// listener against it hears nothing between events.
func eventServer(t *testing.T, send <-chan api.Event) (*Connection, *recorder) {
	t.Helper()

	return pingingEventServer(t, send, 0)
}

// pingingEventServer is eventServer with the keepalive incusd sends every 10s.
// A ping interval of 0 means none at all.
func pingingEventServer(t *testing.T, send <-chan api.Event, every time.Duration) (*Connection, *recorder) {
	t.Helper()

	seen := &recorder{}
	upgrader := websocket.Upgrader{}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen.add(r)

		socket, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}

		defer func() { _ = socket.Close() }()

		// Nil while nothing pings, and a select on a nil channel never fires.
		var ping <-chan time.Time

		if every > 0 {
			ticker := time.NewTicker(every)
			defer ticker.Stop()

			ping = ticker.C
		}

		// One writer for both, since a websocket does not take two.
		for {
			select {
			case event, ok := <-send:
				if !ok {
					return
				}

				payload, err := json.Marshal(event)
				if err != nil {
					return
				}

				err = socket.WriteMessage(websocket.TextMessage, payload)
				if err != nil {
					return
				}

			case <-ping:
				err := socket.WriteControl(websocket.PingMessage, []byte("keepalive"), time.Now().Add(time.Second))
				if err != nil {
					return
				}
			}
		}
	}))
	t.Cleanup(server.Close)

	conn, err := NewConnection(&ConfigRemoteInfo{
		Name:    "events",
		Addrs:   []string{server.URL},
		Project: "myproject",
	})
	require.NoError(t, err)

	return conn, seen
}

func TestIncusListenEvents(t *testing.T) {
	t.Parallel()

	send := make(chan api.Event, 1)
	t.Cleanup(func() { close(send) })

	conn, seen := eventServer(t, send)

	events, err := conn.ListenEvents(t.Context(), []string{"lifecycle", "operation"}, false)
	require.NoError(t, err)

	send <- api.Event{Type: "lifecycle", Project: "myproject"}

	select {
	case event := <-events:
		require.Equal(t, "lifecycle", event.Type)
	case <-time.After(5 * time.Second):
		t.Fatal("no event arrived")
	}

	// The filter and the project are the caller's, not another listener's.
	req := seen.all()[0]
	require.Equal(t, "lifecycle,operation", req.query("type"))
	require.Equal(t, "myproject", req.query("project"))
}

// TestIncusListenEventsAllProjects: the project is dropped, not combined.
func TestIncusListenEventsAllProjects(t *testing.T) {
	t.Parallel()

	send := make(chan api.Event)
	t.Cleanup(func() { close(send) })

	conn, seen := eventServer(t, send)

	_, err := conn.ListenEvents(t.Context(), []string{"lifecycle"}, true)
	require.NoError(t, err)

	require.Eventually(t, func() bool {
		return seen.len() == 1
	}, 5*time.Second, 10*time.Millisecond, "the listener never reached the server")

	req := seen.all()[0]
	require.Equal(t, "true", req.query("all-projects"))
	require.Empty(t, req.query("project"), "an all-projects listener is not scoped to one")
	require.Equal(t, "lifecycle", req.query("type"), "the type filter still applies")
}

// TestIncusListenEventsOwnFilter is the difference from upstream: a second
// listener gets its own socket and its own filter, instead of joining the
// first one's.
func TestIncusListenEventsOwnFilter(t *testing.T) {
	t.Parallel()

	send := make(chan api.Event)
	t.Cleanup(func() { close(send) })

	conn, seen := eventServer(t, send)

	_, err := conn.ListenEvents(t.Context(), []string{"lifecycle"}, false)
	require.NoError(t, err)

	_, err = conn.ListenEvents(t.Context(), []string{"operation"}, false)
	require.NoError(t, err)

	require.Eventually(t, func() bool {
		return seen.len() == 2
	}, 5*time.Second, 10*time.Millisecond, "both listeners must reach the server")

	requests := seen.all()
	require.Equal(t, "lifecycle", requests[0].query("type"))
	require.Equal(t, "operation", requests[1].query("type"), "the second listener must ask for its own types")
}

// TestIncusListenEventsClosesOnSilence is the half-open case: a server that
// neither sends nor closes. Without a read deadline the channel stays open until
// the kernel's TCP keepalive gives up, and nothing above learns the stream died.
func TestIncusListenEventsClosesOnSilence(t *testing.T) {
	t.Parallel()

	send := make(chan api.Event)
	t.Cleanup(func() { close(send) })

	conn, _ := eventServer(t, send)
	conn.eventSilence = 200 * time.Millisecond

	events, err := conn.ListenEvents(t.Context(), nil, false)
	require.NoError(t, err)

	select {
	case _, open := <-events:
		require.False(t, open, "the channel must close, not deliver")
	case <-time.After(5 * time.Second):
		t.Fatal("a silent socket left the channel open")
	}
}

// TestIncusListenEventsPingSurvivesSilence is the other half: the server's ping
// is what says a quiet connection is still there, so a listener with nothing to
// deliver must outlive the window rather than be torn down by it.
func TestIncusListenEventsPingSurvivesSilence(t *testing.T) {
	t.Parallel()

	send := make(chan api.Event)
	t.Cleanup(func() { close(send) })

	conn, _ := pingingEventServer(t, send, 50*time.Millisecond)
	conn.eventSilence = 300 * time.Millisecond

	events, err := conn.ListenEvents(t.Context(), nil, false)
	require.NoError(t, err)

	select {
	case _, open := <-events:
		require.True(t, open, "a pinged socket must not be closed as silent")
	case <-time.After(time.Second):
		// Nothing delivered and nothing closed, which is the point.
	}
}

func TestIncusListenEventsClosesOnContext(t *testing.T) {
	t.Parallel()

	send := make(chan api.Event)
	t.Cleanup(func() { close(send) })

	conn, _ := eventServer(t, send)

	incus := conn

	ctx, cancel := context.WithCancel(t.Context())

	events, err := conn.ListenEvents(ctx, nil, false)
	require.NoError(t, err)
	require.Equal(t, 1, incus.events.len())

	cancel()

	select {
	case _, open := <-events:
		require.False(t, open, "the channel must close, not deliver")
	case <-time.After(5 * time.Second):
		t.Fatal("the channel stayed open after the context went")
	}

	// A listener that ended on its own must not stay registered.
	require.Eventually(t, func() bool {
		return incus.events.len() == 0
	}, 5*time.Second, 10*time.Millisecond, "the ended listener is still registered")
}

// TestIncusListenEventsCancelImmediately is the shape a caller writes: build
// the context, listen, cancel. The cancel may land before the read loop has
// even started.
func TestIncusListenEventsCancelImmediately(t *testing.T) {
	t.Parallel()

	send := make(chan api.Event)
	t.Cleanup(func() { close(send) })

	conn, _ := eventServer(t, send)

	for range 20 {
		ctx, cancel := context.WithCancel(t.Context())

		events, err := conn.ListenEvents(ctx, nil, false)
		require.NoError(t, err)

		cancel()

		select {
		case _, open := <-events:
			require.False(t, open)
		case <-time.After(5 * time.Second):
			t.Fatal("the channel stayed open after the context went")
		}
	}

	incus := conn

	require.Eventually(t, func() bool {
		return incus.events.len() == 0
	}, 5*time.Second, 10*time.Millisecond, "twenty ended listeners are still registered")
}

// TestIncusListenEventsCancelWhileDelivering cancels with the consumer not
// reading, so the read loop is parked on the channel send rather than on the
// socket.
func TestIncusListenEventsCancelWhileDelivering(t *testing.T) {
	t.Parallel()

	send := make(chan api.Event)

	conn, _ := eventServer(t, send)

	ctx, cancel := context.WithCancel(t.Context())

	events, err := conn.ListenEvents(ctx, nil, false)
	require.NoError(t, err)

	// Fill past the buffer without reading a single one.
	go func() {
		for range incusEventBuffer * 2 {
			select {
			case send <- api.Event{Type: "lifecycle"}:
			case <-ctx.Done():
				return
			}
		}
	}()

	time.Sleep(100 * time.Millisecond)
	cancel()

	// Drain whatever was buffered; the channel still has to close.
	closed := false

	for !closed {
		select {
		case _, open := <-events:
			closed = !open
		case <-time.After(5 * time.Second):
			t.Fatal("the channel stayed open while blocked on a send")
		}
	}
}

// TestIncusListenEventsScopedEndsOnItsOwnContext: a project-scoped copy is a
// view on the parent's transport, but its listeners are its own and end with the
// context they were opened on, not with the parent's.
func TestIncusListenEventsScopedEndsOnItsOwnContext(t *testing.T) {
	t.Parallel()

	send := make(chan api.Event)
	t.Cleanup(func() { close(send) })

	conn, _ := eventServer(t, send)

	scoped := conn.WithProject("second")

	ctx, cancel := context.WithCancel(t.Context())

	events, err := scoped.ListenEvents(ctx, nil, false)
	require.NoError(t, err)
	require.Equal(t, 1, scoped.events.len())
	require.Equal(t, 0, conn.events.len(), "a copy's listener is not the parent's")

	cancel()

	select {
	case _, open := <-events:
		require.False(t, open, "the channel must close, not deliver")
	case <-time.After(5 * time.Second):
		t.Fatal("the copy's listener outlived its context")
	}

	require.Eventually(t, func() bool {
		return scoped.events.len() == 0
	}, 5*time.Second, 10*time.Millisecond, "the ended listener is still registered")
}

func TestIncusListenEventsAgainstRealIncus(t *testing.T) {
	testlib.SkipLocal(t)
	t.Parallel()

	conn := testConnection(t)

	ctx, cancel := context.WithCancel(t.Context())

	events, err := conn.ListenEvents(ctx, []string{"lifecycle"}, false)
	require.NoError(t, err)

	cancel()

	select {
	case <-events:
	case <-time.After(10 * time.Second):
		t.Fatal("the event socket did not close")
	}
}

// TestLifecycleEvent: incusd fills Name and Project on instance events alone,
// so every other kind has to be taken out of Source.
func TestLifecycleEvent(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name  string
		lc    api.EventLifecycle
		wantN string
		wantP string
	}{
		{
			name:  "an instance carries both already",
			lc:    api.EventLifecycle{Action: "instance-started", Source: "/1.0/instances/web1?project=alpha", Name: "web1", Project: "alpha"},
			wantN: "web1",
			wantP: "alpha",
		},
		{
			name:  "a network in the default project",
			lc:    api.EventLifecycle{Action: "network-created", Source: "/1.0/networks/ic-q2mjfn37xz"},
			wantN: "ic-q2mjfn37xz",
			// api.URL.Project omits the query for the default project, and so does
			// a resource that is not project-scoped, so this is not guessed at.
			wantP: "",
		},
		{
			name:  "a network in a project owning its own",
			lc:    api.EventLifecycle{Action: "network-updated", Source: "/1.0/networks/br0?project=alpha"},
			wantN: "br0",
			wantP: "alpha",
		},
		{
			name:  "a profile",
			lc:    api.EventLifecycle{Action: "profile-updated", Source: "/1.0/profiles/default?project=alpha"},
			wantN: "default",
			wantP: "alpha",
		},
		{
			name:  "a project is not project-scoped",
			lc:    api.EventLifecycle{Action: "project-updated", Source: "/1.0/projects/alpha"},
			wantN: "alpha",
			wantP: "",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			metadata, err := json.Marshal(tc.lc)
			require.NoError(t, err)

			got, err := LifecycleEvent(api.Event{Type: "lifecycle", Metadata: metadata})
			require.NoError(t, err)

			require.Equal(t, tc.wantN, got.Name)
			require.Equal(t, tc.wantP, got.Project)
			require.Equal(t, tc.lc.Action, got.Action)
		})
	}
}

// TestLifecycleEventRejectsBadMetadata: metadata that will not parse is the one
// failure this reports.
func TestLifecycleEventRejectsBadMetadata(t *testing.T) {
	t.Parallel()

	_, err := LifecycleEvent(api.Event{Type: "lifecycle", Metadata: []byte("not json")})
	require.Error(t, err)
}
