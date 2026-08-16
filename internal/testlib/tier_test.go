package testlib_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/lxc/incus-compose/internal/testlib"
)

// ran reports whether the body carried on past the guard. A skip unwinds its own
// goroutine, so reaching the next line is the only observable outcome.
func ran(t *testing.T, guard func(*testing.T)) bool {
	t.Helper()

	reached := false

	t.Run("probe", func(t *testing.T) {
		guard(t)

		reached = true
	})

	return reached
}

// TestStageNames pins the literals: the other half of the contract is in
// just/test.just, which no Go test can read.
func TestStageNames(t *testing.T) {
	assert.Equal(t, "INCUS_COMPOSE_TEST_LOCAL", testlib.EnvLocal,
		"just/test.just sets this on test-local")
	assert.Equal(t, "INCUS_COMPOSE_TEST_E2E", testlib.EnvE2E,
		"just/test.just sets this on test-e2e")
}

// TestSkipLocal: a daemon test skips in the stage with no daemon, runs elsewhere.
func TestSkipLocal(t *testing.T) {
	t.Run("skips in the local stage", func(t *testing.T) {
		t.Setenv(testlib.EnvLocal, "1")
		assert.False(t, ran(t, testlib.SkipLocal))
	})

	t.Run("runs when nothing is set", func(t *testing.T) {
		t.Setenv(testlib.EnvLocal, "")
		assert.True(t, ran(t, testlib.SkipLocal))
	})

	t.Run("runs in the e2e stage", func(t *testing.T) {
		t.Setenv(testlib.EnvLocal, "")
		t.Setenv(testlib.EnvE2E, "1")
		assert.True(t, ran(t, testlib.SkipLocal))
	})
}

// TestSkipE2E: the opposite polarity, an e2e test runs only when asked for.
func TestSkipE2E(t *testing.T) {
	t.Run("skips when nothing is set", func(t *testing.T) {
		t.Setenv(testlib.EnvE2E, "")
		assert.False(t, ran(t, testlib.SkipE2E))
	})

	t.Run("skips in the local stage", func(t *testing.T) {
		t.Setenv(testlib.EnvLocal, "1")
		t.Setenv(testlib.EnvE2E, "")
		assert.False(t, ran(t, testlib.SkipE2E))
	})

	t.Run("runs in the e2e stage", func(t *testing.T) {
		t.Setenv(testlib.EnvE2E, "1")
		assert.True(t, ran(t, testlib.SkipE2E))
	})
}
