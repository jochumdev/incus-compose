// Package testlib holds what every package's tests need: the tier guards, the
// naming and argument conventions, and snapshot normalization.
//
// Under internal/ because it has no stability promise: every signature here
// changes whenever a test needs it to, with no changelog entry. The compiler is
// what keeps that off a library user, not this paragraph.
//
// It may import the standard library, external modules, and shared. client,
// iclient and project test in-package, so a helper here that reached for one of
// them would be an import cycle for exactly the tests that need it most. A
// helper that does need our own types belongs in the package it serves.
package testlib
