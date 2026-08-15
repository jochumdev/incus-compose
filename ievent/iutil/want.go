package iutil

// Want is one action a plugin cares about, and how much of it the source should
// read before the walk. Nil is none, which is what an observer returns.
type Want struct {
	// Action is the Incus lifecycle action, its own string.
	Action string

	// Enrich is what to read before delivering, unioned across plugins. Zero
	// means the bare event.
	Enrich Enrichment

	// Debounce says this plugin can live with only the last of a burst on this
	// action. False wins, so the zero value vetoes for everybody.
	Debounce bool
}
