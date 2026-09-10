package shared

// DNS constants for ic-dns configuration and discovery.
const (
	DNSKeyPrefix = "user.dns."

	// DNSDaemonScopeKey is where the daemon instance stores its scope.
	DNSDaemonScopeKey = DNSKeyPrefix + "scope"

	// DNSScopeKey is the project config key opting a project into DNS.
	DNSScopeKey = "user.label.dns.scope"

	// DNSZoneKey is the project config key setting the DNS zone.
	DNSZoneKey = "user.label.dns.zone"

	DNSScopeProject = "project"
	DNSScopeGlobal  = "global"
)
