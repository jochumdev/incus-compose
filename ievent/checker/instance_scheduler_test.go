package checker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	incusApi "github.com/lxc/incus/v7/shared/api"

	"github.com/lxc/incus-compose/iclient"
	"github.com/lxc/incus-compose/ievent/iutil"
	"github.com/lxc/incus-compose/shared"
)

// resultGrace bounds how long a result may take to arrive. Every action here
// runs against refusedConn, so the only real work is a rejected TCP connect.
const resultGrace = 5 * time.Second

// absenceGrace is how long "nothing was sent" waits before believing itself.
const absenceGrace = 500 * time.Millisecond

// scheduler is one project's loop state, without the loop.
type scheduler struct {
	ctx       context.Context
	conn      *iclient.Connection
	project   string
	logger    *slog.Logger
	pools     pools
	instances map[string]*instance
	results   chan instanceResult
	metrics   bool
}

func newScheduler(t *testing.T) *scheduler {
	t.Helper()

	return newSchedulerWithPools(t, defaultWorkers, defaultRestartWorkers)
}

// newSchedulerWithPools takes the worker counts, for the tests about what
// happens when there is no free one.
func newSchedulerWithPools(t *testing.T, workers, restartWorkers int) *scheduler {
	t.Helper()

	pools, err := newPools(workers, restartWorkers)
	require.NoError(t, err)

	t.Cleanup(func() { releasePools(pools) })

	return &scheduler{
		ctx:       t.Context(),
		conn:      refusedConn(t),
		project:   "healthd-scheduler",
		logger:    slog.Default(),
		pools:     pools,
		instances: map[string]*instance{},
		results:   make(chan instanceResult, 32),
	}
}

// key is the instances-map key for a name, matching instanceResult.Key.
func (s *scheduler) key(name string) string {
	return s.project + "/" + name
}

// add registers an instance that is idle and due for a check right now.
func (s *scheduler) add(name string, cfg *instanceConfig) *instance {
	inst := &instance{
		name:         name,
		project:      s.project,
		config:       cfg,
		state:        instanceIdle,
		action:       instanceActionCheck,
		due:          time.Now(),
		restartDelay: baseRestartDelay(cfg),
	}

	s.instances[s.key(name)] = inst

	return inst
}

// running marks inst as having an action in flight, the way the scheduler does.
func (s *scheduler) running(t *testing.T, inst *instance, state instanceState, deadline time.Time) {
	t.Helper()

	inst.state = state
	inst.actionDeadline = deadline
	inst.actionContext, inst.actionCancel = context.WithCancel(s.ctx)

	t.Cleanup(inst.actionCancel)
}

func (s *scheduler) run() time.Time {
	return runInstanceActions(s.ctx, s.logger, s.conn, s.pools, s.instances, s.results, s.metrics)
}

func (s *scheduler) result(res instanceResult) {
	handleInstanceResult(s.ctx, s.logger, s.conn, s.instances, s.results, res, s.metrics)
}

func (s *scheduler) event(action string, name string) {
	cfg := map[string]string{"test": `["CMD","true"]`}
	if tracked, ok := s.instances[s.key(name)]; ok {
		cfg = eventConfig(tracked.config)
	}

	s.eventRead(action, name, true, healthKeys(cfg))
}

// eventRead feeds one enriched event the way the enricher delivers it. A
// delete is never enriched in the chain - the enricher short-circuits it, and
// the sweep's roster prunes arrive the same bare way - so it lands bare.
func (s *scheduler) eventRead(action string, name string, running bool, keys map[string]string) {
	if action == incusApi.EventLifecycleInstanceDeleted {
		handleInstanceEvent(s.ctx, s.logger, s.conn, s.instances, s.results,
			iutil.NewEvent(time.Now(), action, s.project, name, ""))

		return
	}

	read := iutil.NewInstance(running, keys, []iutil.InstanceInterface{}, map[string]*iutil.Network{})
	ev := iutil.NewEvent(time.Now(), action, s.project, name, "").WithInstance(read, false)
	handleInstanceEvent(s.ctx, s.logger, s.conn, s.instances, s.results, ev)
}

// eventConfig is the healthcheck keys a parsed config came from, so an event
// carrying them re-parses to the same config.
func eventConfig(cfg *instanceConfig) map[string]string {
	m := map[string]string{
		"start_period":   cfg.startPeriod.String(),
		"start_interval": cfg.startInterval.String(),
		"interval":       cfg.interval.String(),
		"timeout":        cfg.timeout.String(),
		"retries":        strconv.Itoa(cfg.retries),
	}

	if len(cfg.test) > 0 {
		test, _ := json.Marshal(cfg.test)
		m["test"] = string(test)
	}

	if cfg.restart != "" {
		m["restart"] = cfg.restart
	}

	return m
}

