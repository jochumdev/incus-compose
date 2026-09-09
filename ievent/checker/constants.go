package checker

import (
	"time"
)

// apiTimeout bounds one Incus API call. Every call has to carry it: the
// connection sets no deadline of its own, the context is the only bound.
const apiTimeout = 30 * time.Second

const maxRestartDelay = 5 * time.Minute
const restartTimeout = 5 * time.Minute * 4

// poolRetryDelay is how long a refused action waits. It also puts the instance
// back in the timer, which a due time in the past would not.
const poolRetryDelay = 250 * time.Millisecond

// instanceState is what an instance is doing right now; exactly one applies.
type instanceState string

const (
	// instanceIdle waits for due; pending says what fires when it arrives.
	instanceIdle instanceState = "idle"
	// instanceChecking has a healthcheck in flight.
	instanceChecking instanceState = "checking"
	// instanceRestarting has a restart in flight.
	instanceRestarting instanceState = "restarting"
	// instanceParked was stopped on purpose and waits for a start event.
	instanceParked instanceState = "parked"
)

type instanceResultKind string

const (
	instanceResultDiscovered instanceResultKind = "discovered"
	instanceResultRestarted  instanceResultKind = "restarted"
	instanceResultChecked    instanceResultKind = "checked"
	instanceResultStatus     instanceResultKind = "status"
)

// instanceAction is what an idle instance does when it comes due. A check and
// a restart are mutually exclusive: one awaiting a restart is stopped.
type instanceAction string

const (
	instanceActionCheck   instanceAction = "check"
	instanceActionRestart instanceAction = "start"
)

// Default healthcheck settings when keys are missing on the instance.
const (
	defaultRestartDelay    = 5 * time.Second
	defaultInterval        = 30 * time.Second
	defaultTimeout         = 30 * time.Second
	defaultRetries         = 3
	defaultRestartPeriod   = 0 * time.Second
	defaultRestartInterval = 5 * time.Second
)
