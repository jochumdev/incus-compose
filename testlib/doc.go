// Package testlib holds what every package's tests need: the tier guards, the
// naming and argument conventions, and snapshot normalization.
//
// It may import the standard library and external modules, and nothing from
// incus-compose. client, iclient and project test in-package, so a helper here
// that reached for one of them would be an import cycle for exactly the tests
// that need it most. A helper that does need our own types belongs in the
// package it serves.
package testlib