// next returns the result the loop would have received.
func (s *scheduler) next(t *testing.T) instanceResult {
	t.Helper()

	select {
	case res := <-s.results:
		return res
	case <-time.After(resultGrace):
		t.Fatal("no result arrived")
		return instanceResult{}
	}
}

// silent asserts nothing was sent. The decision to send is synchronous, so a
// short grace only has to cover the goroutine reaching its failed call.
func (s *scheduler) silent(t *testing.T) {
	t.Helper()

	select {
	case res := <-s.results:
		t.Fatalf("unexpected %s result for %s", res.kind, res.name)
	case <-time.After(absenceGrace):
	}
}

func testConfig() *instanceConfig {
	return &instanceConfig{
		test:          []string{"CMD", "true"},
		startInterval: time.Second,
		interval:      10 * time.Second,
		timeout:       time.Second,
		retries:       3,
		restart:       "always",
		running:       true,
	}
}

// requireDue pins a rescheduled deadline, allowing for the jitter added to
// spread instances that share an interval.
func requireDue(t *testing.T, inst *instance, from time.Time, base time.Duration) {
	t.Helper()

	got := inst.due.Sub(from)

	require.GreaterOrEqual(t, got, base, "due must be at least the base delay")
	require.LessOrEqual(t, got, base+base/4+time.Second, "due must not exceed the base plus its jitter")
}

// syncBuffer collects log output. The action goroutines log too, so a plain
// bytes.Buffer would race with them.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.buf.String()
}

// TestLoggerTravelsInTheContext pins that the daemon logs where its caller says
// rather than to the process-wide default, which is what carries project= down.
func TestLoggerTravelsInTheContext(t *testing.T) {
	t.Parallel()

	var out syncBuffer

	s := newScheduler(t)
	s.logger = slog.New(slog.NewTextHandler(&out, &slog.HandlerOptions{Level: slog.LevelDebug}))

	inst := s.add("web-1", testConfig())
	s.running(t, inst, instanceChecking, time.Now().Add(-time.Second))

	s.run()

	require.Contains(t, out.String(), "Action deadline exceeded",
		"the watchdog must log through the context's logger, not the default")
	require.Contains(t, out.String(), "web-1")
}

// ----------------------------------------------------------------------------
// runInstanceActions
// ----------------------------------------------------------------------------

func TestRunInstanceActionsReturnsTheEarliestDeadline(t *testing.T) {
	t.Parallel()

	s := newScheduler(t)
	now := time.Now()

	s.add("a-1", testConfig()).due = now.Add(30 * time.Second)
	s.add("b-1", testConfig()).due = now.Add(5 * time.Second)
	s.add("c-1", testConfig()).due = now.Add(90 * time.Second)

	require.Equal(t, s.instances[s.key("b-1")].due, s.run())
	s.silent(t)
}

func TestRunInstanceActionsReturnsZeroWhenNothingIsWaiting(t *testing.T) {
	t.Parallel()

	s := newScheduler(t)

	inst := s.add("a-1", testConfig())
	s.running(t, inst, instanceChecking, time.Now().Add(time.Minute))

	require.True(t, s.run().IsZero(),
		"an instance with an action in flight contributes no deadline: the loop parks instead of spinning")
}

func TestRunInstanceActionsFiresTheDueAction(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		action    instanceAction
		wantState instanceState
		wantKind  instanceResultKind
	}{
		{"check", instanceActionCheck, instanceChecking, instanceResultChecked},
		{"restart", instanceActionRestart, instanceRestarting, instanceResultRestarted},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			s := newScheduler(t)

			inst := s.add("web-1", testConfig())
			inst.action = tt.action
			inst.due = time.Now().Add(-time.Second)

			require.True(t, s.run().IsZero(), "a fired instance contributes no deadline")
			require.Equal(t, tt.wantState, inst.state)
			require.NotNil(t, inst.actionContext, "the action needs a context to be told apart later")
			require.NotNil(t, inst.actionCancel)

			res := s.next(t)
			require.Equal(t, tt.wantKind, res.kind)
			require.Equal(t, "web-1", res.name)
			require.Same(t, inst.actionContext, res.ctx, "the result must carry the context it ran under")
		})
	}
}

