package testlib

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// WriteTempFiles writes files, keyed by a path relative to a fresh temp
// directory, and returns that directory. Parents are created as needed, so a
// key may nest.
func WriteTempFiles(t *testing.T, files map[string]string) string {
	t.Helper()

	dir := t.TempDir()
	for name, content := range files {
		path := filepath.Join(dir, name)
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
		require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
	}

	return dir
}
