package main

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lxc/incus-compose/internal/testlib"
)

// TestExecSelectsCorrectInstance is a regression test for the exec command
// dispatching to the wrong instance when multiple services share a stack.
// It runs `hostname` in each service of a multi-service project and asserts
// the output matches the expected Incus instance name.
func TestExecSelectsCorrectInstance(t *testing.T) {
	t.Parallel()
	testlib.SkipLocal(t)
	testlib.SkipE2E(t)

	ctx := t.Context()
	pn := t.Name()
	compose := testlib.Fixture(t, "proxy", "compose.yaml")

	testlib.CleanupCompose(t, pn, "-f", compose, "down", "--project")

	_, err := testlib.RunCompose(ctx, t, pn, "", nil, "-f", compose, "up", "--detach")
	require.NoError(t, err)

	tests := []struct {
		service  string
		wantHost string
	}{
		{"frontend", "frontend-1"},
		{"backend1", "backend1-1"},
		{"backend2", "backend2-1"},
	}

	for _, tt := range tests {
		t.Run(tt.service, func(t *testing.T) {
			stdout, err := testlib.RunCompose(ctx, t, pn, "", nil, "-f", compose, "exec", "--no-tty", tt.service, "hostname")
			require.NoError(t, err)
			if strings.TrimSpace(stdout) != tt.wantHost {
				t.Errorf("got hostname %q, want %q", strings.TrimSpace(stdout), tt.wantHost)
			}
		})
	}
}

// TestExecRunsAsInstanceUser verifies exec defaults --user/--group to the
// instance's UID/GID (1000:1000 from the service `user:` override), so writing
// to the id-shifted named volume succeeds and the file lands owned by 1000:1000.
func TestE2EExecRunsAsInstanceUser(t *testing.T) {
	t.Parallel()
	testlib.SkipLocal(t)
	testlib.SkipE2E(t)

	ctx := t.Context()
	pn := t.Name()
	compose := testlib.Fixture(t, "with-user", "compose.yaml")

	testlib.CleanupCompose(t, pn, "-f", compose, "down", "--project", "--volumes")

	_, err := testlib.RunCompose(ctx, t, pn, "", nil, "-f", compose, "up", "--detach")
	require.NoError(t, err)

	// The write only succeeds if the process runs as 1000, since /data is owned
	// by the shifted instance user.
	_, err = testlib.RunCompose(ctx, t, pn, "", nil, "-f", compose, "exec", "--no-tty", "web",
		"--", "sh", "-c", "echo hello > /data/test.txt")
	require.NoError(t, err)

	stdout, err := testlib.RunCompose(ctx, t, pn, "", nil, "-f", compose, "exec", "--no-tty", "web",
		"--", "ls", "-ln", "/data/test.txt")
	require.NoError(t, err)

	// ls -ln columns: perms links owner group size date... name.
	fields := strings.Fields(stdout)
	require.GreaterOrEqual(t, len(fields), 4, "unexpected ls output: %q", stdout)
	assert.Equal(t, "1000", fields[2], "file owner uid")
	assert.Equal(t, "1000", fields[3], "file owner gid")
}
