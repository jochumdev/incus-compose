package main

import (
	"fmt"
	"strings"

	"github.com/lxc/incus-compose/shared"
)

// Defaults for what main decides. A plugin's own defaults are its business;
// these are the ones a deployment sees.
const (
	// defaultDataDir holds the enrolled certificate.
	defaultDataDir    = "/var/lib/ic-healthd"
	defaultSecretsDir = "/run/secrets"

	defaultHTTPAddr = ":9153"

	// defaultProjectMarker selects the projects handed to the shared daemon.
	defaultProjectMarker      = shared.HealthScopeKey
	defaultProjectMarkerValue = shared.HealthScopeGlobal

	defaultWorkers        = 128
	defaultRestartWorkers = 32
)

// config is everything the process was told, in one value, so it can be built
// and tested without a command line.
type config struct {
	// Incus.
	IncusURL   string
	Token      string
	DataDir    string
	SecretsDir string
	ClientCert string
	ClientKey  string
	Remote     string
	UseRemote  bool

	// What to watch.
	Projects           []string
	ProjectMarker      string
	ProjectMarkerValue string

	// The daemon's own instance, so it can report itself; empty skips it.
	OwnProject string
	OwnName    string

	// How the chain behaves.
	Workers        int
	RestartWorkers int

	// HTTPAddr serves /metrics, /health and /ready; empty disables it.
	HTTPAddr string
	Metrics  bool

	// Debug and Trace raise the process's log level; trace implies debug.
	Debug bool
	Trace bool
}

func newConfig() *config {
	return &config{
		ProjectMarker:      defaultProjectMarker,
		ProjectMarkerValue: defaultProjectMarkerValue,
		Metrics:            true,
	}
}

// parseMarker splits the project marker. A bare key means "true", so
// --project-marker user.healthcheck.scope is the short way to write the
// common case.
func parseMarker(marker string) (key, value string) {
	key, value, found := strings.Cut(marker, "=")
	if !found {
		return key, "true"
	}

	return key, value
}

// endpoint is what this will connect to, for the startup line. A remote is
// reported by name rather than resolved, which is incustrust's job.
func (c config) endpoint() string {
	if c.IncusURL != "" {
		return c.IncusURL
	}

	if c.UseRemote {
		if c.Remote != "" {
			return "remote:" + c.Remote
		}

		return "remote:default"
	}

	return ""
}

// redacted is this config with the token replaced by its length, so a startup
// log says whether a secret arrived and in what shape. An empty one stays empty.
func (c config) redacted() config {
	if c.Token != "" {
		c.Token = fmt.Sprintf("<redacted-(%d)>", len(c.Token))
	}

	return c
}
