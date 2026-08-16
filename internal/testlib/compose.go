package testlib

import (
	"context"
	"os"
	"slices"
	"strings"
	"testing"
)

// ProjectName makes an Incus project name out of anything, so a test can pass
// t.Name() and get one back.
func ProjectName(name string) string {
	return strings.ToLower(strings.ReplaceAll(name, "/", "-"))
}

// Args builds the incus-compose arguments for a project, ahead of whatever the
// caller runs them with.
func Args(project string, args ...string) []string {
	return append([]string{"--debug", "--project-name", ProjectName(project)}, args...)
}

func KeepTestData() bool {
	_, keep := os.LookupEnv("INCUS_COMPOSE_TEST_KEEP")
	return keep
}

// RunCompose runs incus-compose as a subprocess. An empty dir leaves
// --project-directory off, and a non-empty env implies --os-env.
func RunCompose(ctx context.Context, t *testing.T, project, dir string, env []string, args ...string) (string, error) {
	t.Helper()

	full := []string{}
	if len(env) > 0 {
		full = append(full, "--os-env")
	}

	if dir != "" {
		full = append(full, "--project-directory", dir)
	}

	full = append(full, Args(project, args...)...)

	// A race report leaves the exit code at 0, so a run that found one passes.
	env = append(slices.Clone(env), "GORACE=halt_on_error=1")
	if cover := os.Getenv(EnvCoverDir); cover != "" {
		env = append(env, "GOCOVERDIR="+cover)
	}

	return Exec(ctx, t, RepoRoot(t), env, ComposeBin(t), full...)
}
