package iclient

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/lxc/incus/v7/shared/api"
	"github.com/stretchr/testify/require"

	"github.com/lxc/incus-compose/testlib"
)

// testConnection dials the remote the test environment points at.
func testConnection(t *testing.T) *Connection {
	t.Helper()

	config, err := ReadConfig("")
	require.NoError(t, err)

	info, err := config.RemoteInfos(os.Getenv("INCUS_REMOTE"))
	require.NoError(t, err)

	conn, err := NewConnection(info)
	require.NoError(t, err)

	return conn
}

// transportOf digs out the transport a connection was built with.
func transportOf(t *testing.T, conn *Connection) (*Connection, *http.Transport) {
	t.Helper()

	transport, ok := conn.http.Transport.(*http.Transport)
	require.True(t, ok)

	return conn, transport
}

// TestIncusTransportTuning pins the settings that are wrong by default or wrong
// if someone "tidies" them later.
func TestIncusTransportTuning(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct{ name, addr string }{
		{"unix", "unix:///tmp/x.socket"},
		{"https", "https://127.0.0.1:8443"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			conn, err := NewConnection(&ConfigRemoteInfo{Name: tt.name, Addrs: []string{tt.addr}})
			require.NoError(t, err)

			incus, transport := transportOf(t, conn)

			// A whole-request deadline would cut off the event stream, an
			// operation long-poll and every console or SFTP transfer.
			require.Zero(t, incus.http.Timeout, "the context is the per-call deadline, not Client.Timeout")

			// Blocking on this starves workers behind long-lived streams.
			require.Zero(t, transport.MaxConnsPerHost, "MaxConnsPerHost must stay unbounded")

			// The default of 2 makes a worker pool reconnect constantly.
			require.Greater(t, transport.MaxIdleConnsPerHost, 2)

			require.False(t, transport.DisableKeepAlives, "pooling is why WithProject shares the transport")
			require.False(t, transport.ForceAttemptHTTP2, "events and exec need an HTTP/1.1 upgrade")

			require.NotZero(t, transport.TLSHandshakeTimeout)
			require.GreaterOrEqual(t, transport.ResponseHeaderTimeout, time.Hour,
				"an operation wait sends no header until it finishes")
		})
	}
}

func TestIncusTransportPerAddressKind(t *testing.T) {
	t.Parallel()

	unix, err := NewConnection(&ConfigRemoteInfo{Name: "u", Addrs: []string{"unix:///tmp/x.socket"}})
	require.NoError(t, err)

	incus, transport := transportOf(t, unix)
	require.Equal(t, "/tmp/x.socket", incus.socketPath)
	require.Nil(t, transport.TLSClientConfig, "a unix socket needs no TLS")
	require.Nil(t, transport.Proxy, "a unix socket is never proxied")

	tls, err := NewConnection(&ConfigRemoteInfo{Name: "t", Addrs: []string{"https://127.0.0.1:8443"}})
	require.NoError(t, err)

	incus, transport = transportOf(t, tls)
	require.Empty(t, incus.socketPath)
	require.NotNil(t, transport.TLSClientConfig)
	require.NotNil(t, transport.Proxy, "an https remote honors the proxy environment")
}

// TestIncusWithProjectSharesTransport is the payoff of pooling: a re-scoped
// copy must not open a second pool.
func TestIncusWithProjectSharesTransport(t *testing.T) {
	t.Parallel()

	conn, err := NewConnection(&ConfigRemoteInfo{Name: "t", Addrs: []string{"https://127.0.0.1:8443"}})
	require.NoError(t, err)

	_, original := transportOf(t, conn)
	_, copied := transportOf(t, conn.WithProject("other"))

	require.Same(t, original, copied)
}