// TestRunInstanceActionsDefersWhenNoWorkerIsFree pins what a full pool does to
// an instance: it stays idle and comes back, rather than being dropped or
// counted as an action that ran.
func TestRunInstanceActionsDefersWhenNoWorkerIsFree(t *testing.T) {
	t.Parallel()

	s := newSchedulerWithPools(t, 1, 1)

	first := s.add("web-1", testConfig())
	second := s.add("web-2", testConfig())

	// The one worker goes to whichever came first; it is held by a connect that
	// never answers, so the other has nowhere to run.
	now := time.Now()
	earliest := s.run()

	states := map[instanceState]int{first.state: 1}
	states[second.state]++
	require.Equal(t, 1, states[instanceChecking], "exactly one action may be in flight")
	require.Equal(t, 1, states[instanceIdle], "the other must stay idle")

	deferred := first
	if first.state == instanceChecking {
		deferred = second
	}

	require.Nil(t, deferred.actionContext, "a refused action must not look like one that ran")
	require.True(t, deferred.actionDeadline.IsZero(), "nor gain a deadline for the watchdog to reap")
	require.True(t, deferred.due.After(now), "it must come due again")
	require.Equal(t, deferred.due, earliest,
		"and be the deadline the loop arms its timer with, or nothing would wake for it")
}

func TestRunInstanceActionsSkipsAnInstanceWithoutATest(t *testing.T) {
	t.Parallel()

	s := newScheduler(t)

	cfg := testConfig()
	cfg.test = nil

	inst := s.add("web-1", cfg)
	inst.due = time.Now().Add(-time.Second)

	s.run()

	require.Equal(t, instanceIdle, inst.state, "there is nothing to run inside the instance")
	s.silent(t)
}

func TestRunInstanceActionsWatchdogAbandonsAStuckAction(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		action    instanceAction
		state     instanceState
		wantDue   time.Duration
		wantDelay time.Duration
	}{
		{
			// A check that overran is retried on the normal interval;
			// nothing was restarted, so the budget is left alone.
			name: "check", action: instanceActionCheck, state: instanceChecking,
			wantDue: 10 * time.Second, wantDelay: 30 * time.Second,
		},
		{
			// A restart that timed out is a restart that failed, so it
			// waits out the backoff and widens it.
			name: "restart", action: instanceActionRestart, state: instanceRestarting,
			wantDue: 30 * time.Second, wantDelay: 60 * time.Second,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			s := newScheduler(t)

			inst := s.add("web-1", testConfig())
			inst.action = tt.action
			s.running(t, inst, tt.state, time.Now().Add(-time.Second))

			abandoned := inst.actionContext
			now := time.Now()

			s.run()

			require.Equal(t, instanceIdle, inst.state, "a stuck action must not hold the instance for good")
			require.Nil(t, inst.actionContext)
			require.Nil(t, inst.actionCancel)
			require.True(t, inst.actionDeadline.IsZero())
			require.Error(t, abandoned.Err(), "the abandoned action must be canceled, not merely forgotten")

			require.Equal(t, tt.wantDelay, inst.restartDelay)
			requireDue(t, inst, now, tt.wantDue)
		})
	}
}

// TestRunInstanceActionsWatchdogCountsATimeout pins docker's rule: a probe that
// exceeds its timeout is a failed probe, so an instance whose checks always time
// out is still judged.
func TestRunInstanceActionsWatchdogCountsATimeout(t *testing.T) {
	t.Parallel()

	s := newScheduler(t)

	inst := s.add("web-1", testConfig())
	inst.config.retries = 2

	// A check was launched and has already overrun its budget.
	late := s.checked(t, inst, context.Canceled)
	inst.actionDeadline = time.Now().Add(-time.Second)

	s.run()

	require.Equal(t, instanceIdle, inst.state, "the watchdog frees the slot")
	require.Equal(t, 1, inst.failures, "a timed-out probe counts towards retries")
	require.Equal(t, instanceActionCheck, inst.action, "one failure is not yet a verdict")

	// The abandoned action reports anyway, which it wins about half the time.
	s.result(late)
	require.Equal(t, 1, inst.failures, "the stale result must not count a second time")

	s.silent(t)

	// A second timeout exhausts the retries.
	now := time.Now()

	s.checked(t, inst, context.Canceled)
	inst.actionDeadline = now.Add(-time.Second)

	s.run()

	require.Equal(t, instanceActionRestart, inst.action, "retries exhausted hands it to the restart policy")
	require.Zero(t, inst.failures, "the next round is judged on its own")
	requireDue(t, inst, now, 30*time.Second)

	res := s.next(t)
	require.Equal(t, instanceResultStatus, res.kind)
	require.Equal(t, shared.HealthStatusUnhealthy, res.status)
}

func TestRunInstanceActionsWatchdogIgnoresAZeroDeadline(t *testing.T) {
	t.Parallel()

	s := newScheduler(t)

	inst := s.add("web-1", testConfig())
	inst.state = instanceChecking

	// No action was ever launched, so there is no cancel to call.
	require.NotPanics(t, func() { s.run() })
	require.Equal(t, instanceChecking, inst.state)
}

// ----------------------------------------------------------------------------
// handleInstanceResult: checks
// ----------------------------------------------------------------------------

