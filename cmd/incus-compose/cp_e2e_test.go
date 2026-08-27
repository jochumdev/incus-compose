package main

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lxc/incus-compose/internal/testlib"
)

// TestE2ECp drives cp against a live instance: both directions, the stdout
// target the usage advertises, and a directory, which is what the unconditional
// --recursive is there for.
func TestE2ECp(t *testing.T) {
	t.Parallel()
	testlib.SkipE2E(t)

	ctx := t.Context()
	pn := t.Name()

	dir := testlib.WriteTempFiles(t, map[string]string{
		"compose.yaml": `services:
  web:
    image: docker.io/alpine:edge
    x-incus:
      oci.entrypoint: sh
`,
		"push.txt": "pushed by cp\n",
	})
	compose := filepath.Join(dir, "compose.yaml")

	testlib.CleanupCompose(t, pn, "-f", compose, "down", "--project")

	_, err := testlib.RunCompose(ctx, t, pn, "", nil, "-f", compose, "up", "--detach")
	require.NoError(t, err)

	t.Run("a push lands the file in the instance", func(t *testing.T) {
		_, err := testlib.RunCompose(ctx, t, pn, "", nil,
			"-f", compose, "cp", filepath.Join(dir, "push.txt"), "web:/tmp/push.txt")
		require.NoError(t, err)

		stdout, err := testlib.RunCompose(ctx, t, pn, "", nil,
			"-f", compose, "incus", "exec", "web-1", "--", "cat", "/tmp/push.txt")
		require.NoError(t, err)
		assert.Equal(t, "pushed by cp", strings.TrimSpace(stdout))
	})

	t.Run("a pull brings it back", func(t *testing.T) {
		target := filepath.Join(t.TempDir(), "pulled.txt")

		_, err := testlib.RunCompose(ctx, t, pn, "", nil,
			"-f", compose, "cp", "web:/tmp/push.txt", target)
		require.NoError(t, err)

		content, err := os.ReadFile(target)
		require.NoError(t, err)
		assert.Equal(t, "pushed by cp\n", string(content))
	})

	// The usage and the docs both advertise "-", and unconditional --recursive
	// is the thing that could take it away.
	t.Run("a pull to - writes the file to stdout", func(t *testing.T) {
		stdout, err := testlib.RunCompose(ctx, t, pn, "", nil,
			"-f", compose, "cp", "web:/tmp/push.txt", "-")
		require.NoError(t, err)
		assert.Equal(t, "pushed by cp\n", stdout)
	})

	t.Run("a relative path resolves from the instance root", func(t *testing.T) {
		stdout, err := testlib.RunCompose(ctx, t, pn, "", nil,
			"-f", compose, "cp", "web:tmp/push.txt", "-")
		require.NoError(t, err)
		assert.Equal(t, "pushed by cp\n", stdout)
	})

	t.Run("a directory pulls recursively", func(t *testing.T) {
		_, err := testlib.RunCompose(ctx, t, pn, "", nil,
			"-f", compose, "incus", "exec", "web-1", "--", "sh", "-c",
			"mkdir -p /tmp/tree/nested && echo one > /tmp/tree/one.txt && echo two > /tmp/tree/nested/two.txt")
		require.NoError(t, err)

		target := t.TempDir()

		_, err = testlib.RunCompose(ctx, t, pn, "", nil,
			"-f", compose, "cp", "web:/tmp/tree", target)
		require.NoError(t, err)

		// Asserted by name rather than by path: where incus roots a recursive
		// pull is its call, and what matters here is that both levels arrived.
		found := map[string]bool{}
		require.NoError(t, filepath.WalkDir(target, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}

			if !d.IsDir() {
				found[d.Name()] = true
			}

			return nil
		}))

		assert.True(t, found["one.txt"], "the top level file, got %v", found)
		assert.True(t, found["two.txt"], "the nested file, got %v", found)
	})

	t.Run("neither path naming a service is an error", func(t *testing.T) {
		_, err := testlib.RunCompose(ctx, t, pn, "", nil,
			"-f", compose, "cp", filepath.Join(dir, "push.txt"), "/tmp/nowhere.txt")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "Neither path names a service")
	})

	t.Run("both paths naming a service is an error", func(t *testing.T) {
		_, err := testlib.RunCompose(ctx, t, pn, "", nil,
			"-f", compose, "cp", "web:/tmp/push.txt", "web:/tmp/other.txt")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "Copying between services is not supported")
	})
}

// TestE2ECpDryRun pins the incus command line cp hands off, which is all of cp
// that is ours.
func TestE2ECpDryRun(t *testing.T) {
	t.Parallel()
	testlib.SkipE2E(t)

	ctx := t.Context()
	pn := t.Name()

	dir := testlib.WriteTempFiles(t, map[string]string{
		"compose.yaml": `services:
  web:
    image: docker.io/alpine:edge
    x-incus:
      oci.entrypoint: sh
`,
	})
	compose := filepath.Join(dir, "compose.yaml")

	testlib.CleanupCompose(t, pn, "-f", compose, "down", "--project")

	_, err := testlib.RunCompose(ctx, t, pn, "", nil, "-f", compose, "up", "--detach")
	require.NoError(t, err)

	tests := []struct {
		name string
		args []string
		want string
	}{
		{
			name: "a push owns the file by the instance's user",
			args: []string{"./local.txt", "web:/tmp/there.txt"},
			want: "file push --recursive --uid 0 --gid 0 ./local.txt web-1/tmp/there.txt",
		},
		{
			name: "--archive keeps what the source had",
			args: []string{"--archive", "./local.txt", "web:/tmp/there.txt"},
			want: "file push --recursive ./local.txt web-1/tmp/there.txt",
		},
		{
			name: "a pull needs no ownership at all",
			args: []string{"web:/tmp/there.txt", "./local.txt"},
			want: "file pull --recursive web-1/tmp/there.txt ./local.txt",
		},
		{
			// Ownership is the source's on a pull whatever we pass, so the flag
			// is accepted for `docker compose cp -a` and changes nothing here.
			name: "--archive on a pull is inert",
			args: []string{"--archive", "web:/tmp/there.txt", "./local.txt"},
			want: "file pull --recursive web-1/tmp/there.txt ./local.txt",
		},
		{
			name: "--follow-link dereferences",
			args: []string{"--follow-link", "web:/tmp/there.txt", "./local.txt"},
			want: "file pull --recursive --dereference web-1/tmp/there.txt ./local.txt",
		},
		{
			name: "a leading slash on the instance side is optional",
			args: []string{"web:tmp/there.txt", "-"},
			want: "file pull --recursive web-1/tmp/there.txt -",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			args := append([]string{"-f", compose, "cp", "--dry-run"}, tt.args...)

			stdout, err := testlib.RunCompose(ctx, t, pn, "", nil, args...)
			require.NoError(t, err)

			execPath, cmdLine, ok := strings.Cut(strings.TrimSpace(stdout), " ")
			require.True(t, ok, "unexpected output: %q", stdout)
			assert.Equal(t, "incus", filepath.Base(execPath))
			assert.Equal(t, tt.want, cmdLine)
		})
	}
}
