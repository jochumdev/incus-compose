# The nested Incus dev environment, and putting it back to a clean state.

# Run commands in the nested incus.
incus *args:
    @echo "Using remote '${INCUS_REMOTE-"local"}': incus $*" >&2
    @incus "$@"

# Dev install creates your dev environment: `just dev-install [container] [listen] [project] [image]`
dev-install container_name="local:ict" listen='127.0.0.1:1443' project='default' image='images:debian/trixie' storagepool='default' repo='stable':
    go install gotest.tools/gotestsum@latest
    @just make-nested "{{ container_name }}" "{{ image }}" "{{ listen }}" "{{ project }}" "{{ storagepool }}" "{{ repo }}"
    just build-healthd-image

[private]
make-nested container='local:ict' image='images:debian/trixie' listen="127.0.0.1:1443" project="default" storagepool="default" repo="stable":
    #!/usr/bin/env bash
    set -euo pipefail

    container="{{ container }}"
    image="{{ image }}"
    listen="{{ listen }}"
    storagepool="{{ storagepool }}"
    repo="{{ repo }}"

    key_file=""
    cert_file=""
    if [[ -f $HOME/.config/incus/client.crt ]] && [[ -f $HOME/.config/incus/client.key ]]; then
        key_file="$HOME/.config/incus/client.key"
        cert_file="$HOME/.config/incus/client.crt"
    fi

    # Run setup script (certificate injection is handled by the script)
    set +e
    echo "Trying to create a nested container:\n"
    INCUS_PROJECT="{{ project }}" ./scripts/setup-nested-incus.sh -c "${cert_file}" -n "${container}" -i "${image}" -r "${repo}" -l "${listen}" -p "${storagepool}"
    set -e

    if [[ -z "${listen}" ]]; then
        container_ip=$(incus list "${container}" -c4 --format json 2>/dev/null | jq -r '.[0].state.network // {} | [ .[].addresses[]? | select(.family == "inet") | .address ] | .[0] // empty' 2>/dev/null)
        if [ -z "${container_ip}" ]; then
            echo "Error: Could not get container IP"
            echo "This scripts requires you to setup the nested incus instance first, use 'just make-nested' to create it."
            exit 1
        fi

        url="https://${container_ip}:8443"
    else
        url="https://${listen}"
    fi

    INCUS_REMOTE="${container%%:*}" incus remote remove "${container##*:}" || true
    incus remote add "${container##*:}" "${url}" --accept-certificate

cleanup:
    just purge-projects
    just purge-networks || true
    sudo systemctl restart incus.service incus.socket

# Purge all dangling networks (managed and 0 users) from the configured remote
purge-networks:
    #!/usr/bin/env bash
    set -euo pipefail

    remote="${INCUS_REMOTE:-local}"
    networks=$(incus network list "${remote}:" -f json | jq -r '.[] | select(.used_by | length == 0) | select(.managed == true) | .name')

    if [[ -z "${networks}" ]]; then
        echo "No dangling networks found."
        exit 1
    fi

    echo "Deleting dangling networks on remote '${remote}':"
    while IFS= read -r network; do
        echo "  Deleting: ${network}"
        incus network delete "${remote}:${network}"
    done <<< "${networks}"
    echo "Done."

# Removes all images
purge-images *args:
    #!/usr/bin/env bash
    set -euo pipefail

    remote="${INCUS_REMOTE:-local}"
    images=$(incus image list "${remote}:" {{ args }} -f json | jq -r '.[] .fingerprint')

    if [[ -z "${images}" ]]; then
        echo "No images found."
        exit 1
    fi

    echo "Deleting images on remote '${remote}':"
    while IFS= read -r image; do
        echo "  Deleting: ${image}"
        incus image delete {{ args }} "${remote}:${image}"
    done <<< "${images}"
    echo "Done."

# Removes all projects
purge-projects:
    #!/usr/bin/env bash
    set -euo pipefail

    remote="${INCUS_REMOTE:-local}"
    projects=$(incus project list "${remote}:" -f json | jq -r '.[] | select(.name != "default" and .name != "incus-compose" and .name != "incus-compose-cache" and .name != "incus-compose-tests-cache") | .name')

    if [[ -z "${projects}" ]]; then
        echo "No projects found."
        exit 1
    fi

    echo "Deleting projects on remote '${remote}':"
    while IFS= read -r project; do
        echo "  Deleting: ${project}"
        echo -e "yes\n" | incus project delete -f "${remote}:${project}"
    done <<< "${projects}"
    echo "Done."

purge-tokens:
    #!/usr/bin/env bash
    set -euo pipefail

    remote="${INCUS_REMOTE:-local}"
    tokens=$(incus config trust list-tokens "${remote}:" -f json | jq -r '.[] .client_name')

    if [[ -z "${tokens}" ]]; then
        echo "No tokens found."
        exit 1
    fi

    echo "Revoking tokens on remote '${remote}':"
    while IFS= read -r token; do
        echo "  Revoking: ${token}"
        incus config trust revoke-token "${remote}:${token}"
    done <<< "${tokens}"
    echo "Done."

# Removes all trusted client certificates, except the one named "client.crt"
purge-certs:
    #!/usr/bin/env bash
    set -euo pipefail

    remote="${INCUS_REMOTE:-local}"
    certs=$(incus config trust list "${remote}:" -f json | jq -r '.[] | select(.name != "client.crt" and .name != "dev01") | .fingerprint')

    if [[ -z "${certs}" ]]; then
        echo "No certificates found."
        exit 1
    fi

    echo "Removing certificates on remote '${remote}':"
    while IFS= read -r cert; do
        echo "  Removing: ${cert}"
        incus config trust remove "${remote}:${cert}"
    done <<< "${certs}"
    echo "Done."
