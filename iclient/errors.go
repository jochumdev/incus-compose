package iclient

import (
	"errors"
	"fmt"
	"strings"
)

// The package's sentinels, matched with errors.Is.
var (
	// ErrConfigRemoteNotFound is returned for a remote the configuration
	// does not name.
	ErrConfigRemoteNotFound = errors.New("remote not found")

	// ErrConnectionNoAddress is returned for a remote with nothing to dial.
	ErrConnectionNoAddress = errors.New("remote has no address")

	// ErrConnectionDisconnected is returned by a Connection used after
	// Disconnect.
	ErrConnectionDisconnected = errors.New("connection is disconnected")

	// ErrConnectionUnsupported is returned by a Connection asked for an
	// operation its remote cannot serve, e.g. an instance call on a
	// simplestreams remote.
	ErrConnectionUnsupported = errors.New("operation not supported by this connection")

	// ErrInstanceBusy is returned when another operation holds the instance's
	// lock. Wait it out with Connection.WaitInstanceBusy.
	ErrInstanceBusy = errors.New("instance is busy")
)

// incusBusyMessage is how incusd words the instance operation lock, in
// internal/server/instance/operationlock. There is no status code for it.
const incusBusyMessage = "Instance is busy"

// busyError adds ErrInstanceBusy to err when message reports the lock.
func busyError(err error, message string) error {
	if !strings.Contains(message, incusBusyMessage) {
		return err
	}

	return fmt.Errorf("%w: %w", ErrInstanceBusy, err)
}
