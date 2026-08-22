package testlib

import (
	"os"
	"testing"
)

// The environment a stage runs in. `just test-local` and `just test-e2e` set one
// each; `just test` sets neither, which is what makes it the middle stage.
const (
	EnvLocal    = "INCUS_COMPOSE_TEST_LOCAL"
	EnvE2E      = "INCUS_COMPOSE_TEST_E2E"
	EnvExamples = "INCUS_COMPOSE_TEST_EXAMPLES"
)

// SkipLocal skips a test that needs a real Incus server.
func SkipLocal(t *testing.T) {
	t.Helper()

	if os.Getenv(EnvLocal) != "" {
		t.Skip("needs a real Incus: " + EnvLocal + " is set, run `just test`")
	}
}

// SkipE2E skips a slow test that stands up a fixture stack. Opposite polarity to
// SkipLocal: an end-to-end test runs only when asked for.
func SkipE2E(t *testing.T) {
	t.Helper()

	SkipLocal(t)

	if os.Getenv(EnvE2E) == "" {
		t.Skip("long end-to-end test: set " + EnvE2E + "=1, or run `just test-e2e`")
	}
}

// SkipExamples skips a test that brings up a project from examples/. Same
// polarity as SkipE2E.
func SkipExamples(t *testing.T) {
	t.Helper()

	if os.Getenv(EnvExamples) == "" {
		t.Skip("examples test: set " + EnvExamples + "=1, or run `just test-examples`")
	}
}
