package dns

import (
	"encoding/json"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lxc/incus-compose/ievent/dns/ecs_view"
	"github.com/lxc/incus-compose/ievent/iutil"
)

// oneInstance is what the fold holds for a single instance: a zone, a label or
// two, and one network with an address on it.
func oneInstance(zone, netKey, addr string) *instance {
	return &instance{
		zone: zone,
		meta: map[string]string{"service": "api"},
		nets: map[string]*iutil.Network{
			netKey: iutil.NewNetwork("net0", "shop", true,
				[]netip.Prefix{netip.MustParsePrefix("10.0.0.0/24")},
				[]netip.Addr{netip.MustParseAddr(addr)}, nil),
		},
	}
}

// snapshotWithSerials is a published snapshot carrying one serial per zone.
func snapshotWithSerials(serials map[string]uint32) *ecs_view.Snapshot {
	snap := ecs_view.EmptySnapshot()

	for name, serial := range serials {
		snap.ByZone[name] = &ecs_view.Zone{
			Names:  map[string]map[string]ecs_view.RRSets{},
			Serial: serial,
		}
	}

	return snap
}

// TestColdStoreRoundTrip pins the whole point of the file: what a restart reads
// back is what the last run published.
func TestColdStoreRoundTrip(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	c := newColdStore(dir)
	require.True(t, c.enabled())

	held := map[string]*instance{
		"shop/web": oneInstance("shop.incus.", "shop/net0", "10.0.0.2"),
		"shop/db":  oneInstance("shop.incus.", "shop/net0", "10.0.0.3"),
	}

	snap := snapshotWithSerials(map[string]uint32{"shop.incus.": 7, "other.incus.": 2})

	b, err := encodeCold(held, snap)
	require.NoError(t, err)

	// Written straight rather than through the queue, so this tests the file
	// and not the scheduling.
	c.write(b)

	got, serials := c.load()
	assert.Equal(t, map[string]uint32{"shop.incus.": 7, "other.incus.": 2}, serials)

	require.Len(t, got, 2)
	require.Contains(t, got, "shop/web")

	// Enough to render the same records again: the zone, the labels that name
	// it, and the wire with the address on it.
	web := got["shop/web"]
	assert.Equal(t, "shop.incus.", web.zone)
	assert.Equal(t, "api", web.meta["service"])

	require.Contains(t, web.nets, "shop/net0")
	assert.Equal(t, []netip.Addr{netip.MustParseAddr("10.0.0.2")}, web.nets["shop/net0"].IPv4())
	assert.Equal(t, []netip.Prefix{netip.MustParsePrefix("10.0.0.0/24")}, web.nets["shop/net0"].Prefixes())
}

// TestColdStoreIsStableAcrossRuns pins that two runs holding the same fleet
// write the same bytes, so the file can be diffed.
func TestColdStoreIsStableAcrossRuns(t *testing.T) {
	t.Parallel()

	held := map[string]*instance{
		"shop/web":   oneInstance("shop.incus.", "shop/net0", "10.0.0.2"),
		"shop/db":    oneInstance("shop.incus.", "shop/net0", "10.0.0.3"),
		"shop/cache": oneInstance("shop.incus.", "shop/net0", "10.0.0.4"),
	}

	snap := snapshotWithSerials(map[string]uint32{"shop.incus.": 1})

	first, err := encodeCold(held, snap)
	require.NoError(t, err)

	for range 10 {
		again, err := encodeCold(held, snap)
		require.NoError(t, err)
		require.Equal(t, string(first), string(again), "map order reached the file")
	}
}

// TestColdStoreStartsCold covers every way there is nothing usable to read,
// none of which is an error.
func TestColdStoreStartsCold(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		setup func(t *testing.T, dir string)
	}{
		{
			name:  "no file at all",
			setup: func(_ *testing.T, _ string) {},
		},
		{
			name: "a file from another version",
			setup: func(t *testing.T, dir string) {
				b, err := json.Marshal(coldState{
					Version: coldVersion + 1,
					Serials: map[string]uint32{"shop.incus.": 9},
				})
				require.NoError(t, err)
				require.NoError(t, os.WriteFile(filepath.Join(dir, coldFile), b, 0o600))
			},
		},
		{
			name: "a file that is not JSON at all",
			setup: func(t *testing.T, dir string) {
				require.NoError(t, os.WriteFile(filepath.Join(dir, coldFile), []byte("{"), 0o600))
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			dir := t.TempDir()
			tc.setup(t, dir)

			held, serials := newColdStore(dir).load()
			assert.Nil(t, held)
			assert.Nil(t, serials)
		})
	}
}

