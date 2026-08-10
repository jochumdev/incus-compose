package client

import (
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/lxc/incus-compose/iclient"
)

// ----------------------------------------------------------------------------
// NewError Tests
// ----------------------------------------------------------------------------

func TestNewError_CreatesError(t *testing.T) {
	err := NewError("test error")
	assert.NotNil(t, err)
	assert.Equal(t, "test error", err.Error())
}

func TestNewError_DifferentSentinels(t *testing.T) {
	err1 := NewError("error one")
	err2 := NewError("error one") // Same text, different sentinel

	assert.False(t, errors.Is(err1, err2), "different NewError calls should create different sentinels")
}

// ----------------------------------------------------------------------------
// Error() Tests
// ----------------------------------------------------------------------------

func TestError_Error_Simple(t *testing.T) {
	err := NewError("simple error")
	assert.Equal(t, "simple error", err.Error())
}

func TestError_Error_WithWrapped(t *testing.T) {
	inner := fmt.Errorf("inner error")
	err := NewError("outer error").Wrap(inner)
	assert.Equal(t, "outer error: inner error", err.Error())
}

// ----------------------------------------------------------------------------
// Is() Tests
// ----------------------------------------------------------------------------

func TestError_Is_SameSentinel(t *testing.T) {
	sentinel := NewError("sentinel")
	derived := sentinel.WithText("extra context")

	assert.True(t, errors.Is(derived, sentinel))
}

func TestError_Is_DifferentSentinel(t *testing.T) {
	err1 := NewError("error one")
	err2 := NewError("error two")

	assert.False(t, errors.Is(err1, err2))
}

func TestError_Is_WithChainedContext(t *testing.T) {
	sentinel := NewError("sentinel")
	derived := sentinel.WithText("one").WithText("two").WithAction(ActionEnsure)

	assert.True(t, errors.Is(derived, sentinel))
}

func TestError_Is_WithWrappedError(t *testing.T) {
	sentinel := NewError("sentinel")
	inner := fmt.Errorf("inner")
	derived := sentinel.Wrap(inner)

	assert.True(t, errors.Is(derived, sentinel))
}

func TestError_Is_NonErrorTarget(t *testing.T) {
	err := NewError("test")
	other := fmt.Errorf("other")

	assert.False(t, errors.Is(err, other))
}

// ----------------------------------------------------------------------------
// As() Tests
// ----------------------------------------------------------------------------

func TestError_As_ExtractsError(t *testing.T) {
	original := NewError("original").WithText("context")

	var extracted *Error
	ok := errors.As(original, &extracted)

	assert.True(t, ok)
	assert.Equal(t, original, extracted)
}

func TestError_As_ExtractsFromChain(t *testing.T) {
	sentinel := NewError("sentinel")
	derived := sentinel.WithText("context").WithAction(ActionEnsure)

	var extracted *Error
	ok := errors.As(derived, &extracted)

	assert.True(t, ok)
	assert.Equal(t, derived, extracted)
	assert.True(t, errors.Is(extracted, sentinel))
}

// ----------------------------------------------------------------------------
// Unwrap() Tests
// ----------------------------------------------------------------------------

func TestError_Unwrap_NoWrapped(t *testing.T) {
	err := NewError("test")
	assert.Nil(t, errors.Unwrap(err))
}

func TestError_Unwrap_WithWrapped(t *testing.T) {
	inner := fmt.Errorf("inner error")
	err := NewError("outer").Wrap(inner)

	unwrapped := errors.Unwrap(err)
	assert.Equal(t, inner, unwrapped)
}

func TestError_Unwrap_NestedChain(t *testing.T) {
	innermost := fmt.Errorf("innermost")
	middle := fmt.Errorf("middle: %w", innermost)
	outer := NewError("outer").Wrap(middle)

	// First unwrap gets middle
	unwrapped := errors.Unwrap(outer)
	assert.Equal(t, middle, unwrapped)

	// Second unwrap gets innermost
	unwrapped = errors.Unwrap(unwrapped)
	assert.Equal(t, innermost, unwrapped)
}

