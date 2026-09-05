package checker

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/lxc/incus-compose/shared"
)

// TestParseInstanceConfigSelects pins which instances are worth watching at all.
func TestParseInstanceConfigSelects(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		config  map[string]string
		wantErr error
	}{
		{
			name:    "ignored wins over a valid healthcheck",
			config:  healthKeys(map[string]string{"ignore": "true", "test": `["CMD","true"]`}),
			wantErr: ErrInstanceIgnored,
		},
		{
			name:    "no test and no restart policy",
			config:  healthKeys(nil),
			wantErr: ErrInstanceNoHealthcheck,
		},
		{
			// Wants checking, never opted in: reported, not skipped quietly.
			name: "a healthcheck without the opt-in",
			config: map[string]string{
				shared.HealthKeyPrefix + "test": `["CMD","true"]`,
			},
			wantErr: ErrInstanceNotEnabled,
		},
		{
			// Not enabled and nothing to check anyway: nothing to say.
			name:    "neither the opt-in nor anything to check",
			config:  map[string]string{},
			wantErr: ErrInstanceNoHealthcheck,
		},
		{
			// The opt-in alone is not enough; it still needs something to run.
			name:    "the opt-in with nothing to check",
			config:  map[string]string{shared.HealthEnabledKey: "true"},
			wantErr: ErrInstanceNoHealthcheck,
		},
		{
			// Ownership is a separate question: the daemon does not read it.
			name: "not created by incus-compose, but opted in",
			config: map[string]string{
				shared.HealthEnabledKey:         "true",
				shared.HealthKeyPrefix + "test": `["CMD","true"]`,
			},
		},
		{
			name:    "restart no is not a policy worth watching",
			config:  healthKeys(map[string]string{"restart": "no"}),
			wantErr: ErrInstanceNoHealthcheck,
		},
		{
			name:   "a test alone is enough",
			config: healthKeys(map[string]string{"test": `["CMD","true"]`}),
		},
		{
			name:   "a restart policy alone is enough",
			config: healthKeys(map[string]string{"restart": "always"}),
		},
		{
			name:   "restart no is fine when a test is present",
			config: healthKeys(map[string]string{"restart": "no", "test": `["CMD","true"]`}),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			cfg, err := parseInstanceConfig(tt.config, true)

			if tt.wantErr != nil {
				require.ErrorIs(t, err, tt.wantErr)
				require.Nil(t, cfg)

				return
			}

			require.NoError(t, err)
			require.NotNil(t, cfg)
		})
	}
}

// TestParseInstanceConfigDefaults pins the values documented in healthd.md.
func TestParseInstanceConfigDefaults(t *testing.T) {
	t.Parallel()

	cfg, err := parseInstanceConfig(healthKeys(map[string]string{"test": `["CMD","true"]`}), true)
	require.NoError(t, err)

	require.Equal(t, time.Duration(0), cfg.startPeriod, "start_period defaults to disabled")
	require.Equal(t, 5*time.Second, cfg.startInterval)
	require.Equal(t, 30*time.Second, cfg.interval)
	require.Equal(t, 30*time.Second, cfg.timeout)
	require.Equal(t, 3, cfg.retries)
	require.Equal(t, []string{"CMD", "true"}, cfg.test)
	require.Empty(t, cfg.restart)
	require.True(t, cfg.running, "running mirrors StatusCode")
}

// TestParseInstanceConfigReadsEveryKey pins that no key is silently ignored.
func TestParseInstanceConfigReadsEveryKey(t *testing.T) {
	t.Parallel()

	cfg, err := parseInstanceConfig(healthKeys(map[string]string{
		"test":           `["CMD-SHELL","exit 0"]`,
		"start_period":   "45s",
		"start_interval": "2s",
		"interval":       "7s",
		"timeout":        "3s",
		"retries":        "5",
		"restart":        "unless-stopped",
	}), false)
	require.NoError(t, err)

	require.Equal(t, []string{"CMD-SHELL", "exit 0"}, cfg.test)
	require.Equal(t, 45*time.Second, cfg.startPeriod)
	require.Equal(t, 2*time.Second, cfg.startInterval)
	require.Equal(t, 7*time.Second, cfg.interval)
	require.Equal(t, 3*time.Second, cfg.timeout)
	require.Equal(t, 5, cfg.retries)
	require.Equal(t, "unless-stopped", cfg.restart)
	require.False(t, cfg.running, "a stopped instance parses as not running")
}

