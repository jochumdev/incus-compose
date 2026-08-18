package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lxc/incus-compose/client"
)

func TestPrintBackupList(t *testing.T) {
	cfg := client.BackupConfig{Pool: "default"}

	manifests := []backupManifest{
		{
			Timestamp: "2026-08-13T16:18:38.272605835Z",
			Name:      "first",
			Volumes:   []client.BackupVolume{{Source: client.VolumeInfos{Name: "vol-data"}}},
			Size:      5 * 1024 * 1024,
		},
		{
			Timestamp: "2026-08-13T17:00:00.000000000Z",
			Volumes:   []client.BackupVolume{{Source: client.VolumeInfos{Name: "vol-data"}}},
		},
	}

	t.Run("table", func(t *testing.T) {
		out := &bytes.Buffer{}

		err := printBackupList(out, "table", manifests, cfg)
		require.NoError(t, err)

		assert.Contains(t, out.String(), "TIMESTAMP")
		assert.Contains(t, out.String(), "NAME")
		assert.Contains(t, out.String(), "VOLUMES")
		assert.Contains(t, out.String(), "POOL")
		assert.Contains(t, out.String(), "SIZE")
		assert.Contains(t, out.String(), "5.2MB")
		assert.Contains(t, out.String(), "first")
		assert.Contains(t, out.String(), "---", "an unnamed backup must render a placeholder name")
		assert.Contains(t, out.String(), "default")
	})

	t.Run("json keeps the empty name", func(t *testing.T) {
		out := &bytes.Buffer{}

		err := printBackupList(out, "json", manifests, cfg)
		require.NoError(t, err)

		var got []backupManifest
		err = json.Unmarshal(out.Bytes(), &got)
		require.NoError(t, err)
		require.Len(t, got, 2)

		assert.Equal(t, "", got[1].Name, "the placeholder must not leak into the data")
	})

	t.Run("empty table shows only the header", func(t *testing.T) {
		out := &bytes.Buffer{}

		err := printBackupList(out, "table", nil, cfg)
		require.NoError(t, err)

		// Tabwriter pads cells with spaces, so the header is not tab-separated.
		assert.Equal(t, "TIMESTAMP  NAME  VOLUMES  POOL  SIZE\n", out.String())
	})

	t.Run("empty json is an empty list", func(t *testing.T) {
		out := &bytes.Buffer{}

		err := printBackupList(out, "json", nil, cfg)
		require.NoError(t, err)

		assert.JSONEq(t, "[]", out.String())
	})

	t.Run("empty yaml is an empty list", func(t *testing.T) {
		out := &bytes.Buffer{}

		err := printBackupList(out, "yaml", nil, cfg)
		require.NoError(t, err)

		assert.Equal(t, "[]\n", out.String())
	})
}

func TestSortBackupList(t *testing.T) {
	manifests := []backupManifest{
		{Timestamp: "2026-08-13T16:00:00.000000000Z", Name: "oldest"},
		{Timestamp: "2026-08-13T18:00:00.000000000Z", Name: "newest"},
		{Timestamp: "2026-08-13T17:00:00.000000000Z", Name: "middle"},
		{Timestamp: "not-a-timestamp", Name: "malformed"},
	}

	sortBackupList(manifests)

	names := make([]string, 0, len(manifests))
	for _, m := range manifests {
		names = append(names, m.Name)
	}

	assert.Equal(t, []string{"newest", "middle", "oldest", "malformed"}, names,
		"must sort newest first, malformed timestamps last")

	assert.True(t, strings.Contains(manifests[3].Timestamp, "not-a-timestamp"),
		"malformed timestamp must keep its raw value")
}
