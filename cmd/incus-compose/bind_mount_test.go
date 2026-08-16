package main

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/lxc/incus-compose/client"
	"github.com/lxc/incus-compose/internal/testlib"
)

func TestBindMounts(t *testing.T) {
	t.Parallel()
	testlib.SkipLocal(t)

	pn := t.Name()
	compose := testlib.Fixture(t, "with-bind-mounts", "compose.yaml")
	ctx := t.Context()

	gc, err := client.NewTestClient(ctx)
	if err != nil {
		t.Skip(err.Error())
	}

	skipNotSameHost(t, gc)

	t.Cleanup(func() {
		_, _ = testlib.RunCompose(context.Background(), t, pn, "", nil, "-f", compose, "down", "--project")
	})

	_, err = testlib.RunCompose(ctx, t, pn, "", nil, "-f", compose, "up", "--detach")
	require.NoError(t, err)

	t.Run("file bind-mount", func(t *testing.T) {
		t.Parallel()
		err := pollServiceHTTP(ctx, t, pn, compose, "file-web", "file-bind-mount-ok", 60*time.Second)
		require.NoError(t, err)
	})

	t.Run("dir bind-mount", func(t *testing.T) {
		t.Parallel()
		err := pollServiceHTTP(ctx, t, pn, compose, "dir-web", "dir-bind-mount-ok", 60*time.Second)
		require.NoError(t, err)
	})
}

func TestBindMountErrorsOnRemote(t *testing.T) {
	t.Parallel()
	testlib.SkipLocal(t)

	pn := t.Name()
	compose := testlib.Fixture(t, "with-bind-mounts", "compose.yaml")
	ctx := t.Context()

	gc, err := client.NewTestClient(ctx)
	if err != nil {
		t.Skip(err.Error())
	}

	if gc.SameHost() == nil {
		t.Skip("this test requires the incus server to be on a remote host")
	}

	_, err = testlib.RunCompose(ctx, t, pn, "", nil, "-f", compose, "up", "--detach")
	require.Error(t, err)
}

func TestSeededBindMounts(t *testing.T) {
	t.Parallel()
	testlib.SkipLocal(t)

	pn := t.Name()
	compose := testlib.Fixture(t, "with-seeded-bind-mounts", "compose.yaml")
	ctx := t.Context()

	if _, err := client.NewTestClient(ctx); err != nil {
		t.Skip(err.Error())
	}

	t.Cleanup(func() {
		_, _ = testlib.RunCompose(context.Background(), t, pn, "", nil, "-f", compose, "down", "--project")
	})

	_, err := testlib.RunCompose(ctx, t, pn, "", nil, "-f", compose, "up", "--detach")
	require.NoError(t, err)

	t.Run("file bind-mount", func(t *testing.T) {
		t.Parallel()
		err := pollServiceHTTP(ctx, t, pn, compose, "file-web", "file-bind-mount-ok", 60*time.Second)
		require.NoError(t, err)
	})

	t.Run("dir bind-mount", func(t *testing.T) {
		t.Parallel()
		err := pollServiceHTTP(ctx, t, pn, compose, "dir-web", "dir-bind-mount-ok", 60*time.Second)
		require.NoError(t, err)
	})
}

func TestBindMountNoShift(t *testing.T) {
	t.Parallel()
	testlib.SkipLocal(t)

	pn := t.Name()
	compose := testlib.Fixture(t, "with-bind-mount-no-shift", "compose.yaml")
	ctx := t.Context()

	gc, err := client.NewTestClient(ctx)
	if err != nil {
		t.Skip(err.Error())
	}

	skipNotSameHost(t, gc)

	t.Cleanup(func() {
		_, _ = testlib.RunCompose(context.Background(), t, pn, "", nil, "-f", compose, "down", "--project")
	})

	_, err = testlib.RunCompose(ctx, t, pn, "", nil, "-f", compose, "up", "--detach")
	require.NoError(t, err)

	// With security.shifted=false the bind mount is not id-shifted, so the host
	// file shows up as nobody (65534) inside the unprivileged container.
	err = pollServiceExec(ctx, t, pn, compose, "web",
		[]string{"ls", "-ln", "/www/index.html"}, "65534", 60*time.Second)
	require.NoError(t, err)
}

// pollServiceHTTP execs wget inside the service's instance until the response
// body contains want or timeout elapses.
func pollServiceHTTP(ctx context.Context, t *testing.T, pn, compose, service, want string, timeout time.Duration) error {
	return pollServiceExec(ctx, t, pn, compose, service,
		[]string{"wget", "-q", "-O", "-", "http://127.0.0.1:8080/"}, want, timeout)
}

// pollServiceExec runs `incus-compose exec` for the service until stdout
// contains want or timeout elapses. Checks before sleeping so the last attempt
// is never skipped.
func pollServiceExec(ctx context.Context, t *testing.T, pn, compose, service string, cmd []string, want string, timeout time.Duration) error {
	t.Helper()

	args := append([]string{"-f", compose, "exec", "--no-tty", service, "--"}, cmd...)

	deadline := time.Now().Add(timeout)
	var lastOut string
	var lastErr error

	for {
		out, err := testlib.RunCompose(ctx, t, pn, "", nil, args...)
		if err == nil {
			if !strings.Contains(out, want) {
				return fmt.Errorf("%q not found in output %q", want, out)
			}

			return nil
		}

		lastOut = out
		lastErr = err

		if time.Now().After(deadline) {
			break
		}
		time.Sleep(time.Second)
	}

	return fmt.Errorf("timed out after %s: last output=%q: %w", timeout, lastOut, lastErr)
}
