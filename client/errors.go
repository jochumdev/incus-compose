package client

import (
	"errors"
	"fmt"
	"log/slog"
)

// Severity of an error, defaults to SeverityError.
const (
	SeverityError = slog.LevelError
	SeverityWarn  = slog.LevelWarn
)

// Error is a sentinel-based error type that supports context enrichment.
type Error struct {
	sentinel error
	text     string
	wrapped  error

	// severity indicates how important that error is, defaults to error.
	severity slog.Level
}

// NewError creates a new sentinel error.
func NewError(text string) *Error {
	return &Error{sentinel: errors.New(text), text: text, severity: SeverityError}
}

// WithSeverity returns a new error withe given severity.
func (e *Error) WithSeverity(severity slog.Level) *Error {
	return &Error{
		sentinel: e.sentinel,
		text:     e.text,
		wrapped:  e.wrapped,
		severity: severity,
	}
}

// WithKindName adds resource kind and name context to the error.
func (e *Error) WithKindName(kind Kind, name string) *Error {
	return &Error{
		sentinel: e.sentinel,
		text:     fmt.Sprintf("%v: %v(%v)", e.text, kind, name),
		wrapped:  e.wrapped,
		severity: e.severity,
	}
}

// WithText adds text context to the error.
func (e *Error) WithText(text string) *Error {
	return &Error{
		sentinel: e.sentinel,
		text:     fmt.Sprintf("%v: %v", e.text, text),
		wrapped:  e.wrapped,
		severity: e.severity,
	}
}

// WithAction adds action context to the error.
func (e *Error) WithAction(action Action) *Error {
	return &Error{
		sentinel: e.sentinel,
		text:     fmt.Sprintf("%v: %v", e.text, action),
		wrapped:  e.wrapped,
		severity: e.severity,
	}
}

// WithResource adds resource context to the error.
func (e *Error) WithResource(resource Resource) *Error {
	return &Error{
		sentinel: e.sentinel,
		text:     fmt.Sprintf("%v: %v", e.text, resource),
		wrapped:  e.wrapped,
		severity: e.severity,
	}
}

// Severity returns the errors severity, defaults to SeverityError.
func (e *Error) Severity() slog.Level {
	return e.severity
}

// Error implements the error interface.
func (e *Error) Error() string {
	if e.wrapped != nil {
		return e.text + ": " + e.wrapped.Error()
	}
	return e.text
}

// Unwrap returns the wrapped error for errors.Unwrap() support.
func (e *Error) Unwrap() error {
	return e.wrapped
}

// Wrap wraps another error, preserving the sentinel identity.
func (e *Error) Wrap(wrapped error) *Error {
	return &Error{sentinel: e.sentinel, text: e.text, wrapped: wrapped}
}

// Is implements errors.Is() support by comparing sentinel pointers.
func (e *Error) Is(target error) bool {
	if other, ok := target.(*Error); ok {
		return other.sentinel == e.sentinel
	}
	return false
}

// As implements errors.As() support by copying to target if it's *Error.
func (e *Error) As(target any) bool {
	if t, ok := target.(**Error); ok {
		*t = e
		return true
	}
	return false
}

var (
	// ErrUnsupportedAction indicates the resource does not support the action.
	ErrUnsupportedAction = NewError("resource does not support action")

	// ErrUnknown indicates an unknown error occurred.
	ErrUnknown = NewError("unknown")

	// ErrRunning indicates a command on a running instance.
	ErrRunning = NewError("resource is already running").WithSeverity(SeverityWarn)

	// ErrNotRunning indicates a command on a not running instance.
	ErrNotRunning = NewError("resource is not running").WithSeverity(SeverityWarn)

	// ErrUnknownConfig indicates an unknown config for a resource.
	ErrUnknownConfig = NewError("unknown config for resource")

	// ErrNilPointer indicates something is a nil pointer.
	ErrNilPointer = NewError("found a nil pointer")

	// ErrOperation is returned within an operation.
	ErrOperation = NewError("in an operation")

	// ErrBadDeviceConfig indicates a bad device config.
	ErrBadDeviceConfig = NewError("bad config for device")

	// ErrDependencyNotEnsured indicates a dependency is not ensured.
	ErrDependencyNotEnsured = NewError("dependency not ensured")

	// ErrDisconnected indicates an operation was attempted on a disconnected client.
	ErrDisconnected = NewError("client is not connected")

	// ErrConnectionFailed indicates a connection attempt failed.
	ErrConnectionFailed = NewError("connection failed")

	// ErrServerNotListening indicates the Incus server has no core.https_address
	// set. incus-compose copies cached images between projects using pull mode,
	// which needs the server reachable over the network.
	ErrServerNotListening = NewError("the incus server is not listening on the network (core.https_address is not set); incus-compose needs it for image caching, set it with `incus config set core.https_address=:8443`, see https://github.com/lxc/incus-compose/blob/main/docs/getting-started.md")

	// ErrServerTooOld indicates the Incus server has no oci_network_config API,
	// which every compose network attachment needs.
	ErrServerTooOld = NewError("the incus server has no oci_network_config API, incus-compose needs Incus 7.0.1 (LTS) or 7.2 and newer")

	// ErrAborted indicates an operation was aborted (e.g., by BeforeAny hook).
	ErrAborted = NewError("operation aborted")

	// ErrNotFound indicates a resource was not found.
	ErrNotFound = NewError("resource not found")

	// ErrNotEnsured indicates an operation requires the resource to be ensured first.
	ErrNotEnsured = NewError("resource not ensured").WithSeverity(SeverityWarn)

	// ErrImageRequired indicates an instance requires an image.
	ErrImageRequired = NewError("instances without an image are not yet supported")

	// ErrUnknownResource indicates an unknown resource kind.
	ErrUnknownResource = NewError("unknown resource kind")

	// ErrInvalidFormat indicates invalid format or syntax.
	ErrInvalidFormat = NewError("invalid format")

	// ErrImageSource indicates an image source error.
	ErrImageSource = NewError("image source error")

	// ErrNoPlatform indicates a multi-arch image carries no manifest for the
	// server's architecture.
	ErrNoPlatform = NewError("image has no manifest for the architecture")

	// ErrDeviceConflict indicates a device name conflict.
	ErrDeviceConflict = NewError("device conflict")

	// ErrVolumeMismatch indicates volume configuration mismatch.
	ErrVolumeMismatch = NewError("volume configuration mismatch")

	// ErrVolumeInUse indicates a volume that still has users, which for one
	// several instances share is every delete but the last.
	ErrVolumeInUse = NewError("volume still in use")

	// ErrCreate indicates a resource creation error.
	ErrCreate = NewError("create failed")

	// ErrDelete indicates a resource deletion error.
	ErrDelete = NewError("delete failed")

	// ErrBackupFailed indicates a backup failed.
	ErrBackupFailed = NewError("backup failed")

	// ErrBackupNotFound indicates a backup volume or restore point is missing.
	ErrBackupNotFound = NewError("backup not found")

	// ErrRestoreFailed indicates a restore failed.
	ErrRestoreFailed = NewError("restore failed")
)
