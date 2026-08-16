# The module and the gates built on it.
#
# One module, github.com/lxc/incus-compose, which is what incus-compose itself
# is - so arriving there is a git mv and a module path rewrite rather than a
# re-layout. benchmarks/ stays nested and out of it: the kubernetes yardstick
# drags in client-go, and that has no business in the module ecs_view ships
# from. It reaches back with a replace, being test-only and never published.

# Run this before you commit/push.
pre-commit:
    just tidy
    just boundary
    just lint
    # just test-all

push: pre-commit
    git push

# List every module directory, one per line. The root, plus the nested
# benchmarks, which `go list -m` does not see from here.
[doc("List every module directory, one per line")]
modules:
    @echo "{{ justfile_directory() }}"
    @echo "{{ justfile_directory() }}/benchmarks/coredns/ecs_view"
    @echo "{{ justfile_directory() }}/benchmarks/coredns/kubernetes"

# Dependency direction, which module edges used to make visible and no longer
# do. One module means an upward import is just an import, so it is checked
# rather than trusted - AGENTS.md: enforce by code, not by comment.
#
#   ievent/dns/ecs_view  the engine. It answers queries and derives nothing, so
#                        it must not reach Incus, iclient, or the rest of
#                        ievent. Living under its only consumer does not make it
#                        part of it: what keeps the query path free of
#                        derivation is that it cannot see where records came
#                        from.
#   client/, iclient/,   the library packages, at the root because that is where
#   shared/, incustrust/ incus-compose has them and their import path is what a
#                        library user wrote down. Imported downward only: never
#                        reaches coredns/, cmd/, ievent/ or benchmarks/.
#   internal/testlib     the same direction. internal/ keeps it off a library
#                        user, this keeps it off the packages it serves.
#
# A package that does not exist is a failure, not a pass. The rule above used to
# name coredns/plugin/ecs_view, and when that moved the check went quietly
# vacuous - which is exactly the thing a check is for.
[doc("Check the dependency direction between packages")]
boundary:
    #!/usr/bin/env bash
    set -uo pipefail
    failed=()

    check() {
        local pkg="$1" pattern="$2" why="$3"
        local deps
        deps=$(go list -deps "$pkg" 2>/dev/null || true)
        if [ -z "$deps" ]; then
            echo "==> $pkg does not resolve, so its rule checks nothing"
            failed+=("$pkg")
            return
        fi
        local hits
        # The package itself is in its own dep list, and a rule naming the tree
        # it lives in would match it.
        hits=$(echo "$deps" | grep -vFx "${pkg#./github.com/lxc/incus-compose/}" | grep -vFx "github.com/lxc/incus-compose/${pkg#./}" | grep -E "$pattern" || true)
        if [ -n "$hits" ]; then
            echo "==> $pkg must not import $why"
            echo "$hits" | sed 's/^/    /'
            failed+=("$pkg")
        fi
    }

    check ./ievent/dns/ecs_view \
        '^github.com/lxc/incus(-compose/(client|iclient|shared|incustrust|internal/testlib|ievent))?(/|$)' \
        'Incus, iclient or the rest of ievent'

    for dir in ./client/... ./iclient/... ./shared/... ./incustrust/... ./internal/testlib/...; do
        check "$dir" \
            '^github.com/lxc/incus-compose/(coredns|cmd|ievent|benchmarks)(/|$)' \
            'anything that consumes it'
    done

    if [ ${#failed[@]} -gt 0 ]; then
        echo
        echo "boundary violated in: ${failed[*]}" >&2
        exit 1
    fi

    echo "boundaries hold"

# Lint all files. The path scopes the Go step, the rest are whole-tree.
lint path="./...":
    shellcheck **/*.sh
    npx --yes prettier --check .
    cd docs; npx --yes prettier --check .
    golangci-lint run {{ path }}

# Lint and fix all files. Imports are gopls' job, not this one - see AGENTS.md.
fix path="./...":
    npx --yes prettier --write .
    cd docs; npx --yes prettier --write .
    golangci-lint run --fix {{ path }}

# Run `go mod tidy`.
#
# Plain tidy, in every module, with nothing injected. The replace-and-strip
# dance is gone with go.work: the root module needs no help resolving itself,
# and benchmarks/coredns/ecs_view carries a permanent replace back to it.
[doc("Run `go mod tidy` in every module")]
tidy:
    #!/usr/bin/env bash
    set -uo pipefail

    # Reported rather than fatal: benchmarks/coredns/kubernetes cannot be tidied
    # at all, since the CoreDNS kubernetes plugin needs k8s.io/api packages
    # @latest has dropped.
    failed=()
    for dir in $(just modules); do
        echo "==> ${dir#"${PWD}"/}"
        go -C "$dir" mod tidy || failed+=("${dir#"${PWD}"/}")
    done

    if [ ${#failed[@]} -gt 0 ]; then
        echo
        echo "tidy failed in: ${failed[*]}" >&2
    fi
