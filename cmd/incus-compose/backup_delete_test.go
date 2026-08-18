package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newestFirst is what readBackupManifests hands the retention logic.
func newestFirst() []backupManifest {
	return []backupManifest{
		{Timestamp: "2026-08-13T18:00:00Z", Name: "third"},
		{Timestamp: "2026-08-13T17:00:00Z", Name: "second"},
		{Timestamp: "2026-08-13T16:00:00Z", Name: "first"},
	}
}

func TestBackupsToDelete(t *testing.T) {
	t.Run("a timestamp picks exactly one run", func(t *testing.T) {
		got, err := backupsToDelete(newestFirst(), "2026-08-13T17:00:00Z", -1)
		require.NoError(t, err)
		require.Len(t, got, 1)

		assert.Equal(t, "second", got[0].Name)
	})

	t.Run("an unknown timestamp is an error", func(t *testing.T) {
		_, err := backupsToDelete(newestFirst(), "2020-01-01T00:00:00Z", -1)
		require.Error(t, err)
	})

	t.Run("keep-last drops the oldest", func(t *testing.T) {
		got, err := backupsToDelete(newestFirst(), "", 1)
		require.NoError(t, err)
		require.Len(t, got, 2)

		assert.Equal(t, "second", got[0].Name)
		assert.Equal(t, "first", got[1].Name)
	})

	t.Run("keep-last never drops the newest, which is the delta base", func(t *testing.T) {
		got, err := backupsToDelete(newestFirst(), "", 1)
		require.NoError(t, err)

		for _, m := range got {
			assert.NotEqual(t, "third", m.Name)
		}
	})

	t.Run("keep-last above the count deletes nothing", func(t *testing.T) {
		got, err := backupsToDelete(newestFirst(), "", 5)
		require.NoError(t, err)

		assert.Empty(t, got)
	})

	t.Run("keep-last zero deletes everything", func(t *testing.T) {
		got, err := backupsToDelete(newestFirst(), "", 0)
		require.NoError(t, err)

		assert.Len(t, got, 3)
	})

	t.Run("no backups at all", func(t *testing.T) {
		got, err := backupsToDelete(nil, "", 2)
		require.NoError(t, err)
		assert.Empty(t, got)

		_, err = backupsToDelete(nil, "2026-08-13T17:00:00Z", -1)
		require.Error(t, err)
	})
}

func TestPickBackupManifest(t *testing.T) {
	t.Run("an empty timestamp takes the newest", func(t *testing.T) {
		got, err := pickBackupManifest(newestFirst(), "")
		require.NoError(t, err)

		assert.Equal(t, "third", got.Name)
	})

	t.Run("no backups", func(t *testing.T) {
		_, err := pickBackupManifest(nil, "")
		require.Error(t, err)
	})
}