// ----------------------------------------------------------------------------
// Wrap() Tests
// ----------------------------------------------------------------------------

func TestError_Wrap_PreservesSentinel(t *testing.T) {
	sentinel := NewError("sentinel")
	inner := fmt.Errorf("inner")
	wrapped := sentinel.Wrap(inner)

	assert.True(t, errors.Is(wrapped, sentinel))
}

func TestError_Wrap_IncludesInnerMessage(t *testing.T) {
	sentinel := NewError("outer")
	inner := fmt.Errorf("inner details")
	wrapped := sentinel.Wrap(inner)

	assert.Contains(t, wrapped.Error(), "outer")
	assert.Contains(t, wrapped.Error(), "inner details")
}

func TestError_Wrap_MultipleWraps(t *testing.T) {
	sentinel := NewError("sentinel")
	inner1 := fmt.Errorf("inner1")
	inner2 := fmt.Errorf("inner2")

	wrapped1 := sentinel.Wrap(inner1)
	wrapped2 := sentinel.Wrap(inner2)

	// Both should still match sentinel
	assert.True(t, errors.Is(wrapped1, sentinel))
	assert.True(t, errors.Is(wrapped2, sentinel))

	// But have different wrapped errors
	assert.NotEqual(t, errors.Unwrap(wrapped1), errors.Unwrap(wrapped2))
}

// ----------------------------------------------------------------------------
// WithText() Tests
// ----------------------------------------------------------------------------

func TestError_WithText_AddsContext(t *testing.T) {
	sentinel := NewError("base")
	derived := sentinel.WithText("extra info")

	assert.Contains(t, derived.Error(), "base")
	assert.Contains(t, derived.Error(), "extra info")
}

func TestError_WithText_PreservesSentinel(t *testing.T) {
	sentinel := NewError("sentinel")
	derived := sentinel.WithText("context")

	assert.True(t, errors.Is(derived, sentinel))
}

func TestError_WithText_Chaining(t *testing.T) {
	sentinel := NewError("base")
	derived := sentinel.WithText("one").WithText("two")

	assert.Contains(t, derived.Error(), "base")
	assert.Contains(t, derived.Error(), "one")
	assert.Contains(t, derived.Error(), "two")
	assert.True(t, errors.Is(derived, sentinel))
}

// ----------------------------------------------------------------------------
// WithAction() Tests
// ----------------------------------------------------------------------------

func TestError_WithAction_AddsAction(t *testing.T) {
	sentinel := NewError("failed")
	derived := sentinel.WithAction(ActionEnsure)

	assert.Contains(t, derived.Error(), "failed")
	assert.Contains(t, derived.Error(), string(ActionEnsure))
}

func TestError_WithAction_PreservesSentinel(t *testing.T) {
	sentinel := NewError("sentinel")
	derived := sentinel.WithAction(ActionDelete)

	assert.True(t, errors.Is(derived, sentinel))
}

// ----------------------------------------------------------------------------
// WithKindName() Tests
// ----------------------------------------------------------------------------

func TestError_WithKindName_AddsKindAndName(t *testing.T) {
	sentinel := NewError("not found")
	derived := sentinel.WithKindName(KindInstance, "web")

	assert.Contains(t, derived.Error(), "not found")
	assert.Contains(t, derived.Error(), string(KindInstance))
	assert.Contains(t, derived.Error(), "web")
}

func TestError_WithKindName_PreservesSentinel(t *testing.T) {
	sentinel := NewError("sentinel")
	derived := sentinel.WithKindName(KindNetwork, "mynet")

	assert.True(t, errors.Is(derived, sentinel))
}

// ----------------------------------------------------------------------------
// WithResource() Tests
// ----------------------------------------------------------------------------

func TestError_WithResource_AddsResourceContext(t *testing.T) {
	sentinel := NewError("operation failed")
	resource := newMockResource("myapp-web", KindInstance, PriorityInstance, false)
	derived := sentinel.WithResource(resource)

	assert.Contains(t, derived.Error(), "operation failed")
	assert.Contains(t, derived.Error(), string(KindInstance))
	assert.Contains(t, derived.Error(), "myapp-web")
}

