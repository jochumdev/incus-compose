// Package shared is the vocabulary the source and its plugins both speak: the
// event, what can be read onto it, and the interface a plugin implements.
//
// It knows the Incus API types and nothing that talks to Incus.
package shared

import "time"

// Event is what travels the chain.
//
// It is immutable by construction: every field is unexported and no accessor
// hands back anything a caller can write through, so it is safe to share and to
// hold past the walk that delivered it. It carries no context - deadlines belong
// to the read, where the source sets them.
type Event struct {
	action  string
	project string
	name    string
	oldName string

	state  State
	reason string

	// at is when the event was decoded, not when a plugin saw it.
	at time.Time

	// enriched says which reads have landed, so "no networks" can be told from
	// "nobody asked for networks". See Enriched.
	enriched Enrichment

	// instance: filled by WithInstance. See event_instance.go.
	running bool
	meta    map[string]string

	// networks: filled by WithNetworks, keyed by Network.Key. See
	// event_network.go.
	nets map[string]*Network

	// project: filled by WithProject. See event_project.go.
	projectMeta map[string]string

	// values is plugin-scoped data, held as context holds it: a chain of nodes
	// written once, so deriving costs one node rather than a copy of a map.
	values *valueNode
}

// NewEvent builds one event, in StateOk. The only thing that sets a state.
func NewEvent(at time.Time, action, project, name, oldName string) *Event {
	return &Event{
		at:      at,
		action:  action,
		project: project,
		name:    name,
		oldName: oldName,
		state:   StateOk,
	}
}

// At is when the event was decoded off the stream, which is ahead of the walk.
func (e *Event) At() time.Time { return e.at }

// Action is the Incus lifecycle action, its own string rather than a vocabulary
// of ours, so classifying it stays the caller's business.
func (e *Event) Action() string { return e.action }

// Project is the event's project, taken from the envelope rather than the
// payload, which project and profile events leave empty.
func (e *Event) Project() string { return e.project }

// Name is what the action names.
func (e *Event) Name() string { return e.name }

// OldName is the pre-rename name, empty unless this is a rename. Only a caller
// knows which actions rename anything.
func (e *Event) OldName() string { return e.oldName }

// State is what has happened to this event so far. An event that is not StateOk
// is walking the chain for the observers rather than for action.
func (e *Event) State() State { return e.state }

// Reason describes the current state: which plugin dropped the event, or what
// the source could not complete. Empty while StateOk.
func (e *Event) Reason() string { return e.reason }

// WithDropped marks the event finished with, naming who did it: a plugin's
// Name, or "source/<cause>". A no-op once the event is dropped or failed.
func (e *Event) WithDropped(by string) *Event {
	if e.state != StateOk {
		return e
	}

	next := *e
	next.state, next.reason = StateDropped, by

	return &next
}

// WithFailed marks the event as one the source could not complete. It outranks
// dropped; a second failure keeps the first.
func (e *Event) WithFailed(reason string) *Event {
	if e.state == StateFailed {
		return e
	}

	next := *e
	next.state, next.reason = StateFailed, reason

	return &next
}

// Value returns what WithValue stored under key, or nil.
//
// A key must be an unexported type in the plugin that owns it, and the value
// asserted back in that same package. No type can carry that rule, which is why
// it is written here.
func (e *Event) Value(key any) any {
	for v := e.values; v != nil; v = v.parent {
		if v.key == key {
			return v.val
		}
	}

	return nil
}

// WithValue derives an event carrying one more value.
func (e *Event) WithValue(key, val any) *Event {
	next := *e
	next.values = &valueNode{parent: e.values, key: key, val: val}

	return &next
}

// valueNode is one link in the chain WithValue builds.
type valueNode struct {
	parent *valueNode
	key    any
	val    any
}
