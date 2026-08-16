package examples

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/bradleyjkemp/cupaloy/v2"
	"github.com/stretchr/testify/require"

	"github.com/lxc/incus-compose/internal/testlib"
)

var snapshotter = cupaloy.New(cupaloy.SnapshotSubdirectory(filepath.Join("..", "test", "snapshots", "examples")))

func TestMain(m *testing.M) {
	os.Exit(testlib.Main(m))
}

func TestExample(t *testing.T) {
	t.Parallel()
	testlib.SkipExamples(t)

	examples := []string{"hugo", "leafwiki", "immich", "many-dependencies", "wikijs"}

	for _, example := range examples {
		t.Run(example, func(t *testing.T) {
			t.Parallel()

			ctx := t.Context()
			dir := filepath.Join(testlib.RepoRoot(t), "examples", example)

			t.Cleanup(func() {
				_, _ = testlib.RunCompose(context.Background(), t, t.Name(), dir, nil, "down", "--project")
			})

			_, err := testlib.RunCompose(ctx, t, t.Name(), dir, nil, "up", "--detach")
			require.NoError(t, err)

			// Sometimes this is needed to get the real health status.
			time.Sleep(1 * time.Second)

			stdout, err := testlib.RunCompose(ctx, t, t.Name(), dir, nil, "list", "--format", "json")
			require.NoError(t, err)

			snapshotter.SnapshotT(t, testlib.Strip(stdout))
		})
	}
}
