package dns

import (
	"encoding/json"
	"errors"
	"io/fs"
	"log/slog"
	"net/netip"
	"os"
	"path/filepath"

	"github.com/lxc/incus-compose/ievent/dns/ecs_view"
	"github.com/lxc/incus-compose/ievent/shared"
)

// coldVersion is the on-disk format. A file written by any other version is
// ignored rather than migrated: this is a cache, and starting without it costs
// one whole-fleet read.
const coldVersion = 1

// coldFile is what the store is called under the data directory.
const coldFile = "cold.json"

// coldState is the on-disk form. Explicit types with explicit tags, because the
// file outlives the process that wrote it and a Go rename must not reach it.
type coldState struct {
	Version int `json:"version"`

	// Serials is what a restart is really for: a secondary reads one going
	// backwards as a rebuild and re-transfers, every restart, for ever.
	Serials map[string]uint32 `json:"serials"`

	// Instances is what was served, keyed by project and name, in the distilled
	// form rather than the events it came from.
	Instances map[string]coldInstance `json:"instances,omitempty"`
}

// coldInstance is one instance as the fold held it.
type coldInstance struct {
	Zone string            `json:"zone"`
	Meta map[string]string `json:"meta,omitempty"`

	// Project is the project's own labels, apart from Meta for the reason
	// distill keeps them apart. A file written without them reads as a project
	// never read, which is what it is.
	Project map[string]string `json:"project,omitempty"`

	// Nets is every network it sits on, keyed the way the snapshot keys them:
	// by the project that owns the network, never the one looking at it.
	Nets map[string]coldNetwork `json:"nets,omitempty"`
}

// coldNetwork is one wire and the addresses this instance holds on it, together
// for the same reason shared.Network joins them.
type coldNetwork struct {
	Name     string         `json:"name"`
	Project  string         `json:"project"`
	Managed  bool           `json:"managed"`
	Prefixes []netip.Prefix `json:"prefixes,omitempty"`

	// netip.Addr marshals as text, so these stay readable in the file.
	IPv4 []netip.Addr `json:"v4,omitempty"`
	IPv6 []netip.Addr `json:"v6,omitempty"`
}

// coldStore persists what is served, so a restart answers before it has reached
// Incus and carries its zone serials across.
//
// Encoding happens on the fold goroutine and writing on its own. What crosses
// is a finished []byte, so the writer cannot reach into live state.
type coldStore struct {
	// path is empty when no data directory was configured, which disables every
	// method here.
	path string

	// writes carries encodings to the writer. One slot, and a queued encoding
	// is discarded rather than waited on: a newer one says everything it said.
	writes chan []byte
}

// newColdStore prepares the store under dir. An empty dir disables it, and so
// does one that cannot be created: the plugin serves either way.
func newColdStore(dir string) *coldStore {
	if dir == "" {
		return &coldStore{}
	}

	err := os.MkdirAll(dir, 0o700)
	if err != nil {
		slog.Warn("creating the data directory, continuing without a cold store",
			"dir", dir, "err", err)

		return &coldStore{}
	}

	return &coldStore{
		path:   filepath.Join(dir, coldFile),
		writes: make(chan []byte, 1),
	}
}

// enabled reports whether there is anywhere to store.
func (c *coldStore) enabled() bool { return c.path != "" }

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
	f, err := os.CreateTemp(filepath.Dir(c.path), ".cold-*")
	if err != nil {
		slog.Warn("writing the cold store", "err", err)

		return
	}

	tmp := f.Name()

	// Every failure below leaves the previous file where it is: a stale cache
	// costs one read, no cache costs every serial.
	discard := func(err error) {
		_ = os.Remove(tmp)

		slog.Warn("writing the cold store", "err", err)
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

// load reads what was served last. No usable file is not an error: a first
// start, a wiped directory and another version all mean start cold.
func (c *coldStore) load() (map[string]*instance, map[string]uint32) {
	if !c.enabled() {
		return nil, nil
	}

	b, err := os.ReadFile(c.path)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			slog.Warn("reading the cold store, starting cold", "path", c.path, "err", err)
		}

		return nil, nil
	}

	held, serials, err := decodeCold(b)
	if err != nil {
		slog.Warn("reading the cold store, starting cold", "path", c.path, "err", err)

		return nil, nil
	}

	return held, serials
}

// encodeCold renders what is held, with the serials snap was published under.
func encodeCold(held map[string]*instance, snap *ecs_view.Snapshot) ([]byte, error) {
	state := coldState{
		Version:   coldVersion,
		Serials:   make(map[string]uint32, len(snap.ByZone)),
		Instances: make(map[string]coldInstance, len(held)),
	}

	for name, zone := range snap.ByZone {
		state.Serials[name] = zone.Serial
	}

	for key, inst := range held {
		one := coldInstance{
			Zone:    inst.zone,
			Meta:    inst.meta,
			Project: inst.project,
			Nets:    make(map[string]coldNetwork, len(inst.nets)),
		}

		for netKey, net := range inst.nets {
			one.Nets[netKey] = coldNetwork{
				Name:     net.Name(),
				Project:  net.Project(),
				Managed:  net.Managed(),
				Prefixes: net.Prefixes(),
				IPv4:     net.IPv4(),
				IPv6:     net.IPv6(),
			}
		}

		state.Instances[key] = one
	}

	// Marshaling a map sorts its keys, so two runs holding the same fleet write
	// the same bytes and the file can be diffed.
	return json.Marshal(state)
}

// decodeCold turns a file back into what the plugin folds.
func decodeCold(b []byte) (map[string]*instance, map[string]uint32, error) {
	var state coldState

	err := json.Unmarshal(b, &state)
	if err != nil {
		return nil, nil, err
	}

	if state.Version != coldVersion {
		return nil, nil, errors.New("unknown cold store version")
	}

	held := make(map[string]*instance, len(state.Instances))

	for key, one := range state.Instances {
		inst := &instance{
			zone:     one.Zone,
			meta:     one.Meta,
			project:  one.Project,
			transfer: transferable(one.Project),
			nets:     make(map[string]*shared.Network, len(one.Nets)),
		}

		for netKey, n := range one.Nets {
			inst.nets[netKey] = shared.NewNetwork(
				n.Name, n.Project, n.Managed, n.Prefixes, n.IPv4, n.IPv6)
		}

		held[key] = inst
	}

	return held, state.Serials, nil
}
