package iclient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/lxc/incus/v7/shared/api"
	incusTLS "github.com/lxc/incus/v7/shared/tls"
	"github.com/lxc/incus/v7/shared/util"
)

// Transport tuning; Go's default of 2 idle connections per host throttles a worker pool.
const (
	incusMaxIdleConns        = 128
	incusMaxIdleConnsPerHost = 32
	incusIdleConnTimeout     = 90 * time.Second
	incusDialTimeout         = 10 * time.Second
	incusTLSHandshakeTimeout = 5 * time.Second

	// An operation wait long-polls, hence the hour.
	incusResponseHeaderTimeout = time.Hour
	incusExpectContinueTimeout = 30 * time.Second
)

// Connection talks the Incus REST API to one daemon.
type Connection struct {
	http      *http.Client
	baseURL   string
	userAgent string

	// Reported by GetConnectionInfo; socketPath is empty over TLS.
	socketPath string
	serverCert string

	// events is this Connection's own listeners, never shared with a copy.
	events *incusEvents

	// eventSilence is how long an event socket may say nothing before it counts
	// as dead. A test shortens it, having no half hour to wait.
	eventSilence time.Duration
}

// NewConnection dials an Incus daemon, over its unix socket or over TLS.
func NewConnection(info *ConfigRemoteInfo) (*Connection, error) {
	if len(info.Addrs) == 0 {
		return nil, fmt.Errorf("%q: %w", info.Name, ErrConnectionNoAddress)
	}

	c := &Connection{
		userAgent:    info.UserAgent,
		serverCert:   info.ServerCert,
		events:       &incusEvents{},
		eventSilence: incusEventSilence,
	}

	// Only the first address is tried, where upstream probes the whole rolling list.
	addr := info.Addrs[0]

	transport := incusTransport()

	if strings.HasPrefix(addr, "unix:") {
		socket := incusSocketPath(addr)

		transport.DialContext = func(ctx context.Context, _ string, _ string) (net.Conn, error) {
			return (&net.Dialer{Timeout: incusDialTimeout}).DialContext(ctx, "unix", socket)
		}

		c.socketPath = socket
		c.baseURL = "http://unix.socket"
		c.http = &http.Client{Transport: transport}

		return c, nil
	}

	tlsConfig, err := incusTLS.GetTLSConfigMem(info.ClientCert, info.ClientKey, info.ClientCA, info.ServerCert, info.InsecureSkipVerify)
	if err != nil {
		return nil, fmt.Errorf("%q: building the TLS config: %w", info.Name, err)
	}

	transport.TLSClientConfig = tlsConfig
	transport.Proxy = http.ProxyFromEnvironment
	transport.DialContext = (&net.Dialer{Timeout: incusDialTimeout}).DialContext

	c.baseURL = strings.TrimSuffix(addr, "/")
	c.http = &http.Client{Transport: transport}

	return c, nil
}

// incusTransport is the tuning both address kinds share.
//
// There is deliberately no http.Client.Timeout: it bounds the whole request,
// which would cut off the event stream, an operation long-poll and every
// console or SFTP transfer.
func incusTransport() *http.Transport {
	return &http.Transport{
		// Upstream sets DisableKeepAlives, paying a TCP and TLS handshake per request.
		DisableKeepAlives:   false,
		MaxIdleConns:        incusMaxIdleConns,
		MaxIdleConnsPerHost: incusMaxIdleConnsPerHost,

		// Unbounded on purpose: the worker pool is where concurrency is bounded.
		MaxConnsPerHost: 0,

		IdleConnTimeout:     incusIdleConnTimeout,
		TLSHandshakeTimeout: incusTLSHandshakeTimeout,

		// Time to the first byte: an operation wait sends no header until it finishes.
		ResponseHeaderTimeout: incusResponseHeaderTimeout,
		ExpectContinueTimeout: incusExpectContinueTimeout,

		// Events and exec need an HTTP/1.1 upgrade, which h2 does not do.
		ForceAttemptHTTP2: false,
	}
}

// WithMaxIdleConns returns a copy that keeps at most conns idle connections,
// perHost of them to any one host. The copy starts with a pool of its own,
// because resizing a live one under in-flight requests is a race.
//
// Nothing has to be handed back. A Connection is not a resource with a lifetime
// of its own: what it holds is a transport, and an abandoned one closes its idle
// sockets after incusIdleConnTimeout and is then collected. Callers drop
// connections; they do not close them.
func (c *Connection) WithMaxIdleConns(conns int, perHost int) *Connection {
	clone := *c

	// Listeners are never inherited: a copy that listens gets a socket of its own.
	clone.events = &incusEvents{}

	transport, ok := c.http.Transport.(*http.Transport)
	if !ok {
		// Nothing to retune, so the copy shares c's pool.
		return &clone
	}

	// Clone carries the dialer, the TLS config and the rest of the tuning over.
	tuned := transport.Clone()
	tuned.MaxIdleConns = conns
	tuned.MaxIdleConnsPerHost = perHost

	client := *c.http
	client.Transport = tuned
	clone.http = &client

	return &clone
}

