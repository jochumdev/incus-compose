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
# benchmarks, which `go list -m` does not see from here. A branch without the
# benchmarks lists only the root: a directory with no go.mod is not a module.
[doc("List every module directory, one per line")]
modules:
    #!/usr/bin/env bash
    set -uo pipefail

    echo "{{ justfile_directory() }}"

    for m in benchmarks/ic-dns/ecs_view benchmarks/ic-dns/kubernetes; do
        if [ -f "{{ justfile_directory() }}/$m/go.mod" ]; then
            echo "{{ justfile_directory() }}/$m"
        fi
    done

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
#                        reaches cmd/, ievent/ or benchmarks/.
#   internal/testlib     the same direction. internal/ keeps it off a library
#                        user, this keeps it off the packages it serves.
#
# A package that does not exist is a failure, not a pass: a rule naming one that
# has since moved goes quietly vacuous, which is exactly the thing a check is
# for.
#
# A tree the branch simply does not carry yet is the one exception, and it is
# announced rather than passed over in silence. The gate is the top directory,
# so a package moving *within* a tree that is present still fails: only dropping
# the whole tree skips its rule.
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

    # Gated on the tree, not on the package: within an ievent/ that is here, a
    # moved ecs_view still has to resolve.
    if [ -d ./ievent ]; then
        check ./ievent/dns/ecs_view \
            '^github.com/lxc/incus(-compose/(client|iclient|shared|incustrust|internal/testlib|ievent))?(/|$)' \
            'Incus, iclient or the rest of ievent'
    else
        echo "==> ievent/ is not on this branch, skipping its rule"
    fi

    for dir in ./client/... ./iclient/... ./shared/... ./incustrust/... ./internal/testlib/...; do
        # The first segment, so a package missing inside a tree that is here
        # reaches check() and fails there.
        tree="${dir#./}"
        tree="${tree%%/*}"
        if [ ! -d "./$tree" ]; then
            echo "==> $tree/ is not on this branch, skipping its rule"
            continue
        fi

        check "$dir" \
            '^github.com/lxc/incus-compose/(cmd|ievent|benchmarks)(/|$)' \
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
    golangci-lint run --fix {{ path }}

# Run `go mod tidy`.
#
# Plain tidy, in every module, with nothing injected. The replace-and-strip
# dance is gone with go.work: the root module needs no help resolving itself,
# and benchmarks/ic-dns/ecs_view carries a permanent replace back to it.
[doc("Run `go mod tidy` in every module")]
tidy:
    #!/usr/bin/env bash
    set -uo pipefail

    # Reported rather than fatal: benchmarks/ic-dns/kubernetes cannot be tidied
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
