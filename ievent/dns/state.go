package dns

import (
	iradix "github.com/hashicorp/go-immutable-radix/v2"
	"github.com/koji-hirono/go-critbit"

	"github.com/lxc/incus-compose/ievent/dns/ecs_view"
	"github.com/lxc/incus-compose/ievent/iutil"
)

// state is what folding owns. Run's goroutine alone, so nothing here locks.
type state struct {
	// held is every instance the trees were written from, keyed by project and
	// name. What is in them, not what has arrived.
	held map[string]*instance

	// pending is what has arrived since the last write, nil where the instance
	// went. Written once per window, so an instance that moved and moved back
	// inside one reaches the trees not at all.
	pending map[string]*instance

	// nets is which views are over a wire, and what keeps each alive - the one
	// thing the trees cannot answer, since a wire is not a name.
	nets map[string]map[ecs_view.ViewID]int

	// serials is what each zone was published under.
	serials map[string]zoneSerial

	// names and zones are the working trees. A Tree.Insert is its own Txn and
	// Commit, so it copies every node on the path and allocates a channel for
	// each; one transaction per window reuses them instead.
	names *iradix.Txn[ecs_view.ViewAnswer]
	zones *iradix.Txn[*ecs_view.Zone]

	// v4 and v6 place a querier. They mutate in place, so publish hands over a
	// copy rather than these.
	v4, v6 critbit.Tree[ecs_view.ViewID]

	// chain is what the last event carried.
	chain iutil.ChainState
}

// newState builds the working copy a fold starts from, with the serials a
// previous run published under.
func newState(serials map[string]zoneSerial) *state {
	// A cold store that is disabled or empty loads nothing, and a zone entering
	// is a write: defaulted here rather than guarded at every one.
	if serials == nil {
		serials = map[string]zoneSerial{}
	}

	return &state{
		held:    map[string]*instance{},
		pending: map[string]*instance{},
		nets:    map[string]map[ecs_view.ViewID]int{},
		serials: serials,
		names:   iradix.New[ecs_view.ViewAnswer]().Txn(),
		zones:   iradix.New[*ecs_view.Zone]().Txn(),
	}
}

// write applies everything that has arrived, taking each instance from what the
// trees hold to what the last event about it said. An instance that moved and
// moved back inside one window compares equal here and reaches them not at all.
func (s *state) write(ttl uint32) bool {
	moved := false

	for key, next := range s.pending {
		delete(s.pending, key)

		prev := s.held[key]

		if next != nil && prev != nil && next.Equal(prev.Event) {
			continue
		}

		if next == nil && prev == nil {
			continue
		}

		s.apply(key, next, ttl)

		moved = true
	}

	return moved
}

// snapshot freezes the working copy. The radix trees commit by copy; the
// critbits mutate in place, so they are walked into fresh ones.
func (s *state) snapshot() *ecs_view.Snapshot {
	snap := &ecs_view.Snapshot{
		Tree:   s.names.Commit(),
		Denial: s.zones.Commit(),
	}

	for key, view := range s.v4.All() {
		snap.ByIPv4.Set(key, view)
	}

	for key, view := range s.v6.All() {
		snap.ByIPv6.Set(key, view)
	}

	// Committed, so the next patch opens a transaction of its own rather than
	// writing through nodes this snapshot is now holding.
	s.names = snap.Tree.Txn()
	s.zones = snap.Denial.Txn()

	return snap
}

// view is the set of networks an instance queries from, and the anonymous view
// of each of those networks on its own. Both are filed under every network in
// them, since that is how a name reaches the views that can see it.
//
// The views returned are the ones that did not exist before, which every name
// already on those wires still has to be written under.
func (s *state) view(inst *instance) (ecs_view.ViewID, []ecs_view.ViewID) {
	keys := netKeys(inst)

	own := ecs_view.ViewOf(keys)

	var fresh []ecs_view.ViewID

	if s.claim(keys, own) {
		fresh = append(fresh, own)
	}

	// A network is a view of its own, for a querier sitting on it that no
	// instance claims.
	for _, key := range keys {
		anon := ecs_view.ViewOf([]string{key})
		if s.claim([]string{key}, anon) && anon != own {
			fresh = append(fresh, anon)
		}
	}

	return own, fresh
}

// claim counts one view against every network in it, reporting whether it is
// one nothing had claimed yet.
func (s *state) claim(keys []string, id ecs_view.ViewID) bool {
	first := false

	for _, key := range keys {
		over := s.nets[key]
		if over == nil {
			over = map[ecs_view.ViewID]int{}
			s.nets[key] = over
		}

		if over[id] == 0 {
			first = true
		}

		over[id]++
	}

	return first
}

// release drops one instance's claim, and reports the views nothing is left in
// along with every network left with no view over it at all - the last instance
// leaving is what takes a view off the names on it, and its prefixes out of the
// address trees.
func (s *state) release(inst *instance) (freed []string, gone []ecs_view.ViewID) {
	keys := netKeys(inst)

	own := ecs_view.ViewOf(keys)

	for _, key := range keys {
		if s.drop(key, own) {
			gone = append(gone, own)
		}

		anon := ecs_view.ViewOf([]string{key})
		if s.drop(key, anon) && anon != own {
			gone = append(gone, anon)
		}

		if len(s.nets[key]) == 0 {
			delete(s.nets, key)

			freed = append(freed, key)
		}
	}

	return freed, gone
}

// drop decrements one view over one network, reporting whether that was the
// last claim on it anywhere.
func (s *state) drop(key string, id ecs_view.ViewID) bool {
	over := s.nets[key]
	if over == nil {
		return false
	}

	over[id]--
	if over[id] > 0 {
		return false
	}

	delete(over, id)

	return true
}

// views is every view that can see one network, which is what a name on it has
// to be written under.
func (s *state) views(key string) []ecs_view.ViewID {
	out := make([]ecs_view.ViewID, 0, len(s.nets[key]))
	for id := range s.nets[key] {
		out = append(out, id)
	}

	return out
}