// checked builds the result the check action would have produced.
func (s *scheduler) checked(t *testing.T, inst *instance, err error) instanceResult {
	t.Helper()

	s.running(t, inst, instanceChecking, time.Now().Add(time.Minute))

	return instanceResult{
		kind:    instanceResultChecked,
		name:    inst.name,
		project: inst.project,
		ctx:     inst.actionContext,
		err:     err,
	}
}

func TestCheckResultHealthy(t *testing.T) {
	t.Parallel()

	s := newScheduler(t)

	inst := s.add("web-1", testConfig())
	inst.failures = 2
	inst.restartDelay = time.Minute
	inst.inRestart = true
	inst.restartDone = time.Now().Add(time.Hour)

	now := time.Now()
	s.result(s.checked(t, inst, nil))

	require.Equal(t, instanceIdle, inst.state)
	require.Zero(t, inst.failures, "a success clears the failure run")
	require.Equal(t, 30*time.Second, inst.restartDelay, "a healthy instance earns a fresh restart budget")
	require.False(t, inst.inRestart, "the first success ends the start period early, as in docker")
	require.Equal(t, instanceActionCheck, inst.action)
	requireDue(t, inst, now, 10*time.Second)

	res := s.next(t)
	require.Equal(t, instanceResultStatus, res.kind)
	require.Equal(t, shared.HealthStatusHealthy, res.status)
}

func TestCheckResultFailuresBelowRetriesStaySilent(t *testing.T) {
	t.Parallel()

	s := newScheduler(t)
	inst := s.add("web-1", testConfig())

	for i := 1; i < inst.config.retries; i++ {
		s.result(s.checked(t, inst, errors.New("exit 1")))

		require.Equal(t, i, inst.failures)
		require.Equal(t, instanceActionCheck, inst.action, "the restart is the last resort, not the first")
	}

	s.silent(t)
}

func TestCheckResultRetriesExhaustedRestarts(t *testing.T) {
	t.Parallel()

	s := newScheduler(t)

	inst := s.add("web-1", testConfig())
	inst.failures = inst.config.retries - 1

	now := time.Now()
	s.result(s.checked(t, inst, errors.New("exit 1")))

	require.Equal(t, instanceActionRestart, inst.action)
	require.Zero(t, inst.failures, "the next round is judged on its own")
	require.Equal(t, 60*time.Second, inst.restartDelay, "the delay doubles for the restart after this one")
	requireDue(t, inst, now, 30*time.Second)

	res := s.next(t)
	require.Equal(t, instanceResultStatus, res.kind)
	require.Equal(t, shared.HealthStatusUnhealthy, res.status)
}

func TestCheckResultRetriesExhaustedWithoutAPolicyKeepsChecking(t *testing.T) {
	t.Parallel()

	s := newScheduler(t)

	cfg := testConfig()
	cfg.restart = "no"

	inst := s.add("web-1", cfg)
	inst.failures = cfg.retries - 1

	s.result(s.checked(t, inst, errors.New("exit 1")))

	require.Equal(t, instanceActionCheck, inst.action,
		"nothing may restart it, but it must stay watched so it can recover to healthy")
	require.Equal(t, cfg.retries, inst.failures)

	res := s.next(t)
	require.Equal(t, shared.HealthStatusUnhealthy, res.status)
}

func TestCheckResultStartPeriodFailuresDoNotCount(t *testing.T) {
	t.Parallel()

	s := newScheduler(t)

	cfg := testConfig()
	cfg.startPeriod = time.Hour

	inst := s.add("web-1", cfg)
	inst.inRestart = true
	inst.restartDone = time.Now().Add(time.Hour)

	now := time.Now()

	for range cfg.retries + 2 {
		s.result(s.checked(t, inst, errors.New("not up yet")))
	}

	require.Zero(t, inst.failures, "as in docker, start period failures are not counted towards retries")
	require.Equal(t, instanceActionCheck, inst.action)
	require.True(t, inst.inRestart)
	requireDue(t, inst, now, cfg.startInterval)

	// One write, whatever the number of failures: it is the same status every
	// time, and reportStatus only writes a transition.
	res := s.next(t)
	require.Equal(t, instanceResultStatus, res.kind)
	require.Equal(t, shared.HealthStatusStarting, res.status,
		"a failure inside the start period says the instance is still coming up, not that it is unhealthy")

	s.silent(t)
}

func TestCheckResultStartPeriodEndsOnItsDeadline(t *testing.T) {
	t.Parallel()

	s := newScheduler(t)

	inst := s.add("web-1", testConfig())
	inst.inRestart = true
	inst.restartDone = time.Now().Add(-time.Second)

	s.result(s.checked(t, inst, errors.New("still failing")))

	require.False(t, inst.inRestart, "the start period is over, so failures start counting")
	require.Equal(t, 1, inst.failures)
}

