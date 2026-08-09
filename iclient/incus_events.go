package iclient

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/lxc/incus/v7/shared/api"
)

// incusEventBuffer is how far a listener may fall behind. Nothing is dropped:
// a slow consumer becomes backpressure on the server.
const incusEventBuffer = 64

// incusEventSilence is how long an event socket may say nothing before it counts
// as dead. The server pings every 10s, so silence is not something a healthy
// connection does.
//
// Without it a half-open socket - a peer that went away rather than closed - sits
// in ReadMessage until the kernel's TCP keepalive gives up minutes later, and
// nothing above learns the stream stopped.
const incusEventSilence = 30 * time.Second

// incusEvents is one connection's listeners, keyed by the channel each was
// handed. Each ends with the context it was opened on; this is the register that
// says which are still running.
type incusEvents struct {
	mu      sync.Mutex
	cancels map[chan api.Event]context.CancelFunc
}

// add registers a listener's cancel.
func (e *incusEvents) add(events chan api.Event, cancel context.CancelFunc) {
	e.mu.Lock()
	defer e.mu.Unlock()

	if e.cancels == nil {
		e.cancels = map[chan api.Event]context.CancelFunc{}
	}

	e.cancels[events] = cancel
}

// remove forgets a listener that has already ended.
func (e *incusEvents) remove(events chan api.Event) {
	e.mu.Lock()
	defer e.mu.Unlock()

	delete(e.cancels, events)
}

// len reports how many listeners are still registered.
func (e *incusEvents) len() int {
	e.mu.Lock()
	defer e.mu.Unlock()

	return len(e.cancels)
}

// ListenEvents opens an event socket of this connection's own and returns the
// events it carries. Closing ctx closes the channel.
//
// With allProjects the connection's project is not sent at all, and the server
// answers with every project the certificate is allowed to see.
func (c *Connection) ListenEvents(ctx context.Context, types []string, allProjects bool) (<-chan api.Event, error) {
	transport, ok := c.http.Transport.(*http.Transport)
	if !ok {
		return nil, fmt.Errorf("listening for events: unexpected transport %T", c.http.Transport)
	}

	query := url.Values{}
	if len(types) > 0 {
		query.Set("type", strings.Join(types, ","))
	}

	switch {
	case allProjects:
		query.Set("all-projects", "true")
	case c.project != "":
		query.Set("project", c.project)
	}

	uri := c.eventsURL(query)

	dialer := websocket.Dialer{
		NetDialContext:   transport.DialContext,
		TLSClientConfig:  transport.TLSClientConfig,
		Proxy:            transport.Proxy,
		HandshakeTimeout: incusTLSHandshakeTimeout,
	}

	socket, resp, err := dialer.DialContext(ctx, uri, nil)
	if err != nil {
		if resp != nil {
			_ = resp.Body.Close()
		}

		return nil, fmt.Errorf("opening the event socket: %w", err)
	}

	if resp != nil {
		_ = resp.Body.Close()
	}

	// A ping is the only thing a quiet server sends, so it is what says the socket
	// is still there. The default handler still answers it with a pong.
	resetDeadline := func() {
		_ = socket.SetReadDeadline(time.Now().Add(c.eventSilence))
	}

	resetDeadline()

	pong := socket.PingHandler()
	socket.SetPingHandler(func(data string) error {
		resetDeadline()

		return pong(data)
	})

	// Its own context, so the read loop can end itself on a dead socket without
	// touching the caller's.
	listenCtx, cancel := context.WithCancel(ctx)

	events := make(chan api.Event, incusEventBuffer)

	c.events.add(events, cancel)

	// ReadMessage only unblocks on a closed socket, so the context has to close it.
	go func() {
		<-listenCtx.Done()
		_ = socket.Close()
	}()

	go func() {
		defer close(events)

		// cancel stops the watcher above; remove keeps the hub from growing.
		defer c.events.remove(events)
		defer cancel()

		for {
			_, message, err := socket.ReadMessage()
			if err != nil {
				return
			}

			event := api.Event{}

			err = json.Unmarshal(message, &event)
			if err != nil {
				continue
			}

			select {
			case events <- event:
			case <-listenCtx.Done():
				return
			}
		}
	}()

	return events, nil
}

// LifecycleEvent decodes lifecycle metadata, backfilling Name and Project from
// Source: only instance events carry them as fields.
func LifecycleEvent(raw api.Event) (api.EventLifecycle, error) {
	var lc api.EventLifecycle

	err := json.Unmarshal(raw.Metadata, &lc)
	if err != nil {
		return api.EventLifecycle{}, fmt.Errorf("decoding lifecycle metadata: %w", err)
	}

	if lc.Name != "" && lc.Project != "" {
		return lc, nil
	}

	source, err := url.Parse(lc.Source)
	if err != nil {
		// Nothing to take them from, so what was decoded stands.
		source = &url.URL{}
	}

	if lc.Name == "" && source.Path != "" {
		lc.Name = path.Base(source.Path)
	}

	if lc.Project == "" {
		lc.Project = source.Query().Get("project")
	}

	return lc, nil
}

// eventsURL is the websocket address of the event endpoint.
func (c *Connection) eventsURL(query url.Values) string {
	return c.websocketURL("/1.0/events", query)
}

// websocketURL turns an API path into the websocket address for it.
func (c *Connection) websocketURL(path string, query url.Values) string {
	uri := c.baseURL + path

	switch {
	case strings.HasPrefix(uri, "https://"):
		uri = "wss://" + strings.TrimPrefix(uri, "https://")
	case strings.HasPrefix(uri, "http://"):
		uri = "ws://" + strings.TrimPrefix(uri, "http://")
	}

	if len(query) > 0 {
		uri += "?" + query.Encode()
	}

	return uri
}
