package main

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/urfave/cli/v3"
)

// parse drives the real flag set with an argv, so this tests the command line
// rather than a struct literal.
func parse(t *testing.T, argv ...string) *config {
	t.Helper()

	got := newConfig()

	// The real command, with only its action replaced.
	cmd := runCommand(got)
	cmd.Action = func(_ context.Context, c *cli.Command) error {
		return nil
	}

	err := cmd.Run(t.Context(), append([]string{"run"}, argv...))
	require.NoError(t, err)

	return got
}

// TestConfigDefaults pins what a deployment gets for free.
func TestConfigDefaults(t *testing.T) {
	cfg := parse(t)

	assert.Equal(t, ":53", cfg.DNSAddr)
	assert.Equal(t, ":8080", cfg.HTTPAddr)
	assert.Equal(t, "/var/lib/dns-incus", cfg.DataDir)
	assert.Equal(t, "/run/secrets", cfg.SecretsDir)
	assert.Equal(t, 250*time.Millisecond, cfg.DebounceWindow)
	assert.Equal(t, 16, cfg.Workers)
	assert.Equal(t, 30*time.Second, cfg.ProjectDelay)
	assert.Equal(t, 5*time.Second, cfg.ReadDelay)
	assert.Equal(t, defaultProjectMarker, cfg.ProjectMarker)
	assert.Equal(t, defaultProjectMarkerValue, cfg.ProjectMarkerValue)

	// Off by default: neither a confined certificate nor somebody's CLI identity
	// is a thing to opt into by accident.
	assert.False(t, cfg.Restricted)
	assert.False(t, cfg.UseRemote)

	// Off with them: a binary nobody scrapes pays nothing for the numbers, so
	// recording them is the deliberate act.
	assert.False(t, cfg.Metrics)
}

func TestConfigFromCommand(t *testing.T) {
	cfg := parse(t,
		"--incus", "https://10.0.0.1:8443",
		"--token", "secret",
		"--project", "shop", "--project", "web",
		"--forward", "10.0.0.1:53", "--forward", "10.0.0.2:53",
		"--exclude", "log/arrival",
		"--restricted",
		"--log", "TRACE",
	)

	assert.Equal(t, "https://10.0.0.1:8443", cfg.IncusURL)
	assert.Equal(t, "secret", cfg.Token)
	assert.Equal(t, []string{"shop", "web"}, cfg.Projects)
	assert.Equal(t, []string{"10.0.0.1:53", "10.0.0.2:53"}, cfg.Forward)
	assert.Equal(t, []string{"log/arrival"}, cfg.Exclude)
	assert.True(t, cfg.Restricted)
	assert.Equal(t, "TRACE", cfg.Log)

	cfg = parse(t, "--project-marker", "user.custom=val")
	assert.Equal(t, "user.custom", cfg.ProjectMarker)
	assert.Equal(t, "val", cfg.ProjectMarkerValue)

	cfg = parse(t, "--project-marker", "user.dns")
	assert.Equal(t, "user.dns", cfg.ProjectMarker)
	assert.Equal(t, "true", cfg.ProjectMarkerValue)

	cfg = parse(t, "--project-marker", "")
	assert.Equal(t, "", cfg.ProjectMarker)
	assert.Equal(t, "true", cfg.ProjectMarkerValue)
}

// TestConfigFromEnvironment pins the half a container actually uses. A flag
// that reads no environment variable is a flag a compose file cannot set.
func TestConfigFromEnvironment(t *testing.T) {
	t.Setenv("DNS_INCUS", "https://env:8443")
	t.Setenv("DNS_LISTEN", "127.0.0.1:5353")
	t.Setenv("DNS_WORKERS", "8")
	t.Setenv("DNS_LOG", "DEBUG")

	cfg := parse(t)

	assert.Equal(t, "https://env:8443", cfg.IncusURL)
	assert.Equal(t, "127.0.0.1:5353", cfg.DNSAddr)
	assert.Equal(t, 8, cfg.Workers)
	assert.Equal(t, "DEBUG", cfg.Log)

	// And what is off by default is on when it is asked for.
	cfg = parse(t, "--metrics")
	assert.True(t, cfg.Metrics)

	// The flag still wins over the environment.
	cfg = parse(t, "--listen", "127.0.0.1:15353")
	assert.Equal(t, "127.0.0.1:15353", cfg.DNSAddr)

	t.Setenv("DNS_PROJECT_MARKER", "user.custom=val")
	cfg = parse(t)
	assert.Equal(t, "user.custom", cfg.ProjectMarker)
	assert.Equal(t, "val", cfg.ProjectMarkerValue)

	t.Setenv("DNS_PROJECT_MARKER", "user.dns")
	cfg = parse(t)
	assert.Equal(t, "user.dns", cfg.ProjectMarker)
	assert.Equal(t, "true", cfg.ProjectMarkerValue)

	t.Setenv("DNS_PROJECT_MARKER", "")
	cfg = parse(t)
	assert.Equal(t, "", cfg.ProjectMarker)
	assert.Equal(t, "true", cfg.ProjectMarkerValue)

	t.Setenv("DNS_PROJECT_MARKER", "user.env=fromenv")
	cfg = parse(t, "--project-marker", "user.cli=fromcli")
	assert.Equal(t, "user.cli", cfg.ProjectMarker)
	assert.Equal(t, "fromcli", cfg.ProjectMarkerValue)
}

func TestParseMarker(t *testing.T) {
	cases := []struct {
		name       string
		in         string
		key, value string
	}{
		{
			name:  "a bare key means true",
			in:    "user.dns",
			key:   "user.dns",
			value: "true",
		},
		{
			name:  "an explicit value is taken as it stands",
			in:    "user.label.dns.scope=global",
			key:   "user.label.dns.scope",
			value: "global",
		},
		{
			name:  "the default splits",
			in:    defaultProjectMarker + "=" + defaultProjectMarkerValue,
			key:   "user.label.dns.scope",
			value: "global",
		},
		{
			// serves turns this into a nil filter: every visible project.
			name:  "an empty marker keys on nothing",
			in:    "",
			key:   "",
			value: "true",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			key, value := parseMarker(tc.in)
			assert.Equal(t, tc.key, key)
			assert.Equal(t, tc.value, value)
		})
	}
}

// TestRedactedKeepsTheToken pins that the token never reaches a log line, and
// that redacting does not change the config the process runs on.
func TestRedactedKeepsTheToken(t *testing.T) {
	cfg := config{Token: "secret", IncusURL: "https://10.0.0.1:8443"}

	// The length survives, which is what catches a secret with a trailing newline.
	out := cfg.redacted()
	assert.Equal(t, "<redacted-(6)>", out.Token)
	assert.NotContains(t, out.Token, "secret")
	assert.Equal(t, cfg.IncusURL, out.IncusURL)

	// A value receiver, so the original is untouched.
	assert.Equal(t, "secret", cfg.Token)

	// An empty token stays empty rather than reporting zero length.
	assert.Empty(t, config{}.redacted().Token)

	// And the length is the token's, not a constant that happens to fit.
	assert.Equal(t, "<redacted-(11)>", config{Token: "hello world"}.redacted().Token)
}
