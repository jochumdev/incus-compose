package client

import (
	"context"

	"github.com/lxc/incus-compose/shared"
)

// MaxIncusNameLen is the maximum length for Incus instance names.
// Incus allows up to 63 characters (DNS hostname limit).
const MaxIncusNameLen = 63

// Health check status constants written to HealthConfigKey by ic-healthd.
// Re-exported from the shared package for backward compatibility with
// existing client.Health* references.
const (
	HealthStatusUnknown   = shared.HealthStatusUnknown
	HealthStatusHealthy   = shared.HealthStatusHealthy
	HealthStatusUnhealthy = shared.HealthStatusUnhealthy
	HealthStatusStopped   = shared.HealthStatusStopped

	HealthKeyPrefix = shared.HealthKeyPrefix

	// HealthStatusKey is the instance config key used to store health status.
	HealthStatusKey = shared.HealthStatusKey

	// HealthStoppedKey when "true" means healthchecking is stopped.
	HealthStoppedKey = shared.HealthStoppedKey
)

// DefaultSystemProject is the Incus project the library runs its own instances
// in.
const DefaultSystemProject = "incus-client"

// Kind identifies a resource type.
type Kind string

// Resource kind identifiers.
const (
	KindProject       Kind = "project"
	KindProfile       Kind = "profile"
	KindImage         Kind = "image"
	KindStorageVolume Kind = "storage-volume"
	KindNetwork       Kind = "network"
	KindInstance      Kind = "instance"
)

// BackupVolumePrefix is the prefix for backup volumes: %prefix-%project
const BackupVolumePrefix = "ic-backup-"

// Action identifies a resource action.
type Action string

// Action constants for resource actions.
const (
	ActionEnsure Action = "ensure"
	ActionDelete Action = "delete"
	ActionStart  Action = "start"
	ActionStop   Action = "stop"
	ActionLog    Action = "log"
	ActionBackup Action = "backup"
)

// Resource defines the common interface for all Incus resources.
type Resource interface {
	// Kind returns the resource type identifier (e.g., "instance", "network").
	Kind() Kind

	// Name returns the user-facing resource name.
	Name() string

	// IncusName returns the sanitized name for incus.
	IncusName() string

	// Priority returns the creation/deletion priority for dependency ordering.
	// Lower values are created first and deleted last.
	Priority() int

	// IsEnsured returns wherever the resource has been ensured.
	IsEnsured() bool

	// Created returns true if the resource was created during the last Ensure call.
	// Returns false if the resource already existed or hasn't been ensured yet.
	Created() bool
}

// Config is implemented by resource configuration types.
type Config interface {
	GetConfig() any
}

// type EnsuredResource interface {
// 	Resource
// }

// EnsureAble is implemented by resources that can be ensured.
type EnsureAble interface {
	// Ensure fetches an existing Resource or creates a new one.
	// If a Resource with the same name exists, it is returned.
	Ensure(ctx context.Context, opts ...Option) error
}

// StartAble is implemented by resources that can be started.
type StartAble interface {
	Start(ctx context.Context, opts ...Option) error
}

// StopAble is implemented by resources that can be stopped.
type StopAble interface {
	Stop(ctx context.Context, opts ...Option) error
}

// DeleteAble is implemented by resources that can be deleted.
type DeleteAble interface {
	Delete(ctx context.Context, opts ...Option) error
}

// LogAble is implemented by resources that can stream logs.
type LogAble interface {
	Log(ctx context.Context, opts ...Option) error
}

// BackupAble is implemented by resources that can be backed up.
type BackupAble interface {
	BackupEntry(cfg BackupConfig, backupProject string) BackupVolume
	Backup(ctx context.Context, opts ...Option) error
}
