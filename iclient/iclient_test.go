package iclient

import (
	"os"
	"testing"
)

// The environment a stage runs in. `just test-local` and `just test-e2e` set one
// each; `just test` sets neither, which is what makes it the middle stage.
const (
	envLocal = "INCUS_COMPOSE_TEST_LOCAL"
	envE2E   = "INCUS_COMPOSE_TEST_E2E"
)

// skipLocal skips a test that needs a real Incus server.
func skipLocal(t *testing.T) {
	t.Helper()

	if os.Getenv(envLocal) != "" {
		t.Skip("needs a real Incus: " + envLocal + " is set, run `just test`")
	}
}

// skipE2E skips a slow test that stands up a fixture stack. Opposite polarity to
// SkipLocal: an end-to-end test runs only when asked for.
func skipE2E(t *testing.T) {
	t.Helper()

	skipLocal(t)

	if os.Getenv(envE2E) == "" {
		t.Skip("long end-to-end test: set " + envE2E + "=1, or run `just test-e2e`")
	}
}