// TestParseInstanceConfigRejects covers every guard: a non-positive interval or
// timeout reaching the scheduler makes a check fail instantly and forever.
func TestParseInstanceConfigRejects(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		config map[string]string
	}{
		{"interval zero", healthKeys(map[string]string{"test": `["CMD","true"]`, "interval": "0s"})},
		{"interval negative", healthKeys(map[string]string{"test": `["CMD","true"]`, "interval": "-5s"})},
		{"interval unparseable", healthKeys(map[string]string{"test": `["CMD","true"]`, "interval": "soon"})},
		{"start_interval zero", healthKeys(map[string]string{"test": `["CMD","true"]`, "start_interval": "0s"})},
		{"start_interval negative", healthKeys(map[string]string{"test": `["CMD","true"]`, "start_interval": "-1s"})},
		{"start_period unparseable", healthKeys(map[string]string{"test": `["CMD","true"]`, "start_period": "later"})},
		{"timeout zero", healthKeys(map[string]string{"test": `["CMD","true"]`, "timeout": "0s"})},
		{"timeout negative", healthKeys(map[string]string{"test": `["CMD","true"]`, "timeout": "-2s"})},
		{"timeout unparseable", healthKeys(map[string]string{"test": `["CMD","true"]`, "timeout": "ages"})},
		{"retries zero", healthKeys(map[string]string{"test": `["CMD","true"]`, "retries": "0"})},
		{"retries not a number", healthKeys(map[string]string{"test": `["CMD","true"]`, "retries": "many"})},
		{"test is not json", healthKeys(map[string]string{"test": `CMD true`})},
		{"CMD-SHELL without a command", healthKeys(map[string]string{"test": `["CMD-SHELL"]`})},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			cfg, err := parseInstanceConfig(tt.config, true)

			require.Error(t, err)
			require.Nil(t, cfg, "a rejected config must not be handed on half-built")
		})
	}
}

// TestParseInstanceConfigRestartWithoutTest pins the no-op probe: an instance
// watched only for its restart policy still needs an action to run.
func TestParseInstanceConfigRestartWithoutTest(t *testing.T) {
	t.Parallel()

	for _, policy := range shared.RestartPolicies {
		t.Run(policy, func(t *testing.T) {
			t.Parallel()

			cfg, err := parseInstanceConfig(healthKeys(map[string]string{"restart": policy}), true)
			require.NoError(t, err)

			require.Equal(t, []string{"NONE"}, cfg.test,
				"without a test the probe must be NONE, not empty: an empty test is never scheduled")
		})
	}
}

// TestBaseRestartDelayClamps pins the documented interval*retries baseline and
// both ends of its range.
func TestBaseRestartDelayClamps(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		interval time.Duration
		retries  int
		want     time.Duration
	}{
		{"the documented product", 5 * time.Second, 3, 15 * time.Second},
		{"exactly the floor", 5 * time.Second, 1, defaultRestartDelay},
		{"below the floor is raised", time.Second, 1, defaultRestartDelay},
		{"above the ceiling is capped", 10 * time.Minute, 3, maxRestartDelay},
		{"a zero interval falls back", 0, 3, defaultRestartDelay},
		{"zero retries fall back", 5 * time.Second, 0, defaultRestartDelay},
		{"a negative interval falls back", -time.Second, 3, defaultRestartDelay},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := baseRestartDelay(&instanceConfig{interval: tt.interval, retries: tt.retries})
			require.Equal(t, tt.want, got)
		})
	}
}

// TestInstanceConfigEquals pins that every field counts: equals decides whether
// a re-discovered instance replaces its config.
func TestInstanceConfigEquals(t *testing.T) {
	t.Parallel()

	base := func() *instanceConfig {
		return &instanceConfig{
			test:          []string{"CMD", "true"},
			startPeriod:   time.Second,
			startInterval: 2 * time.Second,
			interval:      3 * time.Second,
			timeout:       4 * time.Second,
			retries:       5,
			restart:       "always",
			running:       true,
		}
	}

	require.True(t, base().equals(base()), "identical configs must compare equal")

	tests := map[string]func(*instanceConfig){
		"test":           func(c *instanceConfig) { c.test = []string{"CMD", "false"} },
		"test length":    func(c *instanceConfig) { c.test = []string{"CMD"} },
		"start_period":   func(c *instanceConfig) { c.startPeriod = time.Hour },
		"start_interval": func(c *instanceConfig) { c.startInterval = time.Hour },
		"interval":       func(c *instanceConfig) { c.interval = time.Hour },
		"timeout":        func(c *instanceConfig) { c.timeout = time.Hour },
		"retries":        func(c *instanceConfig) { c.retries = 9 },
		"restart":        func(c *instanceConfig) { c.restart = "on-failure" },
		"running":        func(c *instanceConfig) { c.running = false },
	}

	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			other := base()
			mutate(other)

			require.False(t, base().equals(other), "%s differing must not compare equal", name)
		})
	}
}

// TestParseInstanceConfigStatusIsNotRead pins that the daemon never treats the
// key it writes as an input.
func TestParseInstanceConfigStatusIsNotRead(t *testing.T) {
	t.Parallel()

	with, err := parseInstanceConfig(healthKeys(map[string]string{
		"test":   `["CMD","true"]`,
		"status": shared.HealthStatusUnhealthy,
	}), true)
	require.NoError(t, err)

	without, err := parseInstanceConfig(healthKeys(map[string]string{"test": `["CMD","true"]`}), true)
	require.NoError(t, err)

	require.True(t, with.equals(without), "the status key must not change the parsed config")
}
