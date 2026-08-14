# incus-compose development commands
#
# Quick start:
#   just dev-install    # Set up nested Incus for testing
#   just run <args>     # Run incus-compose against nested Incus
#   just test           # Run tests against nested Incus
#
# Local development (uses your real Incus - be careful!):
#   just run-local <args>
#   just test-local
#
# Recipes live in just/*.just by topic. Everything the coredns merge brings is
# an import of its own, so it lands here without touching these.

set dotenv-load
set shell := ["bash", "-euo", "pipefail", "-c"]
# Pass args as real positional parameters so "$@" preserves shell metacharacters
# (e.g. a -run 'TestA|TestB' pattern is not split on the pipe).
set positional-arguments

v_test_procs := env("TEST_PROCS", "2")

import 'just/mod.just'
import 'just/test.just'
import 'just/build.just'
import 'just/fleet.just'
import 'just/incus.just'

import 'just/coredns-build.just'
import 'just/coredns-compose.just'

[private]
default:
    @just --list
