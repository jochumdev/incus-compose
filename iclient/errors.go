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

	// ErrConnectionUnsupported is returned by a Connection asked for an
	// operation its remote cannot serve, e.g. an instance call on a
	// simplestreams remote.
	ErrConnectionUnsupported = errors.New("operation not supported by this connection")

	// ErrInstanceBusy is returned when another operation holds the instance's
	// lock. Wait it out with Connection.WaitInstanceBusy.
	ErrInstanceBusy = errors.New("instance is busy")

	// ErrVolumeInUse is returned when a storage volume still has a user, which
	// for a volume several instances share is every instance but the last.
	ErrVolumeInUse = errors.New("storage volume is still in use")

	// ErrRegistryProtocol is returned by NewRepository for a remote that is
	// not an OCI registry.
	ErrRegistryProtocol = errors.New("remote is not an OCI registry")

	// ErrRegistryAddrCredentials is returned by NewRepository for an address
	// carrying credentials, which belong in Username and Password.
	ErrRegistryAddrCredentials = errors.New("registry address carries credentials")

	// ErrCredHelper is returned when a remote's credentials helper fails.
	ErrCredHelper = errors.New("credentials helper failed")
)

// incusBusyMessage is how incusd words the instance operation lock, in
// internal/server/instance/operationlock. There is no status code for it.
const incusBusyMessage = "Instance is busy"

// incusVolumeInUseMessage is how incusd refuses to delete a volume with users,
// in cmd/incusd/storage_volumes.go. There is no status code for it either.
const incusVolumeInUseMessage = "storage volume is still in use"

// busyError adds ErrInstanceBusy or ErrVolumeInUse to err when the message
// reports one of the two locks incusd words rather than gives a code to.
func busyError(err error, message string) error {
	if strings.Contains(message, incusVolumeInUseMessage) {
		return fmt.Errorf("%w: %w", ErrVolumeInUse, err)
	}

	if !strings.Contains(message, incusBusyMessage) {
		return err
	}

	return fmt.Errorf("%w: %w", ErrInstanceBusy, err)
}
