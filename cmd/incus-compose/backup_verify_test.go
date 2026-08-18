package main

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lxc/incus-compose/client"
)

// manifestOf builds a run covering the named source volumes.
func manifestOf(timestamp string, volumes ...string) backupManifest {
	m := backupManifest{Timestamp: timestamp}
	for _, name := range volumes {
		m.Volumes = append(m.Volumes, client.BackupVolume{
			Source: client.VolumeInfos{Name: name},
			Backup: client.VolumeInfos{Name: client.BackupVolumePrefix + name},
		})
	}

	return m
}

func TestBackupVerifyStatus(t *testing.T) {
	cases := []struct {
		name      string
		snapshots []string
		want      string
	}{
		{"a volume that was never backed up", nil, backupVerifyNoVolume},
		{"a volume backed up at another time", []string{"2026-08-13T16:00:00Z"}, backupVerifyNoSnapshot},
		{"a volume with the restore point", []string{"2026-08-13T16:00:00Z", "2026-08-13T17:00:00Z"}, backupVerifyOK},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, backupVerifyStatus(tc.snapshots, "2026-08-13T17:00:00Z"))
		})
	}
}

func TestBackupVerifyDrift(t *testing.T) {
	t.Run("a project and a run that agree drift nowhere", func(t *testing.T) {
		got := backupVerifyDrift(manifestOf("t", "vol-a", "vol-b"), []string{"vol-a", "vol-b"})

		assert.Empty(t, got)
	})

	t.Run("a volume added since the backup is uncovered", func(t *testing.T) {
		got := backupVerifyDrift(manifestOf("t", "vol-a"), []string{"vol-a", "vol-new"})
		require.Len(t, got, 1)

		assert.Equal(t, "vol-new", got[0].Volume)
		assert.Equal(t, backupVerifyUncovered, got[0].Status)
	})

	t.Run("a volume dropped since the backup is gone", func(t *testing.T) {
		got := backupVerifyDrift(manifestOf("t", "vol-a", "vol-old"), []string{"vol-a"})
		require.Len(t, got, 1)

		assert.Equal(t, "vol-old", got[0].Volume)
		assert.Equal(t, backupVerifyGone, got[0].Status)
	})

	t.Run("drift in both directions at once", func(t *testing.T) {
		got := backupVerifyDrift(manifestOf("t", "vol-old"), []string{"vol-new"})

		assert.Len(t, got, 2)
	})
}

func TestPrintBackupVerify(t *testing.T) {
	m := manifestOf("2026-08-13T17:00:00Z", "vol-data")
	results := []backupVerifyResult{
		{Volume: "vol-data", Status: backupVerifyOK},
		{Volume: "vol-new", Status: backupVerifyUncovered},
	}

	t.Run("table", func(t *testing.T) {
		out := &bytes.Buffer{}

		err := printBackupVerify(out, "table", m, results)
		require.NoError(t, err)

		assert.Contains(t, out.String(), "VOLUME")
		assert.Contains(t, out.String(), "STATUS")
		assert.Contains(t, out.String(), backupVerifyUncovered)
	})

	t.Run("json carries the timestamp it verified", func(t *testing.T) {
		out := &bytes.Buffer{}

		err := printBackupVerify(out, "json", m, results)
		require.NoError(t, err)

		got := backupVerifyReport{}
		err = json.Unmarshal(out.Bytes(), &got)
		require.NoError(t, err)

		assert.Equal(t, "2026-08-13T17:00:00Z", got.Timestamp)
		assert.Len(t, got.Volumes, 2)
	})

	t.Run("an unknown format is an error", func(t *testing.T) {
		err := printBackupVerify(&bytes.Buffer{}, "toml", m, results)

		require.Error(t, err)
	})
}

func TestSplitBackupArgs(t *testing.T) {
	manifests := newestFirst()

	t.Run("a leading timestamp is one", func(t *testing.T) {
		ts, services := splitBackupArgs(manifests, []string{"2026-08-13T17:00:00Z", "web"})

		assert.Equal(t, "2026-08-13T17:00:00Z", ts)
		assert.Equal(t, []string{"web"}, services)
	})

	t.Run("a service that no run is named after stays a service", func(t *testing.T) {
		ts, services := splitBackupArgs(manifests, []string{"web", "db"})

		assert.Empty(t, ts)
		assert.Equal(t, []string{"web", "db"}, services)
	})

	t.Run("no arguments", func(t *testing.T) {
		ts, services := splitBackupArgs(manifests, nil)

		assert.Empty(t, ts)
		assert.Empty(t, services)
	})
}