func TestCheckResultNotRunningIsNotAVerdict(t *testing.T) {
	t.Parallel()

	s := newScheduler(t)

	inst := s.add("web-1", testConfig())
	inst.failures = 1
	inst.status = shared.HealthStatusHealthy

	now := time.Now()
	s.result(s.checked(t, inst, ErrNotRunning))

	require.Equal(t, 1, inst.failures, "a stopped instance is a lifecycle fact, not a failed check")

	// A stop event may never arrive, so the check path queues the restart
	// itself rather than waiting for one.
	require.Equal(t, instanceActionRestart, inst.action)
	requireDue(t, inst, now, baseRestartDelay(inst.config))

	// It is still not a health verdict, but the daemon owns the key: leaving
	// the last one there would report a stopped instance as healthy.
	res := s.next(t)
	require.Equal(t, instanceResultStatus, res.kind)
	require.Equal(t, shared.HealthStatusStopped, res.status)
}

// TestCheckResultNotRunningHonoursRestartPolicy pins the gate the stop event
// path applies: an instance watched only for its healthcheck is reported
// stopped and left alone, never started behind the user's back.
func TestCheckResultNotRunningHonoursRestartPolicy(t *testing.T) {
	t.Parallel()

	s := newScheduler(t)

	cfg := testConfig()
	cfg.restart = "no"

	inst := s.add("web-1", cfg)
	s.result(s.checked(t, inst, ErrNotRunning))

	require.NotEqual(t, instanceActionRestart, inst.action)

	res := s.next(t)
	require.Equal(t, instanceResultStatus, res.kind)
	require.Equal(t, shared.HealthStatusStopped, res.status)
}

// TestCheckResultNotRunningBacksOff pins that a crash loop found by the check
// path widens its window the same way the stop event path does.
func TestCheckResultNotRunningBacksOff(t *testing.T) {
	t.Parallel()

	s := newScheduler(t)

	inst := s.add("web-1", testConfig())
	base := baseRestartDelay(inst.config)

	now := time.Now()
	s.result(s.checked(t, inst, ErrNotRunning))
	requireDue(t, inst, now, base)

	inst.state = instanceIdle
	now = time.Now()
	s.result(s.checked(t, inst, ErrNotRunning))
	requireDue(t, inst, now, base*2)
}

func TestCheckResultStaleIsDropped(t *testing.T) {
	t.Parallel()

	s := newScheduler(t)

	inst := s.add("web-1", testConfig())
	res := s.checked(t, inst, nil)

	// The watchdog gave up and a replacement launched, exactly as it would
	// while the abandoned action was still unwinding.
	inst.actionCancel()
	s.running(t, inst, instanceChecking, time.Now().Add(time.Minute))

	live := inst.actionContext
	s.result(res)

	require.Equal(t, instanceChecking, inst.state, "a stale result must not free the live action's slot")
	require.Same(t, live, inst.actionContext, "nor cancel and replace its context")
	require.NoError(t, live.Err(), "the live action must still be running")

	s.silent(t)
}

func TestCheckResultDedupesTheStatusWrite(t *testing.T) {
	t.Parallel()

	s := newScheduler(t)

	inst := s.add("web-1", testConfig())
	inst.status = shared.HealthStatusHealthy

	s.result(s.checked(t, inst, nil))

	s.silent(t)
}

// ----------------------------------------------------------------------------
// handleInstanceResult: restarts, status, discovery
// ----------------------------------------------------------------------------

func (s *scheduler) restarted(t *testing.T, inst *instance, err error) instanceResult {
	t.Helper()

	s.running(t, inst, instanceRestarting, time.Now().Add(time.Minute))

	return instanceResult{
		kind:    instanceResultRestarted,
		name:    inst.name,
		project: inst.project,
		ctx:     inst.actionContext,
		err:     err,
	}
}

func TestRestartResultIntentionallyStoppedParks(t *testing.T) {
	t.Parallel()

	s := newScheduler(t)
	inst := s.add("web-1", testConfig())

	s.result(s.restarted(t, inst, ErrIntentionallyStopped))

	require.Equal(t, instanceParked, inst.state,
		"the user stopped it on purpose, so nothing here may bring it back")
}

func TestRestartResultFailureWalksTheBackoff(t *testing.T) {
	t.Parallel()

	s := newScheduler(t)

	inst := s.add("web-1", testConfig())
	require.Equal(t, 30*time.Second, inst.restartDelay)

	for _, want := range []time.Duration{30 * time.Second, 60 * time.Second} {
		now := time.Now()
		delay := inst.restartDelay

		s.result(s.restarted(t, inst, errors.New("start failed")))

		require.Equal(t, delay, want)
		require.Equal(t, instanceIdle, inst.state)
		require.Equal(t, instanceActionRestart, inst.action)
		requireDue(t, inst, now, want)
		require.Equal(t, want*2, inst.restartDelay)
	}
}

