package shared

import (
	"context"

	"github.com/lxc/incus-compose/iclient"
)

// Command is one thing said between the source and a plugin. A struct rather
// than a bare string so a field can be added without every signature changing.
type Command struct {
	// Action is what is being said. Ours, so it carries a slash.
	Action string
}

// SetupArgs is everything a plugin is handed at Setup. An argument bundle, not
// state: a plugin that needs Context past Setup copies it to a field of its own.
type SetupArgs struct {
	// Context is the process lifetime and bounds the daemon reads a plugin
	// makes. It is not how a plugin is told to finish - CommandDrain is - so
	// canceling it is an abort rather than a shutdown.
	Context context.Context

	// Conn is the Incus connection, handed to every plugin and used by the ones
	// that read or write.
	Conn *iclient.Connection

	// Next is the successor's Handle, the one field that differs per position.
	Next Next

	// CommandIn is the source asking this plugin something, on its own channel
	// so it arrives whatever the event inbox looks like. A plugin answers by
	// sending the same Action back on CommandOut, including for commands it does
	// not know.
	CommandIn <-chan Command

	// CommandOut is this plugin telling the chain something. The source mints an
	// event from it and puts it in at the head, so it reaches every position and
	// in order against the events that caused it.
	CommandOut chan<- Command

	// Wanted is the union of every plugin's Wants, keyed by action, built before
	// any Setup runs and not written after. Every plugin holds this same map.
	Wanted map[string]Want
}
