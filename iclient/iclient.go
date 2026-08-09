// Package iclient is a fork of github.com/lxc/incus/v7/client (Apache-2.0).
//
// It exists because the upstream client races on state shared between the
// connection, its event listeners and the operations running on it, so a single
// InstanceServer cannot be used from several goroutines.
//
// The configuration read path is forked from
// github.com/lxc/incus/v7/shared/cliconfig. Nothing is mutated after ReadConfig
// returns, so a *Config is safe to share.
package iclient
