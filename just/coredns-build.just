# Building and running the coredns binary locally.

# Build and install the coredns binary with this repo's plugins registered.
# Installed to GOBIN: the root holds a coredns/ source directory, so there is no
# unambiguous place to drop a binary of that name beside it.
coredns-build:
    go -C cmd/coredns install .

# Run the ServeDNS benchmarks. No -race: that would measure the race detector.
# Two modules since the restructure - ecs_view is ours, kubernetes the yardstick.
coredns-bench module="benchmarks/coredns/ecs_view" *args:
    go -C {{ module }} test -run '^$' -bench . -benchmem ./... "${@:2}"

# Build and start coredns in the background. Pid in work/coredns.pid, log in work/coredns.log.
coredns-start conf="Corefile" port="1053": coredns-build
    #!/usr/bin/env bash
    set -euo pipefail
    mkdir -p work
    if [ -s work/coredns.pid ] && kill -0 "$(cat work/coredns.pid)" 2>/dev/null; then
        echo "already running as pid $(cat work/coredns.pid), run 'just coredns-stop' first" >&2
        exit 1
    fi
    rm -f work/coredns.pid
    # GOBIN is usually empty, in which case go installs into GOPATH/bin.
    bin="$(go env GOBIN)"
    [ -n "$bin" ] || bin="$(go env GOPATH)/bin"
    args=(-dns.port '{{ port }}' -pidfile work/coredns.pid)
    if [ -f '{{ conf }}' ]; then
        args+=(-conf '{{ conf }}')
    else
        echo "no {{ conf }} found, using CoreDNS's built-in default (whoami)"
    fi
    nohup "$bin/coredns" "${args[@]}" > work/coredns.log 2>&1 &
    for _ in $(seq 50); do
        if [ -s work/coredns.pid ]; then break; fi
        sleep 0.1
    done
    pid=$(cat work/coredns.pid 2>/dev/null || true)
    if [ -z "$pid" ] || ! kill -0 "$pid" 2>/dev/null; then
        echo "coredns failed to start:" >&2
        cat work/coredns.log >&2
        rm -f work/coredns.pid
        exit 1
    fi
    echo "coredns running as pid $pid on port {{ port }}, log: work/coredns.log"

# Stop the coredns started by `just coredns-start`.
coredns-stop:
    #!/usr/bin/env bash
    set -euo pipefail
    if [ ! -s work/coredns.pid ]; then
        echo "not running: no work/coredns.pid"
        exit 0
    fi
    pid=$(cat work/coredns.pid)
    if ! kill -0 "$pid" 2>/dev/null; then
        echo "stale pidfile for pid $pid, removing"
        rm -f work/coredns.pid
        exit 0
    fi
    kill "$pid"
    for _ in $(seq 50); do
        if ! kill -0 "$pid" 2>/dev/null; then break; fi
        sleep 0.1
    done
    if kill -0 "$pid" 2>/dev/null; then
        kill -9 "$pid"
        echo "coredns pid $pid did not exit on SIGTERM, killed"
    else
        echo "stopped coredns pid $pid"
    fi
    rm -f work/coredns.pid