func TestRestartResultBackoffIsCapped(t *testing.T) {
	t.Parallel()

	s := newScheduler(t)

	inst := s.add("web-1", testConfig())
	inst.restartDelay = maxRestartDelay

	s.result(s.restarted(t, inst, errors.New("start failed")))

	require.Equal(t, maxRestartDelay, inst.restartDelay, "the delay may not grow past the cap")
}

func TestRestartResultSuccessReentersTheStartPeriod(t *testing.T) {
	t.Parallel()

	s := newScheduler(t)

	cfg := testConfig()
	cfg.startPeriod = time.Minute

	inst := s.add("web-1", cfg)
	inst.failures = 2

	s.result(s.restarted(t, inst, nil))

	require.Equal(t, instanceIdle, inst.state)
	require.Equal(t, instanceActionCheck, inst.action)
	require.Zero(t, inst.failures)
	require.True(t, inst.inRestart, "a restarted instance gets its start period back, as in docker")
	require.WithinDuration(t, time.Now().Add(cfg.startPeriod), inst.restartDone, 5*time.Second)
	require.Nil(t, inst.actionContext, "the finished action must release its context, not just drop it")
}

// TestStatusResultForgetsAFailedWrite pins the two halves of the status cache:
// reportStatus records what it asked for, and a failed write is forgotten.
func TestStatusResultForgetsAFailedWrite(t *testing.T) {
	t.Parallel()

	s := newScheduler(t)
	inst := s.add("web-1", testConfig())

	reportStatus(s.ctx, slog.Default(), s.conn, s.project, s.results, inst, shared.HealthStatusUnhealthy)
	require.Equal(t, shared.HealthStatusUnhealthy, inst.status,
		"the value is recorded before the write lands, so two writes cannot invert")

	reportStatus(s.ctx, slog.Default(), s.conn, s.project, s.results, inst, shared.HealthStatusUnhealthy)

	res := s.next(t)
	require.Equal(t, instanceResultStatus, res.kind)
	s.silent(t)

	// The write the daemon just made failed after all.
	res.err = errors.New("api down")
	s.result(res)

	require.Empty(t, inst.status, "a failed write must be retried, so it may not stay recorded")

	reportStatus(s.ctx, slog.Default(), s.conn, s.project, s.results, inst, shared.HealthStatusUnhealthy)
	require.Equal(t, instanceResultStatus, s.next(t).kind, "the same verdict must be written again")
}

func TestUpdatedCreatesThenRefreshes(t *testing.T) {
	t.Parallel()

	s := newScheduler(t)

	cfg := testConfig()
	cfg.startPeriod = time.Minute

	s.eventRead(incusApi.EventLifecycleInstanceUpdated, "web-1", true, healthKeys(eventConfig(cfg)))

	inst, ok := s.instances[s.key("web-1")]
	require.True(t, ok)
	require.Equal(t, instanceIdle, inst.state)
	require.Equal(t, instanceActionCheck, inst.action)
	require.Equal(t, 30*time.Second, inst.restartDelay)
	require.True(t, inst.inRestart, "a start period on a fresh instance starts running immediately")

	// A re-read of a tracked instance is a config refresh, not a reset.
	inst.failures = 2

	updated := testConfig()
	updated.interval = time.Minute
	s.eventRead(incusApi.EventLifecycleInstanceUpdated, "web-1", true, healthKeys(eventConfig(updated)))

	require.Same(t, s.instances[s.key("web-1")], inst, "the entry must survive a re-read")
	require.Equal(t, time.Minute, inst.config.interval)
	require.Equal(t, 2, inst.failures, "a config refresh must not forget the failure run")
}

// TestRereadAdoptsTheObservedStatus pins that the daemon's idea of the
// status follows the instance, not only its own writes: a cache that says
// otherwise makes every later verdict look like a no-op.
func TestRereadAdoptsTheObservedStatus(t *testing.T) {
	t.Parallel()

	s := newScheduler(t)

	inst := s.add("web-1", testConfig())
	inst.status = shared.HealthStatusHealthy

	keys := healthKeys(eventConfig(inst.config))
	keys[shared.HealthStatusKey] = shared.HealthStatusStarting

	s.eventRead(incusApi.EventLifecycleInstanceUpdated, "web-1", true, keys)

	require.Equal(t, shared.HealthStatusStarting, inst.status,
		"another writer owns the key just as much: what Incus says is what is on the instance")

	// The point of adopting it: the next verdict is written, not deduped away.
	s.result(s.checked(t, inst, nil))

	res := s.next(t)
	require.Equal(t, instanceResultStatus, res.kind)
	require.Equal(t, shared.HealthStatusHealthy, res.status)
}

