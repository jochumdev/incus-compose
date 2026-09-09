package checker

import (
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	incusApi "github.com/lxc/incus/v7/shared/api"
	"github.com/stretchr/testify/require"

	"github.com/lxc/incus-compose/internal/testlib"
	"github.com/lxc/incus-compose/shared"
)

// TestInstanceExecReportsTheExitCode pins the signal every check is built on.
func TestInstanceExecReportsTheExitCode(t *testing.T) {
	t.Parallel()
	testlib.SkipLocal(t)

	c := testProject(t, "healthd-exec-")
	name := testContainer(t, c, "web", nil, true)
	conn, project := testConn(t, c), c.Project()

	tests := []struct {
		name string
		cmd  []string
		want int
	}{
		{"success", []string{"/bin/true"}, 0},
		{"failure", []string{"/bin/false"}, 1},
		{"an explicit code", []string{"/bin/sh", "-c", "exit 7"}, 7},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			code, _, _, err := instanceExec(t.Context(), slog.Default(), conn, project, name, command{cmd: tt.cmd})

			require.NoError(t, err, "a command that ran is not an error, whatever it returned")
			require.Equal(t, tt.want, code)
		})
	}
}

// TestInstanceExecCapturesOutput pins that both streams come back, which is all
// a failing check leaves to debug with.
func TestInstanceExecCapturesOutput(t *testing.T) {
	t.Parallel()
	testlib.SkipLocal(t)

	c := testProject(t, "healthd-exec-out-")
	name := testContainer(t, c, "web", nil, true)

	code, stdout, stderr, err := instanceExec(t.Context(), slog.Default(), testConn(t, c), c.Project(), name,
		command{cmd: []string{"/bin/sh", "-c", "echo to-stdout; echo to-stderr >&2; exit 3"}})

	require.NoError(t, err)
	require.Equal(t, 3, code)
	require.Contains(t, stdout, "to-stdout")
	require.Contains(t, stderr, "to-stderr")
}

// TestInstanceExecHonoursOCIProcess is what an inherited HEALTHCHECK rests on:
// an image writes one for its own process, and an exec is none of those things
// by default.
func TestInstanceExecHonoursOCIProcess(t *testing.T) {
	t.Parallel()
	testlib.SkipLocal(t)

	c := testProject(t, "healthd-exec-proc-")
	name := testContainer(t, c, "web", nil, true)
	conn, project := testConn(t, c), c.Project()

	report := []string{"/bin/sh", "-c", "pwd; id -u; id -g"}

	code, stdout, _, err := instanceExec(t.Context(), slog.Default(), conn, project, name,
		command{cmd: report, cwd: "/tmp", uid: 1000, gid: 1001})
	require.NoError(t, err)
	require.Equal(t, 0, code)
	require.Equal(t, []string{"/tmp", "1000", "1001"}, strings.Fields(stdout))

	// Leaving them unset is what every caller passed before this, and has to
	// keep meaning "wherever exec lands, as root".
	_, stdout, _, err = instanceExec(t.Context(), slog.Default(), conn, project, name, command{cmd: report})
	require.NoError(t, err)
	require.Equal(t, []string{"/root", "0", "0"}, strings.Fields(stdout))
}

// TestInstanceExecHonoursTheContext is the guarantee the whole daemon rests on:
// a command that never returns must not hold its instance for good.
func TestInstanceExecHonoursTheContext(t *testing.T) {
	t.Parallel()
	testlib.SkipLocal(t)

	c := testProject(t, "healthd-exec-ctx-")
	name := testContainer(t, c, "web", nil, true)

	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()

	done := make(chan error, 1)
	go func() {
		_, _, _, err := instanceExec(ctx, slog.Default(), testConn(t, c), c.Project(), name, command{cmd: []string{"/bin/sh", "-c", "sleep 300"}})
		done <- err
	}()

	select {
	case err := <-done:
		require.Error(t, err, "a command outliving its budget must surface as a failure")
	case <-time.After(30 * time.Second):
		t.Fatal("instanceExec did not return for a 2s budget: a hung exec wedges its instance forever")
	}
}

// TestInstanceCheckActionVerdicts pins the mapping from a command's exit code to
// the health verdict the scheduler acts on.
func TestInstanceCheckActionVerdicts(t *testing.T) {
	t.Parallel()
	testlib.SkipLocal(t)

	c := testProject(t, "healthd-check-")
	name := testContainer(t, c, "web", nil, true)
	conn, project := testConn(t, c), c.Project()

	tests := []struct {
		name    string
		test    []string
		wantErr bool
	}{
		{"CMD that passes", []string{"CMD", "/bin/true"}, false},
		{"CMD that fails", []string{"CMD", "/bin/false"}, true},
		{"CMD-SHELL that passes", []string{"CMD-SHELL", "exit 0"}, false},
		{"CMD-SHELL that fails", []string{"CMD-SHELL", "exit 1"}, true},
		{"a bare command", []string{"/bin/true"}, false},
		{"NONE probes run state only", []string{"NONE"}, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res := instanceCheckAction(t.Context(), slog.Default(), conn, project, name, &instanceConfig{
				test:    tt.test,
				timeout: 30 * time.Second,
			})

			require.Equal(t, instanceResultChecked, res.kind)
			require.Equal(t, name, res.name)

			if tt.wantErr {
				require.Error(t, res.err)
				return
			}

			require.NoError(t, res.err)
		})
	}
}

