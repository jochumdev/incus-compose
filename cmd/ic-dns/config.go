package main

import (
	"fmt"
	"net/netip"
	"strings"
	"time"
)

// Defaults for what main decides. A plugin's own defaults are its business;
// these are the ones a deployment sees.
const (
	defaultDNSAddr  = ":53"
	defaultHTTPAddr = ":8080"

	// defaultDataDir holds the enrolled certificate and what was last served.
	defaultDataDir    = "/var/lib/dns-incus"
	defaultSecretsDir = "/run/secrets"

	defaultProjectMarker      = "user.label.dns.scope"
	defaultProjectMarkerValue = "global"

	// defaultSuffix is the TLD every project's zone is built under.
	defaultSuffix = "incus"

	// defaultTTL is short on purpose: a fleet moves, and maxTTL is the most a
	// header can hold that anything sane will honor.
	defaultTTL = 5
	maxTTL     = 3600

	defaultDebounceWindow = 250 * time.Millisecond
	defaultWorkers        = 16
	defaultReadTimeout    = 10 * time.Second
	defaultProjectDelay   = 30 * time.Second
	defaultReadDelay      = 5 * time.Second
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
	Restricted bool
	Remote     string
	UseRemote  bool

	// What to serve.
	Suffix             string
	Projects           []string
	ProjectMarker      string
	ProjectMarkerValue string

	// Where to serve it.
	DNSAddr  string
	HTTPAddr string
	Forward  []string

	// AllowTransfer is who may ask for a zone transfer, as CIDR prefixes. Empty
	// allows nobody, so a transfer is opt-in here as well as at the zone.
	AllowTransfer []netip.Prefix

	// How the chain behaves.
	TTL            uint
	DebounceWindow time.Duration
	Workers        int
	ReadTimeout    time.Duration
	ProjectDelay   time.Duration
	ReadDelay      time.Duration
	EchoSubnet     bool
	Metrics        bool
	Exclude        []string

	// Log is the level every chain log position prints a routine event at, and
	// the process's own log level. Empty leaves the positions out and the
	// process at Info.
	Log string

	Pprof bool
}

func newConfig() *config {
	return &config{
		ProjectMarker:      defaultProjectMarker,
		ProjectMarkerValue: defaultProjectMarkerValue,
	}
}

// parseMarker splits the project marker. A bare key means "true", so
// --project-marker user.dns is the short way to write the common case.
func parseMarker(marker string) (key, value string) {
	key, value, found := strings.Cut(marker, "=")
	if !found {
		return key, "true"
	}

	return key, value
}

// validate rejects what cannot work, at startup, while somebody is watching.
func (c config) validate() error {
	if c.TTL > maxTTL {
		return fmt.Errorf("ttl %d is out of range, the most a record may carry is %d", c.TTL, maxTTL)
	}

	return nil
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
