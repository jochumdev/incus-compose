package dns

import (
	"encoding/json"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
)

// coldVersion is the on-disk format. A file written by any other version is
// ignored rather than migrated: this is a cache.
const coldVersion = 1

// coldFile is what the store is called under the data directory.
const coldFile = "cold.json"

// coldState is the on-disk form. Explicit types with explicit tags, because the
// file outlives the process that wrote it and a Go rename must not reach it.
//
// Serials are the whole of it. Records come back from the enricher's own store,
// in one read; a serial comes back from nowhere, and a secondary reading one
// going backwards re-transfers on every restart of this process.
type coldState struct {
	Version int `json:"version"`

	Serials map[string]zoneSerial `json:"serials"`
}

type coldStore struct {
	logger *slog.Logger

	// path is empty when no data directory was configured, which disables every
	// method here unless mem is on.
	path string

	// mem keeps the state instead of a file, for a caller with nowhere to put
	// one. Written by this store's own goroutine and read by whoever asks, so
	// held is what guards it.
	mem  bool
	held []byte
	mu   sync.Mutex

	// writes carries encodings to the writer. One slot, and a queued encoding
	// is discarded rather than waited on: a newer one says everything it said.
	writes chan []byte
}

// newColdStore prepares the store under dir. An empty dir disables it, and so
// does one that cannot be created: the plugin serves either way.
func newColdStore(logger *slog.Logger, dir string) *coldStore {
	if dir == "" {
		return &coldStore{}
	}

	err := os.MkdirAll(dir, 0o700)
	if err != nil {
		logger.Warn("creating the data directory, continuing without a cold store",
			"dir", dir, "err", err)

		return &coldStore{}
	}

	return &coldStore{
		path:   filepath.Join(dir, coldFile),
		writes: make(chan []byte, 1),
	}
}

// newMemoryStore keeps what is published in memory rather than under a
// directory, seeded with what a previous run left.
func newMemoryStore(seed []byte) *coldStore {
	return &coldStore{
		mem:    true,
		held:   seed,
		writes: make(chan []byte, 1),
	}
}

// enabled reports whether there is anywhere to store.
func (c *coldStore) enabled() bool { return c.path != "" || c.mem }

// run writes what it is handed until the channel closes. The fold goroutine
// closes it, so the last state encoded on the way out is written, not dropped.
func (c *coldStore) run() {
	for b := range c.writes {
		c.write(b)
	}
}

// store queues an encoding, replacing whatever was waiting. Never blocks: the
// fold goroutine is the only sender, so the slot it emptied is still empty.
func (c *coldStore) store(b []byte) {
	if !c.enabled() || b == nil {
		return
	}

	select {
	case <-c.writes:
	default:
	}

	c.writes <- b
}

// close stops the writer once it has drained.
func (c *coldStore) close() {
	if !c.enabled() {
		return
	}

	close(c.writes)
}

// write replaces the file atomically, so a process starting up reads either the
// whole previous state or the whole new one.
func (c *coldStore) write(b []byte) {
	if c.mem {
		c.mu.Lock()
		c.held = b
		c.mu.Unlock()

		return
	}

	f, err := os.CreateTemp(filepath.Dir(c.path), ".cold-*")
	if err != nil {
		c.logger.Warn("writing the cold store", "err", err)

		return
	}

	tmp := f.Name()

	// Every failure below leaves the previous file where it is: a stale cache
	// costs one read, no cache costs every serial.
	discard := func(err error) {
		_ = os.Remove(tmp)

		c.logger.Warn("writing the cold store", "err", err)
	}

	_, err = f.Write(b)
	if err != nil {
		_ = f.Close()

		discard(err)

		return
	}

	err = f.Close()
	if err != nil {
		discard(err)

		return
	}

	err = os.Rename(tmp, c.path)
	if err != nil {
		discard(err)
	}
}

// load reads what was published last. No usable file is not an error: a first
// start, a wiped directory and another version all mean start with none.
func (c *coldStore) load() map[string]zoneSerial {
	if !c.enabled() {
		return nil
	}

	if c.mem {
		c.mu.Lock()
		held := c.held
		c.mu.Unlock()

		serials, err := decodeCold(held)
		if err != nil {
			return nil
		}

		return serials
	}

	b, err := os.ReadFile(c.path)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			c.logger.Warn("reading the cold store, starting cold", "path", c.path, "err", err)
		}

		return nil
	}

	serials, err := decodeCold(b)
	if err != nil {
		c.logger.Warn("reading the cold store, starting cold", "path", c.path, "err", err)

		return nil
	}

	return serials
}

// encodeCold renders the serials each zone was last published under.
func encodeCold(serials map[string]zoneSerial) ([]byte, error) {
	return json.Marshal(coldState{Version: coldVersion, Serials: serials})
}

// decodeCold reads a store back. A file of another version is not an error and
// not a migration either: it is ignored, and the fleet is read again.
func decodeCold(b []byte) (map[string]zoneSerial, error) {
	var state coldState

	err := json.Unmarshal(b, &state)
	if err != nil {
		return nil, err
	}

	if state.Version != coldVersion {
		return nil, nil
	}

	return state.Serials, nil
}