// TestInstanceCheckActionNotRunning pins that a stopped instance is reported as
// a lifecycle fact, so the scheduler neither counts it nor writes a verdict.
func TestInstanceCheckActionNotRunning(t *testing.T) {
	t.Parallel()
	testlib.SkipLocal(t)

	c := testProject(t, "healthd-check-down-")
	name := testContainer(t, c, "web", nil, false)

	res := instanceCheckAction(t.Context(), slog.Default(), testConn(t, c), c.Project(), name, &instanceConfig{
		test:    []string{"CMD", "/bin/true"},
		timeout: 30 * time.Second,
	})

	require.ErrorIs(t, res.err, ErrNotRunning,
		"a stopped instance must be distinguishable from a failing one")
}

// TestInstanceRestartActionRestarts pins that a running instance is replaced,
// not merely left alone.
func TestInstanceRestartActionRestarts(t *testing.T) {
	t.Parallel()
	testlib.SkipLocal(t)

	c := testProject(t, "healthd-restart-")
	name := testContainer(t, c, "web", nil, true)
	conn, project := testConn(t, c), c.Project()

	before, _, err := conn.GetInstanceState(t.Context(), project, name)
	require.NoError(t, err)
	require.Equal(t, incusApi.Running, before.StatusCode)

	res := instanceRestartAction(t.Context(), conn, project, name)
	require.Equal(t, instanceResultRestarted, res.kind)
	require.NoError(t, res.err)

	after, _, err := conn.GetInstanceState(t.Context(), project, name)
	require.NoError(t, err)
	require.Equal(t, incusApi.Running, after.StatusCode)
	require.NotEqual(t, before.StartedAt, after.StartedAt, "the instance must actually have been replaced")
}

// TestInstanceRestartActionStartsAStoppedInstance pins the crash path: the
// instance is already down, so there is nothing to stop first.
func TestInstanceRestartActionStartsAStoppedInstance(t *testing.T) {
	t.Parallel()
	testlib.SkipLocal(t)

	c := testProject(t, "healthd-restart-down-")
	name := testContainer(t, c, "web", nil, false)
	conn, project := testConn(t, c), c.Project()

	res := instanceRestartAction(t.Context(), conn, project, name)
	require.NoError(t, res.err)

	state, _, err := conn.GetInstanceState(t.Context(), project, name)
	require.NoError(t, err)
	require.Equal(t, incusApi.Running, state.StatusCode)
}

// TestInstanceRestartActionRefusesAnIntentionalStop pins the one thing that must
// never happen: undoing an `incus-compose stop`.
func TestInstanceRestartActionRefusesAnIntentionalStop(t *testing.T) {
	t.Parallel()
	testlib.SkipLocal(t)

	c := testProject(t, "healthd-restart-marked-")
	name := testContainer(t, c, "web", map[string]string{shared.HealthStoppedKey: "true"}, false)
	conn, project := testConn(t, c), c.Project()

	res := instanceRestartAction(t.Context(), conn, project, name)

	require.ErrorIs(t, res.err, ErrIntentionallyStopped)

	state, _, err := conn.GetInstanceState(t.Context(), project, name)
	require.NoError(t, err)
	require.Equal(t, incusApi.Stopped, state.StatusCode,
		"a deliberately stopped instance must stay stopped")
}

// TestPatchInstanceConfigWritesOnlyItsKeys pins that the daemon's write is a
// patch, not a replace: it must not disturb keys it does not own.
func TestPatchInstanceConfigWritesOnlyItsKeys(t *testing.T) {
	t.Parallel()
	testlib.SkipLocal(t)

	c := testProject(t, "healthd-patch-")
	name := testContainer(t, c, "web", map[string]string{
		shared.HealthKeyPrefix + "test": `["CMD","/bin/true"]`,
		"user.keep.me":                  "untouched",
	}, false)
	conn, project := testConn(t, c), c.Project()

	require.NoError(t, WriteStatus(t.Context(), slog.Default(), conn, project, name, shared.HealthStatusUnhealthy))

	inst, _, err := conn.GetInstance(t.Context(), project, name, nil)
	require.NoError(t, err)

	require.Equal(t, shared.HealthStatusUnhealthy, inst.Config[shared.HealthStatusKey])
	require.Equal(t, "untouched", inst.Config["user.keep.me"])
	require.Equal(t, `["CMD","/bin/true"]`, inst.Config[shared.HealthKeyPrefix+"test"],
		"the daemon owns the status key and nothing else")
}
