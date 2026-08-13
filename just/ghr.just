# The self-hosted GitHub Actions runner's nested Incus daemons.
#
# See docs/root/github-runner.md. The certificate is the runner user's own,
# pulled out of the runner container first:
#
#   incus --project=ic-runner file pull \
#     runner/home/runner/.config/incus/client.crt work/runner.crt

# Create the four nested test daemons: `just ghr-create-icts [cert]`
ghr-create-icts cert="work/runner.crt":
    #!/usr/bin/env bash
    set -euo pipefail

    export INCUS_REMOTE="local-https"
    export INCUS_PROJECT="ic-runner"

    extra_opts=""
    if [[ -n ${APT_CACHER_NG:-} ]]; then
      extra_opts="-a ${APT_CACHER_NG}"
    fi

    cert="$(realpath "{{ cert }}")"

    incus storage create tmpfs dir source=/mnt/tmpfs || true

    incus stop runner || true

    echo "Creating: ict-stable" >&2
    ./scripts/setup-nested-incus.sh -c "${cert}" -n ict-stable -r stable -o -s tmpfs -f ${extra_opts}

    echo "Creating: ict-custom" >&2
    ./scripts/setup-nested-incus.sh -c "${cert}" -n ict-custom -r stable -p local -b vmbr0 -o -s tmpfs -f ${extra_opts}

    echo "Creating: ict-lts" >&2
    ./scripts/setup-nested-incus.sh -c "${cert}" -n ict-lts -r lts-7.0 -o -s tmpfs -f ${extra_opts}

    echo "Creating: ict-daily" >&2
    ./scripts/setup-nested-incus.sh -c "${cert}" -n ict-daily -r daily -o -s tmpfs -f ${extra_opts}

    incus start runner || true

ghr-remove-icts:
    #!/usr/bin/env bash
    set -euo pipefail

    export INCUS_PROJECT="ic-runner"

    declare -a RUNNERS
    RUNNERS=( "ict-stable" "ict-custom" "ict-lts" "ict-daily" )

    for r in "${RUNNERS[@]}"; do
     	echo "Removing: ${r}" >&2
     	INCUS_REMOTE=local incus rm -f "${r}" || true
    done

    incus storage rm tmpfs || true
