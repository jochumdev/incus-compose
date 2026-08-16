# incus-compose development commands
#
# Quick start:
#   just dev-install    # Set up nested Incus for testing
#   just run <args>     # Run incus-compose against nested Incus
#   just test           # Run tests against nested Incus
#   just test-local     # Unit tests only, no Incus needed
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

[private]
default:
    @just --list
