package ecs_view

import (
	"net/netip"
	"slices"

	"github.com/miekg/dns"
)

// Result says what a lookup found, so one place decides how to answer.
type Result int

const (
	// Success means the name exists and has records visible to the querier.
	Success Result = iota
	// NameError means the name does not exist in the zone, or exists but is
	// invisible to this querier. The two are deliberately indistinguishable.
	NameError
	// NoData means the name exists and is visible but has nothing of the type
	// that was asked for.
	NoData
)

// Answer is the outcome of a lookup. RRs belongs to the snapshot and is iutil
// with every other query reading the same view, so nothing may write to it.
type Answer struct {
	RRs    []dns.RR
	Result Result
}

// ViewFor resolves a client address to the view it may see. One that belongs to
// no host but falls inside known networks is an anonymous member of all of them.
func (s *Snapshot) ViewFor(addr netip.Addr) (ViewID, bool) {
	id, ok := s.ByAddr[addr]
	if ok {
		// Claimed twice, so which host is asking cannot be known.
		if id == AmbiguousView {
			return "", false
		}

		return id, true
	}

	keys := s.LookupNet(addr)
	if len(keys) == 0 {
		return "", false
	}

	return ViewOf(keys), true
}

// Resolve answers qname of type qtype for one view: three map lookups, because
// the filtering and the rendering happened when the snapshot was built.
//
// An unmaterialized view and one holding nothing both read as NameError.
func (s *Snapshot) Resolve(qname string, qtype uint16, id ViewID) Answer {
	names, materialized := s.Views[id]
	if !materialized {
		return Answer{Result: NameError}
	}

	sets, visible := names[qname]
	if !visible {
		// Invisible reads exactly like missing.
		return Answer{Result: NameError}
	}

	rrs, ok := sets[qtype]
	if !ok {
		// The name is here, this type is not.
		return Answer{Result: NoData}
	}

	return Answer{RRs: rrs, Result: Success}
}

// Gather collects one name's records as the given networks see them. A name on
// exactly one hands its records over by reference; only one on several joins,
// and that happens once when the snapshot is built.
func Gather(perNet map[string]RRSets, keys []string) (RRSets, bool) {
	var (
		only  RRSets
		count int
	)

	for _, key := range keys {
		sets, iutil := perNet[key]
		if !iutil {
			continue
		}

		only = sets
		count++
	}

	switch count {
	case 0:
		return nil, false
	case 1:
		return only, true
	}

	joined := RRSets{}

	for _, key := range keys {
		for qtype, rrs := range perNet[key] {
			for _, rr := range rrs {
				// A CNAME does not vary by network, so it is one value under
				// several keys and joining blindly would repeat it. Identity
				// rather than equality, that being the sharing undone.
				if slices.Contains(joined[qtype], rr) {
					continue
				}

				joined[qtype] = append(joined[qtype], rr)
			}
		}
	}

	return joined, true
}
