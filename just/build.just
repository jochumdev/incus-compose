# Building and running incus-compose and its ic-healthd sidecar.

# Build a dev binary
build: update-healthd
    #!/usr/bin/env bash
    set -euo pipefail

    image=$(source .env; echo "${INCUS_COMPOSE_HEALTHD_IMAGE:-}")
    version="${image##*:}"

    if [[ "${version}" == "${image}" ]]; then
        version="`git describe --tags --always --long --dirty="-dirty"`"
    fi

    go install -C ./cmd/incus-compose -ldflags="-X github.com/lxc/incus-compose/cmd/incus-compose/version.Version=v${version#v}"

# Usage: just run -f test/fixtures/simple/compose.yaml config
run *args:
    @go run ./cmd/incus-compose {{ args }}

# Build ic-healthd binary
build-healthd:
    CGO_ENABLED=0 go build -tags=netgo -ldflags="-w -s -X github.com/lxc/incus-compose/cmd/ic-healthd/version.Version=`git describe --tags --always --long --dirty="-dirty"`" -trimpath -o bin/ic-healthd ./cmd/ic-healthd

# Build ic-healthd container image
build-healthd-image tag_base="ghcr.io/lxc/incus-compose/ic-healthd":
    #!/usr/bin/env bash
    set -euo pipefail

    if [[ ! -f .env ]]; then
      cp .env.sample .env
    fi

    # The random suffix gives a clear sign that your version is running.
    VERSION="`git describe --tags --always --long --dirty="-dirty"`-`openssl rand -hex 4`"
    # Container tags carry no v prefix, `release` pushes them without one too.
    export VERSION="${VERSION#v}"
    echo ${VERSION}

    echo "Building for the 'default' cache on '${INCUS_REMOTE}'"
    just run -P cmd/ic-healthd build --os-env # os-env cause of VERSION

    if [[ ${INCUS_COMPOSE_IMAGE_CACHE:-} != "incus-compose-tests-cache" ]]; then
      echo "Building for the 'incus-compose-tests-cache' cache on '${INCUS_REMOTE}'"
      just run --image-cache="incus-compose-tests-cache" -P cmd/ic-healthd build --os-env
    fi

    sed -i -e 's|export INCUS_COMPOSE_HEALTHD_IMAGE=".*"|export INCUS_COMPOSE_HEALTHD_IMAGE="{{ tag_base }}:'${VERSION}'"|g' .env

# Rebuild the ic-healthd image and put the shared daemon on it
update-healthd *args="--trace": build-healthd-image
    #!/usr/bin/env bash
    set -euo pipefail

    remote="${INCUS_REMOTE:-local}"

    echo "Deleting the global ic-healthd on remote '${remote}'"
    echo "yes" | incus project rm --force "incus-compose" || true

    # New image
    export INCUS_COMPOSE_HEALTHD_IMAGE=$(source .env; echo "$INCUS_COMPOSE_HEALTHD_IMAGE")
    echo "Image ${INCUS_COMPOSE_HEALTHD_IMAGE}"

    # The healthd-scope project is left behind as the handle for `healthd logs`.
    just run healthd up {{ args }}

# Build ic-healthd container image
release-healthd-image tag="ghcr.io/lxc/incus-compose/ic-healthd:latest":
    #!/usr/bin/env bash
    set -euo pipefail

    VERSION="`git describe --tags --always --long --dirty="-dirty"`-`openssl rand -hex 4`"

    # New image
    podman build --tag "{{ tag }}" --build-arg VERSION="${VERSION}" -f ./cmd/ic-healthd/Dockerfile .
    echo "Image ${INCUS_COMPOSE_HEALTHD_IMAGE}"

    echo "${GITHUB_TOKEN}" | podman login --username "${GITHUB_USERNAME}" --password-stdin ghcr.io
    podman push "{{ tag }}"

# Run with local healthd binary (for testing without an explicit OCI image) (ex. just run-healthd -f test/healthd/debug/compose.yaml up )
run-healthd compose="examples/immich/compose.yaml" name="immich": build-healthd
    go run ./cmd/incus-compose --debug -f {{ compose }} healthd up --recreate --binary bin/ic-healthd
    go run ./cmd/incus-compose -f {{ compose }} incus exec {{ name }}-ic-healthd -- tail -n 1000 -f /var/log/ic-healthd.log