// TestIncusWithMaxIdleConns is the mirror image of WithProject: here the
// transport must NOT be shared, because the pool size belongs to the pool.
func TestIncusWithMaxIdleConns(t *testing.T) {
	t.Parallel()

	conn, err := NewConnection(&ConfigRemoteInfo{
		Name:    "t",
		Addrs:   []string{"https://127.0.0.1:8443"},
		Project: "myproject",
	})
	require.NoError(t, err)

	_, before := transportOf(t, conn)

	tuned := conn.WithMaxIdleConns(7, 3)

	incus, after := transportOf(t, tuned)

	require.NotSame(t, before, after, "resizing a live pool under in-flight requests is a race")
	require.Equal(t, 7, after.MaxIdleConns)
	require.Equal(t, 3, after.MaxIdleConnsPerHost)

	// The original keeps the defaults.
	require.Equal(t, incusMaxIdleConns, before.MaxIdleConns)
	require.Equal(t, incusMaxIdleConnsPerHost, before.MaxIdleConnsPerHost)

	// Everything else survives the clone.
	require.Equal(t, "myproject", incus.project)
	require.NotNil(t, after.TLSClientConfig)
	require.NotNil(t, after.Proxy)
	require.False(t, after.ForceAttemptHTTP2)
	require.Equal(t, incusResponseHeaderTimeout, after.ResponseHeaderTimeout)
	require.Zero(t, incus.http.Timeout)
}

func TestIncusWithMaxIdleConnsKeepsTheDialer(t *testing.T) {
	t.Parallel()

	conn, err := NewConnection(&ConfigRemoteInfo{Name: "u", Addrs: []string{"unix:///tmp/x.socket"}})
	require.NoError(t, err)

	incus, transport := transportOf(t, conn.WithMaxIdleConns(4, 2))

	require.NotNil(t, transport.DialContext, "a unix connection is nothing without its dialer")
	require.Equal(t, "/tmp/x.socket", incus.socketPath)
}

func TestIncusWithersCompose(t *testing.T) {
	t.Parallel()

	conn, err := NewConnection(&ConfigRemoteInfo{Name: "t", Addrs: []string{"https://127.0.0.1:8443"}})
	require.NoError(t, err)

	incus, transport := transportOf(t, conn.WithMaxIdleConns(9, 5).WithProject("other"))

	require.Equal(t, "other", incus.project)
	require.Equal(t, 9, transport.MaxIdleConns, "WithProject must keep the retuned transport")
}

func TestNewConnectionIncusNoAddress(t *testing.T) {
	t.Parallel()

	_, err := NewConnection(&ConfigRemoteInfo{Name: "empty"})
	require.ErrorIs(t, err, ErrConnectionNoAddress)
}

func TestIncusSocketPathExplicit(t *testing.T) {
	t.Parallel()

	require.Equal(t, "/tmp/a.socket", incusSocketPath("unix:///tmp/a.socket"))
	require.Equal(t, "/tmp/b.socket", incusSocketPath("unix:/tmp/b.socket"))
}

func TestIncusSocketPathFromEnv(t *testing.T) {
	t.Setenv("INCUS_SOCKET", "/tmp/from-env.socket")
	require.Equal(t, "/tmp/from-env.socket", incusSocketPath("unix://"))

	// An address carrying a path beats the environment.
	require.Equal(t, "/tmp/explicit.socket", incusSocketPath("unix:///tmp/explicit.socket"))
}

func TestIncusSocketPathFromDir(t *testing.T) {
	dir := t.TempDir()

	t.Setenv("INCUS_SOCKET", "")
	t.Setenv("INCUS_DIR", dir)

	require.Equal(t, filepath.Join(dir, "unix.socket"), incusSocketPath("unix://"))
}

func TestIncusGetInstanceNames(t *testing.T) {
	testlib.SkipLocal(t)
	t.Parallel()

	conn := testConnection(t)

	names, err := conn.GetInstanceNames(t.Context(), nil)
	require.NoError(t, err)

	// Every name must be a bare name, not the resource URL the API returns.
	for _, name := range names {
		require.NotContains(t, name, "/", "GetInstanceNames must strip the URL prefix")
	}
}

func TestIncusGetInstancesRecursion(t *testing.T) {
	testlib.SkipLocal(t)
	t.Parallel()

	ctx := t.Context()
	conn := testConnection(t)

	instances, err := conn.GetInstances(ctx, nil)
	require.NoError(t, err)

	full, err := conn.GetInstances(ctx, &GetInstancesArgs{Full: true})
	require.NoError(t, err)

	require.Len(t, full, len(instances), "recursion=1 and recursion=2 must see the same set")

	names, err := conn.GetInstanceNames(ctx, nil)
	require.NoError(t, err)

	require.Len(t, names, len(instances), "the name list must match the recursive list")
}

