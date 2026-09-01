# Building and running incus-compose and its ic-healthd sidecar.

# Build a dev binary
build: _update-healthd _build-sleep _build-dns
    #!/usr/bin/env bash
    set -euo pipefail

    if [[ ! -f .env ]]; then
      cp .env.sample .env
    fi

    VERSION=$(source .env; echo "${VERSION:-}")
    if [[ -z ${VERSION} ]]; then
        VERSION="`git describe --tags --always --long --dirty="-dirty"`"
        sed -i -e 's|export VERSION=".*"|export VERSION="'${VERSION}'"|g' .env
    fi

    IC_SUBNET=$(source .env; echo "${IC_SUBNET:-}")
    if [[ -z "${IC_SUBNET:-}" ]]; then
      hash=$(printf '%s' "${PWD}" | cksum | cut -d' ' -f1)
      IC_SUBNET="10.$(( (hash / 256) % 256 )).$(( hash % 256 ))"
      sed -i -e 's|export IC_SUBNET=".*"|export IC_SUBNET="'${IC_SUBNET}'"|g' .env
    fi

    go install -C ./cmd/incus-compose -ldflags="-X github.com/lxc/incus-compose/cmd/incus-compose/version.Version=v${VERSION#v}"

# Usage: just run -f test/fixtures/simple/compose.yaml config
run *args:
    @go run -ldflags "-X github.com/lxc/incus-compose/cmd/incus-compose/version.Version=v${VERSION#v}" ./cmd/incus-compose {{ args }}

[private]
_build-healthd:
    #!/usr/bin/env bash
    set -euo pipefail

    echo "Building healthd for the 'default' cache on '${INCUS_REMOTE}'"
    just run -f container/ic-healthd/compose.yaml build --os-env # os-env cause of VERSION

    if [[ ${INCUS_COMPOSE_IMAGE_CACHE:-} != "incus-compose-tests-cache" ]]; then
      echo "Building healthd for the 'incus-compose-tests-cache' cache on '${INCUS_REMOTE}'"
      just run --image-cache="incus-compose-tests-cache" -f container/ic-healthd/compose.yaml build --os-env
    fi

[private]
_build-sleep:
    #!/usr/bin/env bash
    set -euo pipefail

    echo "Building ic-sleep for the 'default' cache on '${INCUS_REMOTE}'"
    just run -f container/ic-sleep/compose.yaml build --os-env # os-env cause of VERSION

    if [[ ${INCUS_COMPOSE_IMAGE_CACHE:-} != "incus-compose-tests-cache" ]]; then
      echo "Building ic-sleep for the 'incus-compose-tests-cache' cache on '${INCUS_REMOTE}'"
      just run --image-cache="incus-compose-tests-cache" -f container/ic-sleep/compose.yaml build --os-env
    fi

[private]
_build-dns:
    #!/usr/bin/env bash
    set -euo pipefail

    echo "Building ic-dns for the 'default' cache on '${INCUS_REMOTE}'"
    just run -f container/ic-dns/compose-build.yaml build --os-env # os-env cause of VERSION

    if [[ ${INCUS_COMPOSE_IMAGE_CACHE:-} != "incus-compose-tests-cache" ]]; then
      echo "Building ic-dns for the 'incus-compose-tests-cache' cache on '${INCUS_REMOTE}'"
      just run --image-cache="incus-compose-tests-cache" -f container/ic-dns/compose-build.yaml build --os-env
    fi

[private]
_update-healthd *args="--trace": _build-healthd
    #!/usr/bin/env bash
    set -euo pipefail

    remote="${INCUS_REMOTE:-local}"

    echo "Deleting the global ic-healthd on remote '${remote}'"
    echo "yes" | incus project rm --force "incus-compose" || true

    # New image
    export INCUS_COMPOSE_HEALTHD_IMAGE=$(source .env; echo "ghcr.io/lxc/incus-compose/ic-healthd:${VERSION}")
    echo "Image ${INCUS_COMPOSE_HEALTHD_IMAGE}"

    # The healthd-scope project is left behind as the handle for `healthd logs`.
    just run healthd up {{ args }}

# Build ic-healthd container image
release-healthd tag="ghcr.io/lxc/incus-compose/ic-healthd:latest":
    #!/usr/bin/env bash
    set -euo pipefail

    VERSION="`git describe --tags --always --long --dirty="-dirty"`-`openssl rand -hex 4`"

    # New image
    podman build --tag "{{ tag }}" --build-arg VERSION="${VERSION}" -f ./cmd/ic-healthd/Dockerfile .
    echo "Image ${INCUS_COMPOSE_HEALTHD_IMAGE}"

    echo "${GITHUB_TOKEN}" | podman login --username "${GITHUB_USERNAME}" --password-stdin ghcr.io
    podman push "{{ tag }}"
