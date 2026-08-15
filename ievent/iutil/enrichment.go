package iutil

import (
	"fmt"
	"strings"
)

// Enrichment names a read the source can perform for a plugin.
//
// A set rather than one flag, because "no networks on this event" and "nobody
// asked for networks" are different answers, and a plugin acting on the first
// when it has the second publishes an absence that is not real.
type Enrichment uint8

const (
	// EnrichedInstance says GetInstance and GetInstanceState have landed:
	// Running and Metadata mean something. See event_instance.go.
	EnrichedInstance Enrichment = 1 << iota

	// EnrichedNetworks says the networks this instance sits on have landed,
	// with the addresses it holds on each. See event_network.go.
	EnrichedNetworks

	// EnrichedProject says the project's own labels have landed, read off its
	// own configuration and never its default profile. See event_project.go.
	EnrichedProject
)

// Enriched reports whether every kind in want has landed on this event.
//
//	if !ev.Enriched(iutil.EnrichedInstance | iutil.EnrichedNetworks) {
//		...
//	}
func (e *Event) Enriched(want Enrichment) bool {
	return e.enriched&want == want
}

// Enrichments is the whole set that landed, for an observer with nothing
// particular to ask. Ask Enriched when the question is whether a kind is there:
// this is a set, so comparing it for equality changes meaning as kinds are added.
func (e *Event) Enrichments() Enrichment { return e.enriched }

// String names the kinds in the set. Whatever is left after the named bits is
// printed as a number, so an unnamed kind shows up rather than vanishing.
func (e Enrichment) String() string {
	named := []struct {
		bit  Enrichment
		name string
	}{
		{EnrichedInstance, "instance"},
		{EnrichedNetworks, "networks"},
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
