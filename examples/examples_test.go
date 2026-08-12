package examples

import (
	"bytes"
	"context"
	"log/slog"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/bradleyjkemp/cupaloy/v2"
	"github.com/stretchr/testify/require"

	"github.com/lxc/incus-compose/testlib"
)

var snapshotter = cupaloy.New(cupaloy.SnapshotSubdirectory(filepath.Join("..", "test", "snapshots", "examples")))

func runCommand(ctx context.Context, t *testing.T, projectName string, args ...string) (*bytes.Buffer, error) {
	t.Helper()

	mArgs := append([]string{"run", "--", "github.com/lxc/incus-compose/cmd/incus-compose/..."},
		testlib.Args(projectName, args...)...)
	slog.DebugContext(ctx, "Running", "args", mArgs)

	stdout := &bytes.Buffer{}
	execCmd := exec.CommandContext(ctx, "go", mArgs...) //nolint:gosec
	execCmd.Stdout = stdout
	execCmd.Stderr = t.Output()

	err := execCmd.Run()
	return stdout, err
}

func stripOutput(t *testing.T, output *bytes.Buffer) string {
	t.Helper()

	return testlib.Strip(output.String())
}

// func TestMain(m *testing.M) {
// 	logger := slog.New(slog.NewTextHandler(
// 		os.Stderr,
// 		&slog.HandlerOptions{Level: slog.LevelDebug - 4}),
// 	)

// 	slog.SetDefault(logger)

// 	code := m.Run()
// 	os.Exit(code)
// }

func TestExample(t *testing.T) {
	t.Parallel()
	testlib.SkipExamples(t)

	examples := []struct {
		name string
		dir  string
	}{
		{
			name: "hugo",
			dir:  "./hugo/",
		},
		{
			name: "leafwiki",
			dir:  "./leafwiki/",
		},
		{
			name: "immich",
			dir:  "./immich/",
		},
		{
			name: "many-dependencies",
			dir:  "./many-dependencies/",
		},
		{
			name: "wikijs",
			dir:  "./wikijs/",
		},
	}

	for _, example := range examples {
		t.Run(example.name, func(t *testing.T) {
			t.Parallel()

			ctx := t.Context()
			t.Cleanup(func() {
				_, _ = runCommand(context.Background(), t, t.Name(), "--project-directory", example.dir, "down", "--project")
			})

			args := []string{"--project-directory", example.dir, "up", "--detach"}
			_, err := runCommand(ctx, t, t.Name(), args...)
			require.NoError(t, err)

			// Sometimes this is needed to get the real health status.
			time.Sleep(1 * time.Second)

			args = []string{"--project-directory", example.dir, "list", "--format", "json"}
			stdout, err := runCommand(ctx, t, t.Name(), args...)
			require.NoError(t, err)

			snapshotter.SnapshotT(t, stripOutput(t, stdout))
		})
	}
}
