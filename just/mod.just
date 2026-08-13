# The module and the gates built on it.
#
# One module today. The loops below exist because the coredns merge brings
# nested modules with it (its benchmarks stay out of the module ecs_view ships
# from), and `go list -m` does not see those from here.

# List every module directory, one per line.
modules:
    @echo "{{ justfile_directory() }}"

# Lint all files.
lint folder="./...":
    shellcheck **/*.sh
    npx --yes prettier --check .
    cd docs; npx --yes prettier --check .
    golangci-lint run {{ folder }}

# Lint and fix all files. Imports are gopls' job, not this one - see AGENTS.md.
fix folder="./...":
    npx --yes prettier --write .
    cd docs; npx --yes prettier --write .
    golangci-lint run --fix {{ folder }}

# Run `go mod tidy` in every module, reporting every failure rather than the first.
tidy:
    #!/usr/bin/env bash
    set -uo pipefail
    failed=()
    for dir in $(just modules); do
        echo "==> ${dir#"${PWD}"/}"
        go -C "$dir" mod tidy || failed+=("${dir#"${PWD}"/}")
    done
    if [ ${#failed[@]} -gt 0 ]; then
        echo
        echo "tidy failed in: ${failed[*]}" >&2
        exit 1
    fi

# Build every module. Carries on past a failure so a sweep shows all of it.
build-all:
    #!/usr/bin/env bash
    set -uo pipefail
    failed=()
    for dir in $(just modules); do
        printf '%-38s ' "${dir#"${PWD}"/}"
        if out=$(go -C "$dir" build ./... 2>&1); then
            echo OK
        else
            echo FAIL
            echo "$out" | sed 's/^/    /'
            failed+=("${dir#"${PWD}"/}")
        fi
    done
    if [ ${#failed[@]} -gt 0 ]; then
        echo
        echo "build failed in: ${failed[*]}" >&2
        exit 1
    fi

# Run this before you commit/push.
pre-commit:
    just tidy
    rg -q "// TODO" **/*.go || exit 0
    just lint
    just test

push: pre-commit
    git push

[private]
log-run logfile="" cmd="":
    @(time {{ cmd }}) 2>&1 | tee -a {{ logfile }} || EXIT_CODE=$?; \
    echo -e "\n\nCMD: {{ cmd }}\nLog: {{ logfile }}" | tee -a {{ logfile }}; \
    exit ${EXIT_CODE:-0}
