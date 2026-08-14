package shared

import "maps"

// Running says Incus reported the instance running when it was read. Only
// meaningful with EnrichedInstance, and a fact rather than a verdict: what a
// running instance with no addresses yet means is the consumer's to decide.
func (e *Event) Running() bool { return e.running }

// Metadata returns one of the instance's configuration keys, by its whole name,
// as Incus returned it. Which keys mean something is the caller's.
func (e *Event) Metadata(key string) (string, bool) {
	v, ok := e.meta[key]

	return v, ok
}

// Metadatas returns the whole configuration, as a clone.
func (e *Event) Metadatas() map[string]string {
	return maps.Clone(e.meta)
}

// WithInstance derives an event carrying what one instance read found. Plain
// values rather than an incusapi.Instance, so this package stays free of the
// Incus module.
//
// It takes ownership of meta and nets - do not keep or reuse them.
func (e *Event) WithInstance(running bool, meta map[string]string, nets map[string]*Network) *Event {
	next := *e
	next.running = running
	next.meta = meta
	next.nets = nets
	next.enriched |= EnrichedInstance | EnrichedNetworks

	return &next
}