// TestIncusGetInstanceNotFound pins the error mapping: an API error envelope
// has to come back as a 404 StatusError, not as a decode failure or a nil.
func TestIncusGetInstanceNotFound(t *testing.T) {
	testlib.SkipLocal(t)
	t.Parallel()

	ctx := t.Context()
	conn := testConnection(t)

	_, _, err := conn.GetInstance(ctx, "ic-iclient-does-not-exist", nil)
	require.Error(t, err)
	require.True(t, api.StatusErrorCheck(err, 404), "want a 404 StatusError, got %v", err)

	_, _, err = conn.GetInstanceState(ctx, "ic-iclient-does-not-exist")
	require.Error(t, err)
	require.True(t, api.StatusErrorCheck(err, 404), "want a 404 StatusError, got %v", err)
}

// TestIncusGetInstanceRoundTrip only runs where the remote already has an
// instance; it checks the single-instance calls agree with the list ones.
func TestIncusGetInstanceRoundTrip(t *testing.T) {
	testlib.SkipLocal(t)
	t.Parallel()

	ctx := t.Context()
	conn := testConnection(t)

	names, err := conn.GetInstanceNames(ctx, nil)
	require.NoError(t, err)

	if len(names) == 0 {
		t.Skip("no instance on the remote to read")
	}

	name := names[0]

	instance, etag, err := conn.GetInstance(ctx, name, nil)
	require.NoError(t, err)
	require.Equal(t, name, instance.Name)
	require.NotEmpty(t, etag, "GetInstance must return the ETag header")

	full, _, err := conn.GetInstance(ctx, name, &GetInstanceArgs{Full: true})
	require.NoError(t, err)
	require.Equal(t, name, full.Name)

	state, _, err := conn.GetInstanceState(ctx, name)
	require.NoError(t, err)
	require.NotEmpty(t, state.Status)
}

// TestIncusUnknownProjectIsEmpty pins what the server actually does: an
// unknown project is an empty collection, not a 404.
func TestIncusUnknownProjectIsEmpty(t *testing.T) {
	testlib.SkipLocal(t)
	t.Parallel()

	config, err := ReadConfig("")
	require.NoError(t, err)

	info, err := config.RemoteInfos(os.Getenv("INCUS_REMOTE"))
	require.NoError(t, err)

	info.Project = "ic-iclient-no-such-project"

	conn, err := NewConnection(info)
	require.NoError(t, err)

	names, err := conn.GetInstanceNames(t.Context(), nil)
	require.NoError(t, err)
	require.Empty(t, names)
}

// recordingRequest is one request a recording server saw.
type recordingRequest struct {
	method string
	url    url.URL
	etag   string
	body   string
	header http.Header
}

// uri is the path and query, the way the request went out.
func (r recordingRequest) uri() string {
	return r.url.RequestURI()
}

// query returns one query parameter of the request.
func (r recordingRequest) query(key string) string {
	return r.url.Query().Get(key)
}

// recorder collects what a test server saw. The handler runs on its own
// goroutine, so every access goes through the lock.
type recorder struct {
	mu       sync.Mutex
	requests []recordingRequest
}

// add records one request, draining its body.
func (r *recorder) add(req *http.Request) {
	body, _ := io.ReadAll(req.Body)

	r.mu.Lock()
	defer r.mu.Unlock()

	r.requests = append(r.requests, recordingRequest{
		method: req.Method,
		url:    *req.URL,
		etag:   req.Header.Get("If-Match"),
		body:   string(body),
		header: req.Header.Clone(),
	})
}

// all returns the requests seen so far, in order.
func (r *recorder) all() []recordingRequest {
	r.mu.Lock()
	defer r.mu.Unlock()

	return append([]recordingRequest(nil), r.requests...)
}

