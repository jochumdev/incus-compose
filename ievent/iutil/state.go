package iutil

// State is what has happened to an event so far. The set is closed and the
// source owns it; anything a plugin wants to carry of its own goes in WithValue.
type State string

const (
	// StateOk is an event nothing has finished with.
	StateOk State = "ok"

	// StateDropped is an event a plugin has finished with, still walking the
	// chain so that the observers behind it can see it.
	StateDropped State = "dropped"

	// StateFailed is an event the source could not complete.
	StateFailed State = "failed"
)
