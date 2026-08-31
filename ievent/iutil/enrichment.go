package iutil

import (
	"fmt"
	"strings"
)

// Enrichment names a read the source can perform for a plugin, as a bitset: an absent read and
// an unrequested one are different answers, so no single flag can carry it.
type Enrichment uint8

const (
	// EnrichedInstance says GetInstance and GetInstanceState landed: Running and Metadata mean something.
	EnrichedInstance Enrichment = 1 << iota

	EnrichedInstanceWithInterfaces

	// EnrichedNetwork says the networks this instance sits on, and its addresses on each, landed.
	EnrichedNetwork

	// EnrichedProject says the project's own labels landed, read off its configuration, never its default profile.
	EnrichedProject
)

// Enriched reports whether every kind in want has landed on this event.
//
//	if !ev.Enriched(iutil.EnrichedInstance | iutil.EnrichedInstanceWithInterfaces) {
//		...
//	}
func (e *Event) Enriched(want Enrichment) bool {
	return e.enriched&want == want
}

// Enrichments is the whole set that landed, for an observer with nothing particular to ask; ask
// Enriched instead when the question is whether one kind is there.
func (e *Event) Enrichments() Enrichment { return e.enriched }

// String names the kinds in the set. Whatever is left after the named bits is
// printed as a number, so an unnamed kind shows up rather than vanishing.
func (e Enrichment) String() string {
	named := []struct {
		bit  Enrichment
		name string
	}{
		{EnrichedInstance, "instance"},
		{EnrichedInstanceWithInterfaces, "instance-with-interfaces"},
		{EnrichedNetwork, "network"},
		{EnrichedProject, "project"},
	}

	var out []string

	rest := e

	for _, n := range named {
		if e&n.bit == 0 {
			continue
		}

		out = append(out, n.name)
		rest &^= n.bit
	}

	if rest != 0 {
		out = append(out, fmt.Sprintf("%#x", uint8(rest)))
	}

	if len(out) == 0 {
		return "none"
	}

	return strings.Join(out, ",")
}