// uris returns the path and query of every request, in order.
func (r *recorder) uris() []string {
	requests := r.all()

	uris := make([]string, 0, len(requests))
	for _, req := range requests {
		uris = append(uris, req.uri())
	}

	return uris
}

// len returns how many requests have arrived.
func (r *recorder) len() int {
	r.mu.Lock()
	defer r.mu.Unlock()

	return len(r.requests)
}

// recordingServer answers like Incus and records every request it was sent.
func recordingServer(t *testing.T, metadata string) (*Connection, *recorder) {
	t.Helper()

	seen := &recorder{}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen.add(r)

		w.Header().Set("ETag", "test-etag")
		w.Header().Set("Content-Type", "application/json")

		_, _ = io.WriteString(w, `{"type":"sync","status_code":200,"metadata":`+metadata+`}`)
	}))
	t.Cleanup(server.Close)

	conn, err := NewConnection(&ConfigRemoteInfo{
		Name:    "recording",
		Addrs:   []string{server.URL},
		Project: "myproject",
	})
	require.NoError(t, err)

	return conn, seen
}

// TestIncusRequestURLs pins what goes on the wire; a real server answers happily
// with the project or recursion parameter dropped.
func TestIncusRequestURLs(t *testing.T) {
	t.Parallel()

	ctx := t.Context()

	for _, tt := range []struct {
		name     string
		metadata string
		call     func(*Connection) error
		want     string
	}{
		{
			"GetInstanceNames", `[]`,
			func(c *Connection) error { _, err := c.GetInstanceNames(ctx, nil); return err },
			"/1.0/instances?project=myproject",
		},
		{
			"GetInstanceNames typed", `[]`,
			func(c *Connection) error {
				_, err := c.GetInstanceNames(ctx, &GetInstancesArgs{Type: api.InstanceTypeContainer})
				return err
			},
			"/1.0/instances?instance-type=container&project=myproject",
		},
		{
			"GetInstances", `[]`,
			func(c *Connection) error { _, err := c.GetInstances(ctx, nil); return err },
			"/1.0/instances?project=myproject&recursion=1",
		},
		{
			"GetInstancesFull", `[]`,
			func(c *Connection) error { _, err := c.GetInstances(ctx, &GetInstancesArgs{Full: true}); return err },
			"/1.0/instances?project=myproject&recursion=2",
		},
		{
			"GetInstance", `{}`,
			func(c *Connection) error { _, _, err := c.GetInstance(ctx, "web-1", nil); return err },
			"/1.0/instances/web-1?project=myproject",
		},
		{
			"GetInstance full", `{}`,
			func(c *Connection) error {
				_, _, err := c.GetInstance(ctx, "web-1", &GetInstanceArgs{Full: true})

				return err
			},
			"/1.0/instances/web-1?project=myproject&recursion=1",
		},
		{
			"GetInstanceState", `{}`,
			func(c *Connection) error { _, _, err := c.GetInstanceState(ctx, "web-1"); return err },
			"/1.0/instances/web-1/state?project=myproject",
		},
		{
			"name is escaped", `{}`,
			func(c *Connection) error { _, _, err := c.GetInstance(ctx, "a/b", nil); return err },
			"/1.0/instances/a%2Fb?project=myproject",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			conn, seen := recordingServer(t, tt.metadata)

			require.NoError(t, tt.call(conn))
			require.Equal(t, []string{tt.want}, seen.uris())
		})
	}
}

// TestIncusWithProject covers the sharing contract: a re-scoped copy reuses the
// transport, leaves the original alone, and sends the new project.
func TestIncusWithProject(t *testing.T) {
	t.Parallel()

	ctx := t.Context()

	conn, seen := recordingServer(t, `[]`)
	other := conn.WithProject("second")

	_, err := other.GetInstanceNames(ctx, nil)
	require.NoError(t, err)

	_, err = conn.GetInstanceNames(ctx, nil)
	require.NoError(t, err)

	require.Equal(t, []string{
		"/1.0/instances?project=second",
		"/1.0/instances?project=myproject",
	}, seen.uris(), "the copy must re-scope and the original must not change")

	require.NotSame(t, conn, other, "WithProject must not mutate in place")
	require.Same(t, conn.http, other.http, "the transport is what gets shared")
}

