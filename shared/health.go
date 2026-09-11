// Package shared holds constants and small helpers shared across packages
// that would otherwise need to import the full client package.
package shared

// Health check status constants written to HealthConfigKey by ic-healthd.
const (
	HealthStatusUnknown   = "unknown"
	HealthStatusStarting  = "starting"
	HealthStatusHealthy   = "healthy"
	HealthStatusUnhealthy = "unhealthy"
	HealthStatusStopped   = "stopped"

	HealthKeyPrefix = "user.healthcheck."

	// HealthStatusKey is the instance config key used to store health status.
	HealthStatusKey = HealthKeyPrefix + "status"

	// HealthEnabledKey when "true" opts the instance in, and is the only thing
	// ic-healthd consults. On a project it means the same one level up.
	HealthEnabledKey = HealthKeyPrefix + "enabled"

	// HealthStoppedKey when "true" means healthchecking is stopped.
	HealthStoppedKey = HealthKeyPrefix + "stopped"

	// HealthIgnoreKey opts an instance out of health checking entirely.
	HealthIgnoreKey = HealthKeyPrefix + "ignore"

	// HealthScopeKey names the daemon watching a project. Missing means neither.
	HealthScopeKey = HealthKeyPrefix + "scope"

	// HealthProjectKey names the Incus project the shared daemon runs in, which
	// is not the system project when it attaches to another project's network.
	HealthProjectKey = HealthKeyPrefix + "project"
)

// Values of HealthScopeKey.
const (
	HealthScopeProject = "project"
	HealthScopeGlobal  = "global"
)

// RestartPolicies are the restart values worth tracking even without a test command.
var RestartPolicies = []string{"always", "on-failure", "unless-stopped"}