// incusSocketPath resolves the socket for a unix address: the path it carries,
// else INCUS_SOCKET, else INCUS_DIR, else the usual places.
func incusSocketPath(addr string) string {
	path := strings.TrimPrefix(strings.TrimPrefix(addr, "unix://"), "unix:")
	if path != "" {
		return path
	}

	path = os.Getenv("INCUS_SOCKET")
	if path != "" {
		return path
	}

	dir := os.Getenv("INCUS_DIR")
	if dir == "" {
		_, err := os.Lstat("/run/incus/unix.socket")
		if err == nil {
			dir = "/run/incus"
		} else {
			dir = "/var/lib/incus"
		}
	}

	socket := filepath.Join(dir, "unix.socket")

	// incus-user hands unprivileged callers a socket of their own.
	userSocket := filepath.Join(dir, "unix.socket.user")
	if !util.PathIsWritable(socket) && util.PathIsWritable(userSocket) {
		return userSocket
	}

	return socket
}

// uriFor renders a path under /1.0 in project. An empty project sends none,
// which incusd reads as the default project.
func uriFor(project string, path string, query url.Values) string {
	if project != "" {
		if query == nil {
			query = url.Values{}
		}

		query.Set("project", project)
	}

	uri := "/1.0" + path
	if len(query) > 0 {
		uri += "?" + query.Encode()
	}

	return uri
}

// do runs one request against /1.0 in project, and returns the decoded envelope
// plus the ETag a conditional update needs.
func (c *Connection) do(ctx context.Context, project string, method string, path string, query url.Values, body any, etag string) (*api.Response, string, error) {
	return c.doPath(ctx, method, uriFor(project, path, query), body, etag)
}

// doPath is do without the /1.0 prefix or the project, for a caller that has
// built the whole path itself.
func (c *Connection) doPath(ctx context.Context, method string, path string, body any, etag string) (*api.Response, string, error) {
	var reader io.Reader

	contentType := ""

	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return nil, "", fmt.Errorf("encoding the request body: %w", err)
		}

		reader = bytes.NewReader(encoded)
		contentType = "application/json"
	}

	return c.send(ctx, method, path, reader, contentType, etag, nil)
}

// send issues one request and decodes the API envelope it answers with. header
// carries what an upload cannot put in the body.
func (c *Connection) send(ctx context.Context, method string, path string, body io.Reader, contentType string, etag string, header http.Header) (*api.Response, string, error) {
	req, err := c.request(ctx, method, path, body)
	if err != nil {
		return nil, "", err
	}

	req.Header.Set("Accept", "application/json")

	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}

	if etag != "" {
		req.Header.Set("If-Match", etag)
	}

	maps.Copy(req.Header, header)

	resp, err := c.http.Do(req) //nolint:gosec
	if err != nil {
		return nil, "", err
	}

	defer func() { _ = resp.Body.Close() }()

	decoded := &api.Response{}

	err = json.NewDecoder(resp.Body).Decode(decoded)
	if err != nil {
		return nil, "", fmt.Errorf("decoding %s %s: %w", method, path, err)
	}

	if decoded.Type == api.ErrorResponse {
		return nil, "", busyError(api.StatusErrorf(decoded.Code, "%s", decoded.Error), decoded.Error)
	}

	return decoded, resp.Header.Get("ETag"), nil
}

// doRaw hands back the undecoded body of a /1.0 endpoint that answers with
// something other than an API envelope. The caller closes it.
func (c *Connection) doRaw(ctx context.Context, project string, method string, path string, query url.Values) (io.ReadCloser, error) {
	req, err := c.request(ctx, method, uriFor(project, path, query), nil)
	if err != nil {
		return nil, err
	}

	resp, err := c.http.Do(req) //nolint:gosec
	if err != nil {
		return nil, err
	}

	if resp.StatusCode != http.StatusOK {
		defer func() { _ = resp.Body.Close() }()

		// A failure still answers with an envelope.
		decoded := &api.Response{}

		err = json.NewDecoder(resp.Body).Decode(decoded)
		if err != nil {
			return nil, api.StatusErrorf(resp.StatusCode, "%s %s: %s", method, path, resp.Status)
		}

		return nil, api.StatusErrorf(decoded.Code, "%s", decoded.Error)
	}

	return resp.Body, nil
}

// request builds a request against the connection's server.
func (c *Connection) request(ctx context.Context, method string, path string, body io.Reader) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, body) //nolint:gosec // the address is the configured remote's
	if err != nil {
		return nil, err
	}

	if c.userAgent != "" {
		req.Header.Set("User-Agent", c.userAgent)
	}

	return req, nil
}

// resourceNames turns the resource URLs a non-recursive collection returns
// into bare names.
func resourceNames(collection string, uris []string) ([]string, error) {
	names := make([]string, 0, len(uris))

	for _, uri := range uris {
		parsed, err := url.Parse(uri)
		if err != nil {
			return nil, fmt.Errorf("parsing the resource URL %q: %w", uri, err)
		}

		_, name, found := strings.Cut(parsed.Path, collection+"/")
		if !found {
			return nil, fmt.Errorf("unexpected resource URL %q", uri)
		}

		names = append(names, name)
	}

	return names, nil
}

// getStruct GETs path in project and unmarshals the response metadata into target.
func (c *Connection) getStruct(ctx context.Context, project string, path string, query url.Values, target any) (string, error) {
	resp, etag, err := c.do(ctx, project, http.MethodGet, path, query, nil, "")
	if err != nil {
		return "", err
	}

	err = resp.MetadataAsStruct(target)
	if err != nil {
		return "", fmt.Errorf("decoding the metadata of GET %s: %w", path, err)
	}

	return etag, nil
}