func TestError_WithResource_PreservesSentinel(t *testing.T) {
	sentinel := NewError("sentinel")
	resource := newMockResource("default", KindProfile, PriorityProfile, false)
	derived := sentinel.WithResource(resource)

	assert.True(t, errors.Is(derived, sentinel))
}

// ----------------------------------------------------------------------------
// Combined Context Tests
// ----------------------------------------------------------------------------

func TestError_CombinedContext_AllMethodsChained(t *testing.T) {
	sentinel := NewError("base error")
	inner := fmt.Errorf("root cause")
	resource := newMockResource("web", KindInstance, PriorityInstance, false)

	derived := sentinel.
		WithAction(ActionEnsure).
		WithResource(resource).
		Wrap(inner)

	// All context present
	assert.Contains(t, derived.Error(), "base error")
	assert.Contains(t, derived.Error(), "root cause")

	// Sentinel preserved
	assert.True(t, errors.Is(derived, sentinel))

	// Wrapped accessible
	assert.Equal(t, inner, errors.Unwrap(derived))

	// As works
	var extracted *Error
	assert.True(t, errors.As(derived, &extracted))
}

// ----------------------------------------------------------------------------
// Sentinel Error Tests
// ----------------------------------------------------------------------------

func TestSentinelErrors_AreDistinct(t *testing.T) {
	sentinels := []*Error{
		ErrNotFound,
		ErrNotEnsured,
		ErrOperation,
		ErrCreate,
		ErrAborted,
		ErrConnectionFailed,
		ErrDisconnected,
		ErrUnsupportedAction,
		ErrUnknownResource,
		ErrUnknownConfig,
		ErrInvalidFormat,
		ErrImageSource,
		ErrImageRequired,
		ErrDeviceConflict,
		ErrVolumeMismatch,
		ErrBadDeviceConfig,
		ErrDependencyNotEnsured,
		ErrNilPointer,
		ErrUnknown,
	}

	for i, s1 := range sentinels {
		for j, s2 := range sentinels {
			if i == j {
				assert.True(t, errors.Is(s1, s2), "sentinel should match itself: %v", s1)
			} else {
				assert.False(t, errors.Is(s1, s2), "different sentinels should not match: %v vs %v", s1, s2)
			}
		}
	}
}

func TestSentinelErrors_DerivedMatchOriginal(t *testing.T) {
	tests := []struct {
		name     string
		sentinel *Error
	}{
		{"ErrNotFound", ErrNotFound},
		{"ErrOperation", ErrOperation},
		{"ErrCreate", ErrCreate},
		{"ErrInvalidFormat", ErrInvalidFormat},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			derived := tt.sentinel.WithText("extra context").WithAction(ActionEnsure)
			assert.True(t, errors.Is(derived, tt.sentinel))
		})
	}
}

// ----------------------------------------------------------------------------
// Immutability Tests
// ----------------------------------------------------------------------------

func TestError_MethodsDoNotMutateSentinel(t *testing.T) {
	original := ErrNotFound
	originalMsg := original.Error()

	// Call various methods
	_ = original.WithText("extra")
	_ = original.WithAction(ActionEnsure)
	_ = original.WithKindName(KindInstance, "web")
	_ = original.Wrap(fmt.Errorf("inner"))

	// Original should be unchanged
	assert.Equal(t, originalMsg, original.Error())
	assert.Nil(t, errors.Unwrap(original))
}

// retryBusy stands on this: iclient's sentinel must survive ErrOperation.Wrap.
func TestError_Wrap_PreservesForeignSentinel(t *testing.T) {
	busy := fmt.Errorf("%w: operation 1234: Instance is busy", iclient.ErrInstanceBusy)

	wrapped := ErrOperation.Wrap(busy)

	assert.True(t, errors.Is(wrapped, iclient.ErrInstanceBusy), "the sentinel must survive Wrap")
	assert.True(t, errors.Is(wrapped, ErrOperation), "and so must our own")
	assert.False(t, errors.Is(wrapped, ErrNotFound))
}