func TestIncusWithProjectEmpty(t *testing.T) {
	t.Parallel()

	conn, seen := recordingServer(t, `[]`)

	_, err := conn.WithProject("").GetInstanceNames(t.Context(), nil)
	require.NoError(t, err)

	require.Equal(t, []string{"/1.0/instances"}, seen.uris(), "an empty project sends no parameter")
}

// TestIncusInstancesArgs covers the axes the args struct replaced.
func TestIncusInstancesArgs(t *testing.T) {
	t.Parallel()

	ctx := t.Context()

	for _, tt := range []struct {
		name string
		args *GetInstancesArgs
		want string
	}{
		{"nil is the zero value", nil, "/1.0/instances?project=myproject&recursion=1"},
		{"full", &GetInstancesArgs{Full: true}, "/1.0/instances?project=myproject&recursion=2"},
		{
			"all projects drops the project",
			&GetInstancesArgs{AllProjects: true},
			"/1.0/instances?all-projects=true&recursion=1",
		},
		{
			"filters are rendered as the API reads them",
			&GetInstancesArgs{Filters: []string{"status=Running"}},
			"/1.0/instances?filter=status+eq+Running&project=myproject&recursion=1",
		},
		{
			"every axis at once",
			&GetInstancesArgs{
				Type:        api.InstanceTypeContainer,
				Full:        true,
				AllProjects: true,
				Filters:     []string{"status=Running", "type=container"},
			},
			"/1.0/instances?all-projects=true&filter=status+eq+Running+and+type+eq+container" +
				"&instance-type=container&recursion=2",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			conn, seen := recordingServer(t, `[]`)

			_, err := conn.GetInstances(ctx, tt.args)
			require.NoError(t, err)
			require.Equal(t, []string{tt.want}, seen.uris())
		})
	}
}

// TestIncusGetInstanceNamesRefusesAllProjects: a bare name is not unique
// across projects, so this combination has no honest answer.
func TestIncusGetInstanceNamesRefusesAllProjects(t *testing.T) {
	t.Parallel()

	conn, seen := recordingServer(t, `[]`)

	_, err := conn.GetInstanceNames(t.Context(), &GetInstancesArgs{AllProjects: true})
	require.ErrorIs(t, err, ErrConnectionUnsupported)
	require.Empty(t, seen.all(), "it must refuse before asking the server")
}

func TestIncusGetInstanceReturnsETag(t *testing.T) {
	t.Parallel()

	conn, _ := recordingServer(t, `{"name":"web-1"}`)

	instance, etag, err := conn.GetInstance(t.Context(), "web-1", nil)
	require.NoError(t, err)
	require.Equal(t, "web-1", instance.Name)
	require.Equal(t, "test-etag", etag)
}

// TestIncusErrorEnvelope proves an API error envelope becomes a StatusError
// rather than a decode failure or a silent success.
func TestIncusErrorEnvelope(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)

		_, _ = io.WriteString(w, `{"type":"error","error_code":404,"error":"Instance not found"}`)
	}))
	t.Cleanup(server.Close)

	conn, err := NewConnection(&ConfigRemoteInfo{Name: "erroring", Addrs: []string{server.URL}})
	require.NoError(t, err)

	_, _, err = conn.GetInstance(t.Context(), "nope", nil)
	require.Error(t, err)
	require.True(t, api.StatusErrorCheck(err, 404), "want a 404 StatusError, got %v", err)
	require.Contains(t, err.Error(), "Instance not found")
}

// TestIncusContextCancelled is the reason the fork exists: a call must abandon
// its request when the context goes, instead of waiting on the server.
func TestIncusContextCancelled(t *testing.T) {
	t.Parallel()

	release := make(chan struct{})

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))

	t.Cleanup(func() {
		close(release)
		server.Close()
	})

	conn, err := NewConnection(&ConfigRemoteInfo{Name: "hanging", Addrs: []string{server.URL}})
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())

	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	_, err = conn.GetInstanceNames(ctx, nil)
	require.ErrorIs(t, err, context.Canceled)
}
