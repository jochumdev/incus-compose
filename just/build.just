# Building and running incus-compose and its ic-healthd sidecar.

# Build a dev binary
build: _build-deps
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
_build-deps:
    #!/usr/bin/env bash
    set -euo pipefail

    export PROJECT_ROOT={{ justfile_directory() }}
    export VERSION=$(source .env; echo "${VERSION#v}")

    echo "Building healthd for the 'default' cache on '${INCUS_REMOTE}'"
    just run -f container/build.yaml build --os-env # os-env cause of VERSION

    if [[ ${INCUS_COMPOSE_IMAGE_CACHE:-} != "incus-compose-tests-cache" ]]; then
      echo "Building healthd for the 'incus-compose-tests-cache' cache on '${INCUS_REMOTE}'"
      just run --image-cache="incus-compose-tests-cache" -f container/build.yaml build --os-env
    fi
