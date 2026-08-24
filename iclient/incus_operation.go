package iclient

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"

	"github.com/lxc/incus/v7/shared/api"
)

// incusOperationsPath is the collection every operation call hangs off.
const incusOperationsPath = "/operations"

// incusOperationBuffer holds updates for a caller that is between reads.
const incusOperationBuffer = 8

// asyncOperation issues a request the server answers asynchronously and
// returns the operation's updates.
//
// The listener opens before the request goes out, so an operation that finishes
// immediately is still reported.
//
// The channel carries the operation as accepted, then every update, and closes
// on a terminal state - the last value is the outcome, including its Err.
//
// A token operation is the exception: it waits to be used rather than
// finishing, so read its first value and cancel the context.
func (c *Connection) asyncOperation(ctx context.Context, project string, method string, path string, body any, etag string) (<-chan api.Operation, error) {
	return c.async(ctx, project, method+" "+path, func(sendCtx context.Context) (*api.Response, error) {
		resp, _, err := c.do(sendCtx, project, method, path, nil, body, etag)

		return resp, err
	})
}

// asyncUpload is asyncOperation for a request that streams a body, which is
// how an image is imported from tarballs. header carries what cannot be JSON.
func (c *Connection) asyncUpload(ctx context.Context, project string, path string, body io.Reader, contentType string, header http.Header) (<-chan api.Operation, error) {
	return c.async(ctx, project, http.MethodPost+" "+path, func(sendCtx context.Context) (*api.Response, error) {
		resp, _, err := c.send(sendCtx, http.MethodPost, uriFor(project, path, nil), body, contentType, "", header)

		return resp, err
	})
}

// async subscribes, then sends, then follows whatever operation came back.
//
// project has to be the one the operation runs in: incusd filters the event
// stream by project, so a listener anywhere else never sees the updates and the
// caller blocks until its context ends.
func (c *Connection) async(ctx context.Context, project string, what string, send func(context.Context) (*api.Response, error)) (<-chan api.Operation, error) {
	listenCtx, cancel := context.WithCancel(ctx)

	events, err := c.ListenEvents(listenCtx, project, []string{api.EventTypeOperation})
	if err != nil {
		cancel()

		return nil, err
	}

	resp, err := send(ctx)
	if err != nil {
		cancel()

		return nil, err
	}

	if resp.Type != api.AsyncResponse {
		cancel()

		return nil, fmt.Errorf("%s: expected an async response, got %q", what, resp.Type)
	}

	started := api.Operation{}

	err = resp.MetadataAsStruct(&started)
	if err != nil {
		cancel()

		return nil, fmt.Errorf("decoding the operation of %s: %w", what, err)
	}

	return c.followOperation(listenCtx, cancel, events, started), nil
}

// followOperation feeds the caller's channel: the operation as it stands, then
// every update the event stream carries for it, until it reaches an end state.
func (c *Connection) followOperation(ctx context.Context, cancel context.CancelFunc, events <-chan api.Event, started api.Operation) <-chan api.Operation {
	updates := make(chan api.Operation, incusOperationBuffer)

	go func() {
		defer close(updates)

		// Stops the event listener, whichever way this returns.
		defer cancel()

		if !emitOperation(ctx, updates, started) {
			return
		}

		for event := range events {
			update := api.Operation{}

			err := json.Unmarshal(event.Metadata, &update)
			if err != nil || update.ID != started.ID {
				continue
			}

			if !emitOperation(ctx, updates, update) {
				return
			}
		}
	}()

	return updates
}

// emitOperation delivers one update and reports whether to keep going.
func emitOperation(ctx context.Context, updates chan<- api.Operation, op api.Operation) bool {
	select {
	case updates <- op:
	case <-ctx.Done():
		return false
	}

	return !op.StatusCode.IsFinal()
}

// GetOperations returns the operations running in project.
func (c *Connection) GetOperations(ctx context.Context, project string) ([]api.Operation, error) {
	// Grouped by status, e.g. {"running": [...]}.
	byStatus := map[string][]api.Operation{}

	query := url.Values{}
	query.Set("recursion", "1")

	_, err := c.getStruct(ctx, project, incusOperationsPath, query, &byStatus)
	if err != nil {
		return nil, err
	}

	operations := []api.Operation{}
	for _, group := range byStatus {
		operations = append(operations, group...)
	}

	return operations, nil
}

// ListenOperation follows an operation this connection did not start. It
// cannot subscribe before the operation exists, so it reads the operation once
// after subscribing.
func (c *Connection) ListenOperation(ctx context.Context, project string, op api.Operation) (<-chan api.Operation, error) {
	listenCtx, cancel := context.WithCancel(ctx)

	events, err := c.ListenEvents(listenCtx, project, []string{api.EventTypeOperation})
	if err != nil {
		cancel()

		return nil, err
	}

	current := api.Operation{}

	_, err = c.getStruct(ctx, project, incusOperationsPath+"/"+url.PathEscape(op.ID), nil, &current)
	if err != nil {
		cancel()

		return nil, err
	}

	return c.followOperation(listenCtx, cancel, events, current), nil
}

// WaitOperationID blocks until the operation ends and returns how it ended.
//
// The server holds the request open, so unlike ListenOperation this costs one
// request rather than an event socket.
func (c *Connection) WaitOperationID(ctx context.Context, project string, id string) (*api.Operation, error) {
	operation := api.Operation{}

	// -1 stops the server applying a timeout of its own and answering early.
	query := url.Values{}
	query.Set("timeout", "-1")

	_, err := c.getStruct(ctx, project, incusOperationsPath+"/"+url.PathEscape(id)+"/wait", query, &operation)
	if err != nil {
		return nil, err
	}

	return &operation, nil
}

// CancelOperation asks the server to cancel an operation.
func (c *Connection) CancelOperation(ctx context.Context, project string, op api.Operation) error {
	_, _, err := c.do(ctx, project, http.MethodDelete, incusOperationsPath+"/"+url.PathEscape(op.ID), nil, nil, "")

	return err
}

// WaitOperation reads an operation to its end and returns the outcome. The
// last value is the result, so a failure comes back as an error here.
//
// Never call it on a token operation, which waits to be used rather than finishing.
func WaitOperation(ctx context.Context, updates <-chan api.Operation) (api.Operation, error) {
	last := api.Operation{}

	for {
		select {
		case update, ok := <-updates:
			if !ok {
				if last.Err != "" {
					// Incus takes the instance lock in the driver, so a busy write fails from here.
					return last, busyError(
						fmt.Errorf("operation %s: %s", last.ID, last.Err), last.Err)
				}

				if !last.StatusCode.IsFinal() {
					return last, fmt.Errorf("operation %s ended at %q", last.ID, last.Status)
				}

				return last, nil
			}

			last = update

		case <-ctx.Done():
			return last, ctx.Err()
		}
	}
}
