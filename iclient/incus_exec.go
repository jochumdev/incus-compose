package iclient

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sync"

	"github.com/gorilla/websocket"
	"github.com/lxc/incus/v7/shared/api"
)

// InstanceExecArgs is where a non-interactive exec sends its output. A nil
// writer discards that stream.
//
// The exit code is on the terminal operation, in Metadata["return"].
type InstanceExecArgs struct {
	Stdout io.Writer
	Stderr io.Writer
}

// ExecInstance runs a command in an instance and streams its output.
//
// The channel closes once the output has drained and the operation has
// finished, so ranging over it to the end is enough.
func (c *Connection) ExecInstance(ctx context.Context, name string, exec api.InstanceExecPost, args *InstanceExecArgs) (<-chan api.Operation, error) {
	if args == nil {
		args = &InstanceExecArgs{}
	}

	if exec.Interactive {
		return nil, fmt.Errorf("an interactive exec: %w", ErrConnectionUnsupported)
	}

	// Without this the server runs the command detached, with no descriptors to read.
	exec.WaitForWS = true

	updates, err := c.asyncOperation(ctx, http.MethodPost, incusInstancePath(name, "/exec"), exec, "")
	if err != nil {
		return nil, err
	}

	// The first value carries the fd secrets.
	started, ok := <-updates
	if !ok {
		return nil, fmt.Errorf("exec on %q: the operation reported nothing", name)
	}

	// Canceling this closes the streams; a websocket read only ends when the socket does.
	streamCtx, closeStreams := context.WithCancel(ctx)

	streams := &sync.WaitGroup{}

	// Every advertised descriptor has to be connected or the server times out waiting.
	writers := map[string]io.Writer{"1": args.Stdout, "2": args.Stderr}

	fds, ok := started.Metadata["fds"].(map[string]any)
	if !ok {
		closeStreams()

		return nil, fmt.Errorf("exec on %q: the operation advertised no file descriptors", name)
	}

	for fd, raw := range fds {
		secret, ok := raw.(string)
		if !ok || secret == "" {
			continue
		}

		socket, err := c.dialOperation(streamCtx, started.ID, secret)
		if err != nil {
			closeStreams()

			return nil, fmt.Errorf("attaching fd %s of the exec on %q: %w", fd, name, err)
		}

		// No stdin, so close it at once and let the command read EOF.
		if fd == "0" {
			_ = socket.WriteMessage(websocket.CloseMessage,
				websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
			_ = socket.Close()

			continue
		}

		streams.Add(1)

		go func() {
			defer streams.Done()

			// A websocket read only returns when the socket closes, so close it on ctx.
			go func() {
				<-streamCtx.Done()
				_ = socket.Close()
			}()

			// A nil writer, which is what control gets, discards.
			drainWebsocket(socket, writers[fd])
		}()
	}

	return withDrain(updates, started, streams, closeStreams), nil
}

// dialOperation opens one of an operation's websockets.
func (c *Connection) dialOperation(ctx context.Context, id string, secret string) (*websocket.Conn, error) {
	transport, ok := c.http.Transport.(*http.Transport)
	if !ok {
		return nil, fmt.Errorf("unexpected transport %T", c.http.Transport)
	}

	query := url.Values{}
	query.Set("secret", secret)

	if c.project != "" {
		query.Set("project", c.project)
	}

	dialer := websocket.Dialer{
		NetDialContext:   transport.DialContext,
		TLSClientConfig:  transport.TLSClientConfig,
		Proxy:            transport.Proxy,
		HandshakeTimeout: incusTLSHandshakeTimeout,
	}

	uri := c.websocketURL("/1.0/operations/"+url.PathEscape(id)+"/websocket", query)

	socket, resp, err := dialer.DialContext(ctx, uri, nil)
	if resp != nil {
		_ = resp.Body.Close()
	}

	if err != nil {
		return nil, err
	}

	// The dialer's context covers the handshake only, so unblock a reader by closing.
	go func() {
		<-ctx.Done()
		_ = socket.Close()
	}()

	return socket, nil
}

// drainWebsocket copies one stream to its writer until the server closes it.
func drainWebsocket(socket *websocket.Conn, writer io.Writer) {
	defer func() { _ = socket.Close() }()

	for {
		_, reader, err := socket.NextReader()
		if err != nil {
			return
		}

		if writer == nil {
			// Still read it: an undrained stream stalls the command.
			_, _ = io.Copy(io.Discard, reader)

			continue
		}

		_, err = io.Copy(writer, reader)
		if err != nil {
			return
		}
	}
}

// withDrain republishes an operation's updates, holding the close until the
// output streams have finished.
func withDrain(updates <-chan api.Operation, started api.Operation, streams *sync.WaitGroup, closeStreams context.CancelFunc) <-chan api.Operation {
	out := make(chan api.Operation, incusOperationBuffer)

	go func() {
		defer close(out)

		// The first value was consumed to reach the fd secrets.
		out <- started

		for update := range updates {
			out <- update
		}

		// The streams have normally drained by here; this covers the one that did not.
		closeStreams()
		streams.Wait()
	}()

	return out
}
