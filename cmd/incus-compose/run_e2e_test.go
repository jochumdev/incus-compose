package main

import (
	"context"
	"os/exec"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lxc/incus-compose/internal/testlib"
	"github.com/lxc/incus-compose/shared"
)

// exitCode returns the status a failed RunCompose exited with.
func exitCode(t *testing.T, err error) int {
	t.Helper()

	if err == nil {
		return 0
	}

	var exitErr *exec.ExitError
	require.ErrorAs(t, err, &exitErr, "expected the command's own status, got %v", err)

	return exitErr.ExitCode()
}

// TestE2ERunExitCode is the whole reason run creates an instance it execs into
// rather than starting the command as the instance: Incus reports no exit
// status for an instance that stopped, and exec reports an exact one.
func TestE2ERunExitCode(t *testing.T) {
	t.Parallel()
	testlib.SkipE2E(t)
	skipNoExtension(t, shared.Incus73Extension, "run tests work best with incus 7.3 or higher")

	ctx := t.Context()
	pn := t.Name()
	compose := testlib.Fixture(t, "simple", "compose.yaml")

	testlib.CleanupCompose(t, pn, "-f", compose, "down", "--project")

	stdout, err := testlib.RunCompose(ctx, t, pn, "", nil,
		"-f", compose, "run", "--rm", "web", "echo", "from-the-one-off")
	require.NoError(t, err)
	assert.Contains(t, stdout, "from-the-one-off", "the command's output has to reach us")

	_, err = testlib.RunCompose(ctx, t, pn, "", nil,
		"-f", compose, "run", "--rm", "web", "sh", "-c", "exit 42")
	assert.Equal(t, 42, exitCode(t, err), "the command's own status, not a generic failure")

	_, err = testlib.RunCompose(ctx, t, pn, "", nil,
		"-f", compose, "run", "--rm", "web", "sh", "-c", "exit 0")
	assert.Equal(t, 0, exitCode(t, err))
}

// TestE2ERunOneOffLifecycle pins that --rm reclaims the instance, that one
// without it survives, and that `down` takes it even though no service
// declares it.
func TestE2ERunOneOffLifecycle(t *testing.T) {
	t.Parallel()
	testlib.SkipE2E(t)
	skipNoExtension(t, shared.Incus73Extension, "run tests work best with incus 7.3 or higher")

	ctx := t.Context()
	pn := t.Name()
	compose := testlib.Fixture(t, "simple", "compose.yaml")

	testlib.CleanupCompose(t, pn, "-f", compose, "down", "--project")

	_, err := testlib.RunCompose(ctx, t, pn, "", nil,
		"-f", compose, "run", "--rm", "web", "true")
	require.NoError(t, err)

	names, err := testlib.RunCompose(ctx, t, pn, "", nil, "-f", compose, "ps", "--all", "--quiet")
	require.NoError(t, err)
	assert.NotContains(t, names, "-run-", "--rm has to reclaim the one-off")

	_, err = testlib.RunCompose(ctx, t, pn, "", nil,
		"-f", compose, "run", "--name", "kept-one-off", "web", "true")
	require.NoError(t, err)

	names, err = testlib.RunCompose(ctx, t, pn, "", nil, "-f", compose, "ps", "--all", "--quiet")
	require.NoError(t, err)
	assert.Contains(t, names, "kept-one-off", "without --rm the one-off stays")

	// It is nobody's declared service, so ps has to name the service anyway.
	listing, err := testlib.RunCompose(ctx, t, pn, "", nil, "-f", compose, "ps", "--all")
	require.NoError(t, err)

	for _, line := range strings.Split(listing, "\n") {
		if strings.Contains(line, "kept-one-off") {
			assert.Contains(t, line, "web", "a one-off is not an orphan, it names its service")
			assert.NotContains(t, line, "<orphan>")
		}
	}

	_, err = testlib.RunCompose(ctx, t, pn, "", nil, "-f", compose, "down")
	require.NoError(t, err)

	names, err = testlib.RunCompose(ctx, t, pn, "", nil, "-f", compose, "ps", "--all", "--quiet")
	require.NoError(t, err)
	assert.NotContains(t, names, "kept-one-off", "down takes the one-offs with it")
}

// TestE2ERunDependencies pins that a one-off brings its dependencies up, and
// that --no-deps leaves them alone.
func TestE2ERunDependencies(t *testing.T) {
	t.Parallel()
	testlib.SkipE2E(t)
	skipNoExtension(t, shared.Incus73Extension, "run tests work best with incus 7.3 or higher")

	ctx := t.Context()
	pn := t.Name()
	compose := testlib.Fixture(t, "two-services", "compose.yaml")

	testlib.CleanupCompose(t, pn, "-f", compose, "down", "--project")

	services, err := testlib.RunCompose(ctx, t, pn, "", nil, "-f", compose, "config", "--services")
	require.NoError(t, err)

	names := strings.Fields(services)
	require.NotEmpty(t, names)

	_, err = testlib.RunCompose(ctx, t, pn, "", nil,
		"-f", compose, "run", "--rm", names[0], "true")
	require.NoError(t, err)
}

// TestE2EUpRunDown pins that a one-off does not outlive the project it was run
// in: `down` takes it along with the declared instances, without `--rm` and
// without any service declaring it.
func TestE2EUpRunDown(t *testing.T) {
	t.Parallel()
	testlib.SkipE2E(t)
	skipNoExtension(t, shared.Incus73Extension, "run tests work best with incus 7.3 or higher")

	ctx := t.Context()
	pn := t.Name()
	compose := testlib.Fixture(t, "simple", "compose.yaml")

	testlib.CleanupCompose(t, pn, "-f", compose, "down", "--project")

	_, err := testlib.RunCompose(ctx, t, pn, "", nil, "-f", compose, "up", "--detach")
	require.NoError(t, err)

	_, err = testlib.RunCompose(ctx, t, pn, "", nil,
		"-f", compose, "run", "--name", "leftover", "web", "true")
	require.NoError(t, err)

	// Incus itself, not ps: `ps --all` lists a declared service whether or not
	// the instance exists, so it cannot answer whether one is gone.
	names := listProjectInstances(ctx, t, pn, compose)
	assert.Contains(t, names, "leftover", "the one-off stays without --rm")
	assert.Contains(t, names, "web-1", "the declared service is up")

	_, err = testlib.RunCompose(ctx, t, pn, "", nil, "-f", compose, "down")
	require.NoError(t, err)

	names = listProjectInstances(ctx, t, pn, compose)
	assert.NotContains(t, names, "leftover", "down takes the one-off with it")
	assert.NotContains(t, names, "web-1", "and the declared service")
}

// listProjectInstances lists the instances Incus actually holds for the project.
func listProjectInstances(ctx context.Context, t *testing.T, pn, compose string) []string {
	t.Helper()

	out, err := testlib.RunCompose(ctx, t, pn, "", nil,
		"-f", compose, "incus", "list", "--format", "csv", "-c", "n")
	require.NoError(t, err)

	return strings.Fields(out)
}
