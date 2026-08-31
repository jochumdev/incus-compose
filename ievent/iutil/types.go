package iutil

import (
	"context"

	"github.com/lxc/incus-compose/iclient"
)

// DefaultProject is where a bridge lives unless a project owns its own.
const DefaultProject = "default"

// FeaturesNetworks is the project config key that gives a project networks of
// its own, rather than referencing the default project's.
const FeaturesNetworks = "features.networks"

// Actions of ours: the slash prefixes who raised it, and an Incus action never contains one.
const (
	// ActionSweepEnd says the enricher has been all the way round the fleet, not what exists.
	ActionSweepEnd = "enricher/sweep-end"

	// ActionConnected and ActionDisconnected report the event stream itself, with no project or name.
	ActionConnected    = "source/connected"
	ActionDisconnected = "source/disconnected"

	// CommandDrain asks a plugin to finish: hand on everything it holds, and answer on CommandOut when empty.
	CommandDrain = "source/drain"
)

// ChainState is what the whole chain knows, as Err is what happened to one event. Gate on
// ChainWarm, not against ChainCold: the zero value is neither, so an unstamped event acts like nothing has been read.
type ChainState string

const (
	// ChainCold is a fleet nothing has read whole yet.
	ChainCold ChainState = "cold"

	// ChainWarm is a fleet read whole, stamped by the enricher on the sweep-end
	// it raises.
	ChainWarm ChainState = "warm"
)

// Next hands the event on. The last plugin in the chain is given one that does
// nothing.
type Next func(ev *Event)

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

// Command is one thing said between the source and a plugin. A struct rather
// than a bare string so a field can be added without every signature changing.
type Command struct {
	// Action is what is being said. Ours, so it carries a slash. Empty puts
	// nothing on the chain.
	Action string

	// ChainState is what the chain is from here on. Empty leaves it as it
	// stands. Independent of Action: a plugin sets either, or both.
	ChainState ChainState
}

// SetupArgs is everything a plugin is handed at Setup. An argument bundle, not
// state: a plugin that needs Context past Setup copies it to a field of its own.
type SetupArgs struct {
	// Context is the process lifetime and bounds the daemon reads a plugin makes; canceling it is
	// an abort, not a shutdown - CommandDrain is how a plugin is told to finish.
	Context context.Context

	// Conn is the Incus connection, handed to every plugin and used by the ones
	// that read or write.
	Conn *iclient.Connection

	// Next is the successor's Handle, the one field that differs per position.
	Next Next

	// CommandIn is the source asking this plugin something, on its own channel so it arrives
	// whatever the event inbox looks like. Answer by sending the same Action back on CommandOut,
	// including for commands not recognized.
	CommandIn <-chan Command

	// CommandOut is this plugin telling the chain something: an Action creates an event the source
	// enters at the head, in order against the events that caused it; ChainState sets what it stamps from here on.
	CommandOut chan<- Command

	// Wanted is the union of every plugin's Wants, keyed by action, built before
	// any Setup runs and not written after. Every plugin holds this same map.
	Wanted map[string]Want
}

// Plugin is one link in the chain: it holds its successor and continues the walk itself, so the
// chain runs as a call stack. A plugin appearing twice needs two constructions, not one value listed twice.
type Plugin interface {
	// Name identifies the plugin in logs, in metrics, and in the chain.
	Name() string

	// Wants declares which actions this plugin cares about and how much of each must be read
	// before it sees one; read from every plugin before anything is wired.
	Wants() []Want

	// Setup wires the plugin, once, before anything runs. An error here stops
	// the process.
	Setup(args SetupArgs) error

	// Handle runs in its parent's goroutine and must not block: enqueue and return, then call Next
	// later from the plugin's own goroutine. Check the error first; a non-nil one means observers-only:
	//
	//	func (p *Plugin) Handle(ev *iutil.Event) {
	//		if ev.Err() != nil {
	//			p.next(ev)
	//
	//			return
	//		}
	//		...
	//	}
	Handle(ev *Event)
}
