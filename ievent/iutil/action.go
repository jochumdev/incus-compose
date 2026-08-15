package iutil

// Actions of ours, not Incus's. The slash is what keeps them from colliding: an
// Incus lifecycle action cannot contain one, and the prefix names who raises it.
const (
	// ActionSweepStart opens a whole-project pass and ActionSweepEnd closes it.
	// A name a plugin holds and does not see between them is gone.
	ActionSweepStart = "source/sweep-start"
	ActionSweepEnd   = "source/sweep-end"

	// ActionConnected and ActionDisconnected report the event stream itself.
	// They carry no project and no name.
	ActionConnected    = "source/connected"
	ActionDisconnected = "source/disconnected"

	// ActionReady and ActionNotReady say whether the plugin that raised one has
	// something worth answering from. Edges rather than a level, so whoever
	// folds them starts not-ready.
	ActionReady    = "chain/ready"
	ActionNotReady = "chain/not-ready"

	// CommandDrain asks one plugin to finish: hand on everything it holds, and
	// answer on CommandOut when there is nothing left.
	CommandDrain = "source/drain"
)