// TestDiscoverResultKeepsAStatusThatAlreadyMatches pins the other half: an
// instance already reporting healthy is not rewritten.
func TestRereadKeepsAStatusThatAlreadyMatches(t *testing.T) {
	t.Parallel()

	s := newScheduler(t)

	keys := healthKeys(eventConfig(testConfig()))
	keys[shared.HealthStatusKey] = shared.HealthStatusHealthy

	s.eventRead(incusApi.EventLifecycleInstanceUpdated, "web-1", true, keys)

	inst := s.instances[s.key("web-1")]
	require.Equal(t, shared.HealthStatusHealthy, inst.status)

	s.result(s.checked(t, inst, nil))

	s.silent(t)
}

// TestUpdatedReportsAStoppedInstance covers the daemon coming up to an
// instance that is down, which has no verdict to carry.
func TestUpdatedReportsAStoppedInstance(t *testing.T) {
	t.Parallel()

	s := newScheduler(t)

	keys := healthKeys(eventConfig(testConfig()))
	keys[shared.HealthStatusKey] = shared.HealthStatusHealthy

	s.eventRead(incusApi.EventLifecycleInstanceUpdated, "web-1", false, keys)

	res := s.next(t)
	require.Equal(t, instanceResultStatus, res.kind)
	require.Equal(t, shared.HealthStatusStopped, res.status)

	// A running one is left to its check, which is what decides.
	s.eventRead(incusApi.EventLifecycleInstanceUpdated, "web-2", true, keys)

	s.silent(t)
}

func TestDiscoverResultIgnoresUninterestingInstances(t *testing.T) {
	t.Parallel()

	for _, err := range []error{ErrInstanceIgnored, ErrInstanceNoHealthcheck} {
		t.Run(err.Error(), func(t *testing.T) {
			t.Parallel()

			s := newScheduler(t)
			s.result(instanceResult{kind: instanceResultDiscovered, name: "web-1", err: err})

			require.Empty(t, s.instances)
		})
	}
}

// ----------------------------------------------------------------------------
// handleInstanceEvent
// ----------------------------------------------------------------------------

func TestStoppedSchedulesARestartWithBackoff(t *testing.T) {
	t.Parallel()

	s := newScheduler(t)
	inst := s.add("web-1", testConfig())

	now := time.Now()
	s.event(incusApi.EventLifecycleInstanceStopped, "web-1")

	require.Equal(t, instanceActionRestart, inst.action)
	requireDue(t, inst, now, 30*time.Second)
	require.Equal(t, 60*time.Second, inst.restartDelay,
		"a crash loop has to back off, so every stop widens the window")
}

// TestStoppedReportsStopped pins the daemon's half of the single-writer rule:
// the status a stop leaves behind is the daemon's to write.
func TestStoppedReportsStopped(t *testing.T) {
	t.Parallel()

	s := newScheduler(t)

	inst := s.add("web-1", testConfig())
	inst.status = shared.HealthStatusHealthy

	s.event(incusApi.EventLifecycleInstanceStopped, "web-1")

	res := s.next(t)
	require.Equal(t, instanceResultStatus, res.kind)
	require.Equal(t, shared.HealthStatusStopped, res.status)
}

// TestStoppedWithoutAPolicyReportsBeforeDropping pins that an instance the
// daemon is about to forget still gets its last status.
func TestStoppedWithoutAPolicyReportsBeforeDropping(t *testing.T) {
	t.Parallel()

	s := newScheduler(t)

	cfg := testConfig()
	cfg.restart = "no"

	inst := s.add("web-1", cfg)
	inst.status = shared.HealthStatusHealthy

	s.event(incusApi.EventLifecycleInstanceStopped, "web-1")

	require.Empty(t, s.instances)

	res := s.next(t)
	require.Equal(t, instanceResultStatus, res.kind)
	require.Equal(t, shared.HealthStatusStopped, res.status)
}

func TestStoppedTwiceKeepsTheFirstSchedule(t *testing.T) {
	t.Parallel()

	s := newScheduler(t)
	inst := s.add("web-1", testConfig())

	s.event(incusApi.EventLifecycleInstanceStopped, "web-1")

	due, delay := inst.due, inst.restartDelay

	s.event(incusApi.EventLifecycleInstanceStopped, "web-1")

	require.Equal(t, due, inst.due, "a repeat stop must not push the restart further out")
	require.Equal(t, delay, inst.restartDelay, "nor double the backoff twice for one crash")
}

func TestStoppedWithoutAPolicyDropsTheInstance(t *testing.T) {
	t.Parallel()

	s := newScheduler(t)

	cfg := testConfig()
	cfg.restart = "no"
	s.add("web-1", cfg)

	s.event(incusApi.EventLifecycleInstanceStopped, "web-1")

	require.Empty(t, s.instances, "nothing will restart it, so there is nothing left to watch")
}

