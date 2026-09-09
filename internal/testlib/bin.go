package testlib

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

// EnvCoverDir is where the CLI writes its counters; setting it builds with -cover.
const EnvCoverDir = "INCUS_COMPOSE_TEST_COVERDIR"

var (
	binOnce sync.Once
	binDir  string
	binPath string
	binErr  error
	version = "latest"
)

// ComposeBin builds the CLI once per test process, with the run's own -race and
// -cover settings. Main removes what it built.
func ComposeBin(t *testing.T) string {
	t.Helper()

	binOnce.Do(func() {
		binDir, binErr = os.MkdirTemp("", "incus-compose-test-")
		if binErr != nil {
			return
		}

		binPath = filepath.Join(binDir, "incus-compose")

		args := []string{"build", "-o", binPath, "-race", "-ldflags=-X github.com/lxc/incus-compose/cmd/incus-compose/version.Version=" + version}

		// Without -coverpkg the binary counts package main and nothing else.
		if os.Getenv(EnvCoverDir) != "" {
			args = append(args, "-cover", "-covermode", "atomic", "-coverpkg", "./...")
		}

		args = append(args, "./cmd/incus-compose")

		cmd := exec.CommandContext(context.Background(), "go", args...) // nolint:gosec
		cmd.Dir = RepoRoot(t)

		out, err := cmd.CombinedOutput()
		if err != nil {
			binErr = fmt.Errorf("building incus-compose: %w: %s", err, out)
		}
	})

	require.NoError(t, binErr)

	return binPath
}

// Main is a test package's TestMain: logger, run, teardown.
func Main(m *testing.M) int {
	InitSlog()

	code := m.Run()

	if binDir != "" {
		_ = os.RemoveAll(binDir)
	}

	return code
}
