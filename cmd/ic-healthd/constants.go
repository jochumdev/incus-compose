package main

import (
	"time"

	"github.com/lxc/incus-compose/shared"
)

const maxRestartDelay = 5 * time.Minute
const restartTimeout = 5 * time.Minute * 4

// apiTimeout bounds one Incus API call. Every call has to carry it: the
// connection sets no deadline of its own, the context is the only bound.
const apiTimeout = 30 * time.Second

// Worker counts over every watched project. Restarts count apart: one holds its
// worker for up to restartTimeout.
const (
	defaultWorkers        = 128
	defaultRestartWorkers = 32
)

// poolRetryDelay is how long a refused action waits. It also puts the instance
// back in the timer, which a due time in the past would not.
const poolRetryDelay = 250 * time.Millisecond

// A project-updated event is sent before incusd applies the change, so a scope
// read can answer on the old config. These bound how long a project missed that
// way stays unwatched.
const (
	scopeRecheckDelay = 500 * time.Millisecond
	scopeRecheckTries = 6
)

// defaultProjectMarker selects the projects handed to the shared daemon.
const defaultProjectMarker = shared.HealthScopeKey + "=" + shared.HealthScopeGlobal

// Channel depths. The router blocks rather than drops, so these only buy slack
// while a loop is between selects.
const (
	// projectBuffer holds one project's events between the router and its scheduler.
	projectBuffer = 32
	// resultBuffer holds one scheduler's action results.
	resultBuffer = 32
)

// config holds the healthd configuration.
type config struct {
	DataDir    string
	SecretsDir string
	IncusURL   string
	Token      string
	OwnProject string
	OwnName    string
	Projects   []string

	// ProjectMarker and ProjectMarkerValue are the project config key and value
	// that opt a project in when Projects is empty, which ignores both.
	ProjectMarker      string
	ProjectMarkerValue string

	// Workers and RestartWorkers cap the actions in flight across all projects.
	Workers        int
	RestartWorkers int

	id string
}

func (cfg *config) ID() string {
	if cfg.id == "" {
		cfg.id = RandString(12)
	}

	return cfg.id
}

type instanceEventAction string

const (
	instanceRestarted instanceEventAction = "restarted"
	instanceUpdated   instanceEventAction = "updated"
	instanceStopped   instanceEventAction = "stopped"
	instanceDeleted   instanceEventAction = "deleted"

	// instanceResync tells a scheduler to rediscover its whole project. It
	// names no instance: the router sends it after events were missed.
	instanceResync instanceEventAction = "resync"
)

// projectEventAction is a project-scoped lifecycle event. The router acts on
// these itself rather than handing them to a scheduler.
type projectEventAction string

const (
	projectCreated projectEventAction = "created"
	projectUpdated projectEventAction = "updated"
	projectDeleted projectEventAction = "deleted"
	projectRenamed projectEventAction = "renamed"
)

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

// instanceAction is what an idle instance does when it comes due. A check and
// a restart are mutually exclusive: one awaiting a restart is stopped.
type instanceAction string

const (
	instanceActionCheck   instanceAction = "check"
	instanceActionRestart instanceAction = "start"
)

type instanceResultKind string

const (
	instanceResultDiscovered instanceResultKind = "discovered"
	instanceResultRestarted  instanceResultKind = "restarted"
	instanceResultChecked    instanceResultKind = "checked"
	instanceResultStatus     instanceResultKind = "status"

	// instanceResultRoster closes a discovery pass with every instance it saw,
	// so the scheduler can drop the ones that are gone.
	instanceResultRoster instanceResultKind = "roster"
)

// instanceEvent is the runner loop's only input, built the same way by the
// lifecycle listener, the checkers and the timers.
type instanceEvent struct {
	Action   instanceEventAction
	Instance string
}

// lifecycleEvent is one decoded Incus event, the router's only input. Exactly
// one of Instance.Action and ProjectAction is set.
type lifecycleEvent struct {
	// Project is the event's project, which on a rename is the new name.
	Project string

	Instance      instanceEvent
	ProjectAction projectEventAction

	// OldName is the pre-rename name, which projectRenamed carries instead of
	// in Project.
	OldName string
}

// Default healthcheck settings when keys are missing on the instance.
const (
	defaultRestartDelay    = 5 * time.Second
	defaultInterval        = 30 * time.Second
	defaultTimeout         = 30 * time.Second
	defaultRetries         = 3
	defaultRestartPeriod   = 0 * time.Second
	defaultRestartInterval = 5 * time.Second
)
