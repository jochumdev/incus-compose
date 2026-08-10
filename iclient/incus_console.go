package iclient

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"sync"

	"github.com/lxc/incus/v7/shared/api"
)

// GetInstanceConsoleLog returns the console ring buffer Incus keeps for an
// instance, which is what `incus console --show-log` prints. The caller closes
// it.
func (c *Connection) GetInstanceConsoleLog(ctx context.Context, name string) (io.ReadCloser, error) {
	return c.doRaw(ctx, http.MethodGet, incusInstancePath(name, "/console"), nil)
}

// ConsoleInstance attaches to an instance's console and copies it to
// args.Output until the context is done or the operation ends. Canceling the
// context detaches, which ends the operation server-side.
func (c *Connection) ConsoleInstance(ctx context.Context, name string, console api.InstanceConsolePost, args *InstanceConsoleArgs) (<-chan api.Operation, error) {
	if args == nil || args.Output == nil {
		return nil, fmt.Errorf("attaching to the console of %q: an output is required", name)
	}

	if console.Type == "" {
		console.Type = "console"
	}

	updates, err := c.asyncOperation(ctx, http.MethodPost, incusInstancePath(name, "/console"), console, "")
	if err != nil {
		return nil, err
	}

	// The first value carries the fd secrets.
	started, ok := <-updates
	if !ok {
		return nil, fmt.Errorf("attaching to the console of %q: the operation reported nothing", name)
	}

	fds, ok := started.Metadata["fds"].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("attaching to the console of %q: the operation advertised no file descriptors", name)
	}

	// Canceling this closes the sockets; a websocket read only ends when the socket does.
	streamCtx, closeStreams := context.WithCancel(ctx)

	// Opened and left alone: the server waits for every advertised descriptor.
	_, err = c.dialOperation(streamCtx, started.ID, consoleSecret(fds, api.SecretNameControl))
	if err != nil {
		closeStreams()

		return nil, fmt.Errorf("attaching to the control channel of the console on %q: %w", name, err)
	}

	socket, err := c.dialOperation(streamCtx, started.ID, consoleSecret(fds, "0"))
	if err != nil {
		closeStreams()

		return nil, fmt.Errorf("attaching to the console on %q: %w", name, err)
	}

	streams := &sync.WaitGroup{}
	streams.Add(1)

	go func() {
		defer streams.Done()

		drainWebsocket(socket, args.Output)
	}()

	return withDrain(updates, started, streams, closeStreams), nil
}

// consoleSecret reads one file descriptor's secret out of the operation.
func consoleSecret(fds map[string]any, name string) string {
	secret, _ := fds[name].(string)

	return secret
}