// TestColdStoreDisabled pins that no data directory disables every method
// rather than failing one. Nothing writes to disk unless it was asked to.
func TestColdStoreDisabled(t *testing.T) {
	t.Parallel()

	c := newColdStore("")

	assert.False(t, c.enabled())

	held, serials := c.load()
	assert.Nil(t, held)
	assert.Nil(t, serials)

	// Neither of these has anywhere to go, and neither may panic on the way to
	// finding that out.
	assert.NotPanics(t, func() { c.store([]byte("{}")) })
	assert.NotPanics(t, c.close)
}

// TestColdStoreKeepsThePreviousFileOnAFailedWrite pins the fallback: a stale
// cache costs one read, and no cache costs every serial.
func TestColdStoreKeepsThePreviousFileOnAFailedWrite(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	c := newColdStore(dir)

	good, err := encodeCold(
		map[string]*instance{"shop/web": oneInstance("shop.", "shop/net0", "10.0.0.2")},
		snapshotWithSerials(map[string]uint32{"shop.": 4}))
	require.NoError(t, err)

	c.write(good)

	// A directory that cannot be written to, so CreateTemp fails and the file
	// already there is what a restart still finds.
	require.NoError(t, os.Chmod(dir, 0o500))

	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	c.write([]byte(`{"version":1,"serials":{"shop.":99}}`))

	_, serials := c.load()
	assert.Equal(t, map[string]uint32{"shop.": 4}, serials, "the previous file was lost")
}

// TestColdStoreStoreKeepsTheNewest pins the one-slot queue: a waiting encoding
// is replaced rather than queued, since a newer one says everything it said.
func TestColdStoreStoreKeepsTheNewest(t *testing.T) {
	t.Parallel()

	c := newColdStore(t.TempDir())

	c.store([]byte("first"))
	c.store([]byte("second"))
	c.store([]byte("third"))

	assert.Equal(t, "third", string(<-c.writes))
	assert.Empty(t, c.writes, "an older encoding was still queued behind it")
}

// TestColdStoreWritesWhatItWasHandedLast pins that closing the channel flushes:
// the encoding made on the way out holds the serials, which nothing can re-read.
func TestColdStoreWritesWhatItWasHandedLast(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	c := newColdStore(dir)

	done := make(chan struct{})

	go func() {
		defer close(done)

		c.run()
	}()

	b, err := encodeCold(
		map[string]*instance{"shop/web": oneInstance("shop.", "shop/net0", "10.0.0.2")},
		snapshotWithSerials(map[string]uint32{"shop.": 12}))
	require.NoError(t, err)

	c.store(b)
	c.close()

	<-done

	_, serials := c.load()
	assert.Equal(t, map[string]uint32{"shop.": 12}, serials)
}

// TestColdStoreWriteIsAtomic pins that a reader never sees a half-written file.
// A big payload and a reader in a loop is what tells rename from write-in-place.
func TestColdStoreWriteIsAtomic(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	c := newColdStore(dir)

	// Big enough that a write-in-place is visibly partial for a while.
	held := map[string]*instance{}
	for i := range 5000 {
		held[fmt.Sprintf("shop/instance-with-a-long-enough-name-%d", i)] =
			oneInstance("shop.", "shop/net0", "10.0.0.2")
	}

	first, err := encodeCold(
		map[string]*instance{"shop/web": oneInstance("shop.", "shop/net0", "10.0.0.2")},
		snapshotWithSerials(map[string]uint32{"shop.": 1}))
	require.NoError(t, err)

	second, err := encodeCold(held, snapshotWithSerials(map[string]uint32{"shop.": 2}))
	require.NoError(t, err)

	c.write(first)

	done := make(chan struct{})

	go func() {
		defer close(done)

		for range 50 {
			c.write(second)
			c.write(first)
		}
	}()

	// Every read is one whole state or the other. A torn one decodes to nothing,
	// so a nil serial map here is the failure and not an empty one.
	reads := 0

	for {
		select {
		case <-done:
			assert.Positive(t, reads, "the reader never raced the writer")

			return
		default:
		}

		_, serials := c.load()
		require.NotNil(t, serials, "read a file that was only partly written")

		reads++
	}
}