func TestDeletedDropsTheInstance(t *testing.T) {
	t.Parallel()

	s := newScheduler(t)
	s.add("web-1", testConfig())

	s.event(incusApi.EventLifecycleInstanceDeleted, "web-1")

	require.Empty(t, s.instances)
}

// TestDeletedNeverTrackedIsANoop pins that a roster prune arriving for an
// instance the checker never saw - dropped by a stop event first, say - is
// harmless.
func TestDeletedNeverTrackedIsANoop(t *testing.T) {
	t.Parallel()

	s := newScheduler(t)

	s.event(incusApi.EventLifecycleInstanceDeleted, "web-1")

	require.Empty(t, s.instances)
}

func TestStartedUnparksAStoppedInstance(t *testing.T) {
	t.Parallel()

	s := newScheduler(t)

	inst := s.add("web-1", testConfig())
	inst.state = instanceParked

	now := time.Now()
	s.event(incusApi.EventLifecycleInstanceRestarted, "web-1")

	require.Equal(t, instanceIdle, inst.state, "parked has no other way out")
	require.Equal(t, instanceActionCheck, inst.action)
	require.WithinDuration(t, now, inst.due, time.Second, "a returning instance is checked at once")
}

// TestStartedCancelsAPendingRestart covers an instance that came back without
// the daemon, as `incus-compose restart` leaves it: firing the restart its stop
// queued would force-stop a running instance one backoff later.
func TestStartedCancelsAPendingRestart(t *testing.T) {
	t.Parallel()

	s := newScheduler(t)
	inst := s.add("web-1", testConfig())

	s.event(incusApi.EventLifecycleInstanceStopped, "web-1")
	require.Equal(t, instanceActionRestart, inst.action, "the stop must have queued one to begin with")

	now := time.Now()
	s.event(incusApi.EventLifecycleInstanceRestarted, "web-1")

	require.Equal(t, instanceIdle, inst.state)
	require.Equal(t, instanceActionCheck, inst.action,
		"it is running again, so there is nothing left to restart")
	require.WithinDuration(t, now, inst.due, time.Second, "a returning instance is checked at once")

	// Checking, not restarting, is what says which action the due time fired.
	s.run()
	require.Equal(t, instanceChecking, inst.state)
}

// TestStartedReentersTheStartPeriod pins that a start by anyone else leaves the
// instance in the same shape as one the daemon did itself.
func TestStartedReentersTheStartPeriod(t *testing.T) {
	t.Parallel()

	s := newScheduler(t)

	cfg := testConfig()
	cfg.startPeriod = time.Minute

	inst := s.add("web-1", cfg)
	inst.failures = 2

	s.event(incusApi.EventLifecycleInstanceRestarted, "web-1")

	require.Zero(t, inst.failures, "the run that led to the stop is over")
	require.True(t, inst.inRestart, "a restarted instance gets its start period back, as in docker")
	require.WithinDuration(t, time.Now().Add(cfg.startPeriod), inst.restartDone, 5*time.Second)
}

// TestStartedLeavesAnActionInFlightAlone pins that the event cannot desync the
// watchdog: the action in flight owns the instance.
func TestStartedLeavesAnActionInFlightAlone(t *testing.T) {
	t.Parallel()

	for _, state := range []instanceState{instanceChecking, instanceRestarting} {
		t.Run(string(state), func(t *testing.T) {
			t.Parallel()

			s := newScheduler(t)

			inst := s.add("web-1", testConfig())
			deadline := time.Now().Add(time.Minute)
			s.running(t, inst, state, deadline)

			live := inst.actionContext
			due := inst.due

			s.event(incusApi.EventLifecycleInstanceRestarted, "web-1")

			require.Equal(t, state, inst.state)
			require.Same(t, live, inst.actionContext)
			require.Equal(t, due, inst.due)
			require.Equal(t, deadline, inst.actionDeadline)
		})
	}
}

func TestStartedDiscoversAnUnknownInstance(t *testing.T) {
	t.Parallel()

	s := newScheduler(t)
	s.event(incusApi.EventLifecycleInstanceRestarted, "web-1")

	inst, ok := s.instances[s.key("web-1")]
	require.True(t, ok, "an enriched start carries everything a discovery used to read")
	require.Equal(t, instanceIdle, inst.state)
	require.Equal(t, instanceActionCheck, inst.action)
}

func TestEventsForUnknownInstancesAreHarmless(t *testing.T) {
	t.Parallel()

	for _, action := range []string{incusApi.EventLifecycleInstanceStopped, incusApi.EventLifecycleInstanceDeleted} {
		t.Run(action, func(t *testing.T) {
			t.Parallel()

			s := newScheduler(t)
			require.NotPanics(t, func() { s.event(action, "never-seen-1") })
			require.Empty(t, s.instances)
		})
	}
}
