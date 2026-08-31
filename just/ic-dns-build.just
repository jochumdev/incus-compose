# Building and running the ic-dns binary locally.

# Build and install the ic-dns binary with this repo's plugins registered.
# Installed to GOBIN: nothing in the tree is an unambiguous place for a binary.
ic-dns-build:
    go -C cmd/ic-dns install .

# Run the ServeDNS benchmarks. No -race: that would measure the race detector.
# Two modules since the restructure - ecs_view is ours, kubernetes the yardstick.
ic-dns-bench module="benchmarks/ic-dns/ecs_view" *args:
    go -C {{ module }} test -run '^$' -bench . -benchmem ./... "${@:2}"

# Build and start ic-dns in the background. Pid in work/ic-dns.pid, log in work/ic-dns.log.
ic-dns-start conf="Corefile" port="1053": ic-dns-build
    #!/usr/bin/env bash
    set -euo pipefail
    mkdir -p work
    if [ -s work/ic-dns.pid ] && kill -0 "$(cat work/ic-dns.pid)" 2>/dev/null; then
        echo "already running as pid $(cat work/ic-dns.pid), run 'just ic-dns-stop' first" >&2
        exit 1
    fi
    rm -f work/ic-dns.pid
    # GOBIN is usually empty, in which case go installs into GOPATH/bin.
    bin="$(go env GOBIN)"
    [ -n "$bin" ] || bin="$(go env GOPATH)/bin"
    args=(-dns.port '{{ port }}' -pidfile work/ic-dns.pid)
    if [ -f '{{ conf }}' ]; then
        args+=(-conf '{{ conf }}')
    else
        echo "no {{ conf }} found, using the built-in default (whoami)"
    fi
    nohup "$bin/ic-dns" "${args[@]}" > work/ic-dns.log 2>&1 &
    for _ in $(seq 50); do
        if [ -s work/ic-dns.pid ]; then break; fi
        sleep 0.1
    done
    pid=$(cat work/ic-dns.pid 2>/dev/null || true)
    if [ -z "$pid" ] || ! kill -0 "$pid" 2>/dev/null; then
        echo "ic-dns failed to start:" >&2
        cat work/ic-dns.log >&2
        rm -f work/ic-dns.pid
        exit 1
    fi
    echo "ic-dns running as pid $pid on port {{ port }}, log: work/ic-dns.log"

# Stop the ic-dns started by `just ic-dns-start`.
ic-dns-stop:
    #!/usr/bin/env bash
    set -euo pipefail
    if [ ! -s work/ic-dns.pid ]; then
        echo "not running: no work/ic-dns.pid"
        exit 0
    fi
    pid=$(cat work/ic-dns.pid)
    if ! kill -0 "$pid" 2>/dev/null; then
        echo "stale pidfile for pid $pid, removing"
        rm -f work/ic-dns.pid
        exit 0
    fi
    kill "$pid"
    for _ in $(seq 50); do
        if ! kill -0 "$pid" 2>/dev/null; then break; fi
        sleep 0.1
    done
    if kill -0 "$pid" 2>/dev/null; then
        kill -9 "$pid"
        echo "ic-dns pid $pid did not exit on SIGTERM, killed"
    else
        echo "stopped ic-dns pid $pid"
    fi
    rm -f work/ic-dns.pid
