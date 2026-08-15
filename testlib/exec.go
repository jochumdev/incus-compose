package testlib

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/kballard/go-shellquote"
	"github.com/stretchr/testify/require"

	"github.com/lxc/incus-compose/shared"
)

var rootPath = ""

func DebugCommands() bool {
	_, debug := os.LookupEnv("INCUS_COMPOSE_TEST_DEBUG")
	_, trace := os.LookupEnv("INCUS_COMPOSE_TEST_TRACE")

	return debug || trace
}

func TraceCommands() bool {
	_, ok := os.LookupEnv("INCUS_COMPOSE_TEST_TRACE")

	return ok
}

// RepoRoot is the absolute path of this checkout, derived from this file's own
// location so it does not depend on the working directory.
func RepoRoot(t *testing.T) string {
	t.Helper()

	if rootPath != "" {
		return rootPath
	}

	out := &bytes.Buffer{}
	cmd := exec.CommandContext(t.Context(), "go", "list", "-m", "-f", "\"{{ .Dir }}\"") // nolint:gosec
	cmd.Stdout = out
	require.NoError(t, cmd.Run(), "cannot determine the path of the go.mod")

	rootPath = strings.Trim(out.String(), "\n \"")

	return rootPath
}

// FixtureRoot is the compose stacks this suite boots, at the top of the
// checkout: they are the fleet, not this package's testdata.
func FixtureRoot(t *testing.T) string {
	t.Helper()

	return filepath.Join(RepoRoot(t), "test", "fixtures")
}

// Exec executes a command with extra environment and returns what it wrote to
// stdout. What it wrote to stderr comes back on the error.
func Exec(ctx context.Context, t *testing.T, cwd string, env []string, name string, args ...string) (string, error) {
	t.Helper()

	var out, stderr bytes.Buffer

	cmd := exec.CommandContext(ctx, name, args...) // nolint:gosec
	cmd.Dir = cwd
	cmd.Env = append(os.Environ(), env...)
	if TraceCommands() {
		cmd.Stdout = io.MultiWriter(&out, t.Output())
		cmd.Stderr = io.MultiWriter(&stderr, t.Output())
	} else {
		cmd.Stdout = &out
	}

	if DebugCommands() {
		line := name + " " + shellquote.Join(args...)
		redactedEnv := slices.Clone(env)
		for i, e := range redactedEnv {
			if strings.HasPrefix(e, "INCUS_TOKEN=") {
				redactedEnv[i] = "INCUS_TOKEN=<redacted>"
			}
		}

		slog.Log(ctx, shared.LevelTrace, "Running", "cwd", cwd, "cmd", line, "env", redactedEnv)
	}

	err := cmd.Run()

	// Carried on the error rather than logged, so a command that failed says why
	// wherever its error is reported, and one that worked says nothing at all.
	if err != nil && stderr.Len() > 0 {
		err = fmt.Errorf("%w: %s", err, strings.TrimSpace(stderr.String()))
	}

	return out.String(), err
}

// RunCompose runs the compose project in dir with the env and args.
func RunCompose(ctx context.Context, t *testing.T, project, dir string, env []string, args ...string) (string, error) {
	t.Helper()

	// --os-env is what lets a fixture read DNS_DOMAIN and COREFILE from the
	// environment set below.
	full := append([]string{
		"run",
		"./cmd/incus-compose/...",
		"--debug",
		"--os-env",
		"--project-name", project,
		"--project-directory", dir,
	}, args...)

	return Exec(ctx, t, RepoRoot(t), env, "go", full...)
}
