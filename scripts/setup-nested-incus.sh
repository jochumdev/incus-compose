#!/usr/bin/env bash
set -euo pipefail
# Copyright (c) 2025 René Jochum <rene@jochum.dev>
# This script is released into the public domain or under CC0-1.0.
# Use it however you want, no restrictions.

# Required: path to client certificate to inject
CLIENT_CERT=""

# Setup a nested Incus container for testing incus-compose
# This creates an isolated Incus instance accessible via HTTPS

# Default values
CONTAINER_NAME="incus-compose-test"
IMAGE="images:debian/trixie"
INCUS_REPO="stable" # stable, lts-6.0, lts-7.0, daily
FORCE="false"
STORAGE_POOL="default"
HOST_STORAGE_POOL=""
BRIDGE="incusbr0"
LISTEN=""
OVN="false"

# Track whether we created the container so we can cleanup on failure if desired
CONTAINER_CREATED="false"

cleanup() {
    local rc=$?
    if [[ $rc != 0 ]] && [[ "${CONTAINER_CREATED}" == "true" || "${FORCE}" == "true" ]]; then
        echo "Cleaning up created container ${CONTAINER_NAME} due to error (exit ${rc})..."
        incus delete --force "${CONTAINER_NAME}" >/dev/null 2>&1 || true
    fi
    return $rc
}
trap cleanup EXIT

# Usage information
usage() {
    cat <<EOF
Usage: $(basename "$0") -c CERT [OPTIONS]

Setup a nested Incus container for testing incus-compose.

REQUIRED:
-c CERT         Path to client certificate to inject into trust store

OPTIONS:
-n NAME         Container name (default: ${CONTAINER_NAME})
                Note: Dots will be replaced with hyphens (DNS-safe)
-i IMAGE        Base image (default: ${IMAGE})
-r REPO         Incus repository: stable, lts-6.0, lts-7.0, daily (default: ${INCUS_REPO})
-l ADDRESS      Add port proxy (example: 127.0.0.1:2443) (default: "")
-p POOL         Storage pool to create (default: ${STORAGE_POOL})
-s POOL         Storage pool on the host to put the container itself on
                (default: whatever the host's default profile uses)
-b BRIDGE       Bridge to create (default: ${BRIDGE})
-o              Install OVN and configure it (default: false)
-f              Force delete any existing container (default: false)
-h              Show this help message

EXAMPLES:
# Create with defaults (stable version)
$(basename "$0") -c test/certs/incus-compose-test.crt

# Create with LTS version
$(basename "$0") -c test/certs/incus-compose-test.crt -r lts

# Create with custom name
$(basename "$0") -c test/certs/my-test.crt -n my-test -r lts

EOF
    exit 0
}

# Parse arguments
while getopts "c:n:i:r:l:p:s:b:ofh" opt; do
    case ${opt} in
    c)
        CLIENT_CERT="${OPTARG}"
        ;;
    n)
        CONTAINER_NAME="${OPTARG}"
        ;;
    i)
        IMAGE="${OPTARG}"
        ;;
    r)
        INCUS_REPO="${OPTARG}"
        ;;
    l)
        LISTEN="${OPTARG}"
        ;;
    p)
        STORAGE_POOL="${OPTARG}"
        ;;
    s)
        HOST_STORAGE_POOL="${OPTARG}"
        ;;
    b)
        BRIDGE="${OPTARG}"
        ;;
    o)
        OVN="true"
        ;;
    f)
        FORCE="true"
        ;;
    h)
        usage
        ;;
    \?)
        echo "Invalid option: -${opt}" >&2
        echo "Use -h for help" >&2
        exit 1
        ;;
    :)
        echo "Option -${OPTARG} requires an argument" >&2
        exit 1
        ;;
    esac
done

# Validate required arguments
if [[ -z "${CLIENT_CERT}" ]]; then
    echo "Error: Client certificate (-c) is required" >&2
    echo "Use -h for help" >&2
    exit 1
fi

if [[ ! -f "${CLIENT_CERT}" ]]; then
    echo "Error: Certificate file not found: ${CLIENT_CERT}" >&2
    exit 1
fi

if [[ $OVN == "true" ]]; then
  set +euo pipefail
  if ! lsmod | grep -q "openvswitch"; then
    echo "Error: The openvswitch kernel module is not loaded" >&2
    exit 1
  fi
  set -euo pipefail
fi

# Sanitize container name to be DNS-safe
CONTAINER_NAME="${CONTAINER_NAME//./-}"

shift $((OPTIND - 1))

# Validate repository selection early
case "${INCUS_REPO}" in
stable)
    REPO_URL="https://pkgs.zabbly.com/incus/stable"
    ;;
lts-6.0)
    REPO_URL="https://pkgs.zabbly.com/incus/lts-6.0"
    ;;
lts-7.0)
    REPO_URL="https://pkgs.zabbly.com/incus/lts-7.0"
    ;;
daily)
    REPO_URL="https://pkgs.zabbly.com/incus/daily"
    ;;
*)
    echo "Error: Unknown repository '${INCUS_REPO}'" >&2
    echo "Valid options: stable, lts-6.0, lts-7.0, daily" >&2
    exit 1
    ;;
esac

# Ensure incus CLI is available
if ! command -v incus >/dev/null 2>&1; then
    echo "Error: 'incus' CLI not found in PATH. Please install/incus or adjust PATH." >&2
    exit 1
fi

echo "==> Configuration:"
echo "    Container name: ${CONTAINER_NAME}"
echo "    Base image: ${IMAGE}"
echo "    Proxy listen: ${LISTEN}"
echo "    Incus repository: ${INCUS_REPO}"
echo "    Repository URL: ${REPO_URL}"
echo "    Client certificate: ${CLIENT_CERT}"
echo "    Storage pool: ${STORAGE_POOL}"
echo "    Host storage pool: ${HOST_STORAGE_POOL:-<host default>}"
echo "    OVN: ${OVN}"
echo ""

if incus info "${CONTAINER_NAME}" >/dev/null 2>&1; then
    if [[ $FORCE == "true" ]]; then
        echo "Deleting existing container ${CONTAINER_NAME} (force)"
        incus delete --force "${CONTAINER_NAME}"
    else
        echo "Error: Container ${CONTAINER_NAME} already exists."
        echo "Delete it first with: incus delete -f ${CONTAINER_NAME}"
        exit 1
    fi
fi

echo "==> Creating nested Incus container: ${CONTAINER_NAME}"

# Create container with nesting enabled
launch_opts=()
if [[ -n "${HOST_STORAGE_POOL}" ]]; then
    launch_opts=(--storage "${HOST_STORAGE_POOL}")
fi

incus launch "${IMAGE}" "${CONTAINER_NAME}" \
    -c security.nesting=true \
    -c security.privileged=true \
    "${launch_opts[@]}"

CONTAINER_CREATED="true"

INSTALL_SCRIPT=$(
    cat <<'EOF'
#!/bin/bash
set -euo pipefail

export DEBIAN_FRONTEND=noninteractive

echo "Installing prerequisites..."
apt-get update -q
apt-get install -y -q curl gpg ca-certificates

echo "Adding Incus repository..."
mkdir -p /etc/apt/keyrings/
curl -fsSL https://pkgs.zabbly.com/key.asc -o /etc/apt/keyrings/zabbly.asc

cat > /etc/apt/sources.list.d/zabbly-incus.sources <<SOURCES_EOF
Enabled: yes
Types: deb
URIs: REPO_URL_PLACEHOLDER
Suites: $(. /etc/os-release && echo ${VERSION_CODENAME})
Components: main
Architectures: $(dpkg --print-architecture)
Signed-By: /etc/apt/keyrings/zabbly.asc
SOURCES_EOF

echo "Installing Incus..."
apt-get update -q
apt-get install -y -q incus-base pigz

# Disable AppArmor in nested environment to prevent conflicts with container security profiles.
# This is safe for development VMs but should NEVER be done on production hosts.
echo "Disabling AppArmor to avoid kernel compatibility issues..."
systemctl stop apparmor || true
systemctl disable apparmor || true
systemctl mask apparmor || true

echo "Incus installed successfully!"
EOF
)

echo "==> Executing installation script"
# Keep your variable-based pipe approach; replace placeholder and stream into container
echo "${INSTALL_SCRIPT}" | sed "s|REPO_URL_PLACEHOLDER|${REPO_URL}|g" | incus exec "${CONTAINER_NAME}" -- bash -s

if [[ $OVN == "true" ]]; then
  echo "==> Installing OVN"
  incus exec "${CONTAINER_NAME}" -- apt-get install -qy ovn-host ovn-central
  incus exec "${CONTAINER_NAME}" -- ovs-vsctl set open_vswitch . \
     external_ids:ovn-remote=unix:/run/ovn/ovnsb_db.sock \
     external_ids:ovn-encap-type=geneve \
     external_ids:ovn-encap-ip=127.0.0.1
fi

echo "==> Executing Incus init script"

CONFIGURE_SCRIPT=$(
    cat <<'EOF'
#!/bin/bash
set -euo pipefail

echo "Starting Incus daemon..."
systemctl enable --now incus.socket || true

echo "Waiting for Incus to be ready..."
# incus admin waitready exists on newer installs; fall back to a small loop if necessary.
if incus admin waitready --timeout=60 >/dev/null 2>&1; then
    echo "Incus admin reports ready"
else
    echo "Waiting for Incus socket by polling..."
    for i in {1..30}; do
        if incus info >/dev/null 2>&1; then
            break
        fi
        sleep 1
    done
fi

echo "Initializing Incus..."
cat <<PRESEED_EOF | incus admin init --preseed
config:
  core.https_address: "[::]:8443"
networks:
- name: __BRIDGE__
  type: bridge
  config:
    ipv4.address: 10.183.0.1/23
    ipv6.address: none
    ipv4.dhcp.ranges: 10.183.0.65-10.183.0.254
    ipv4.ovn.ranges: 10.183.1.1-10.183.1.254
storage_pools:
- name: __STORAGE_POOL__
  driver: dir
profiles:
- name: default
  devices:
    root:
      path: /
      pool: default
      type: disk
    eth0:
      name: eth0
      network: __BRIDGE__
      type: nic
PRESEED_EOF

EOF
)

CONFIGURE_SCRIPT="$(echo "${CONFIGURE_SCRIPT}" | sed -e 's/__STORAGE_POOL__/'"${STORAGE_POOL}"'/g' -e 's/__BRIDGE__/'"${BRIDGE}"'/g')"

# Stream the configure script as well (no temp files)
echo "${CONFIGURE_SCRIPT}" | incus exec "${CONTAINER_NAME}" -- bash -s

if [[ $OVN == "true" ]]; then
    echo "==> Seeding a permanent OVN network for fast capability detection"
    incus exec "${CONTAINER_NAME}" -- incus network create ic-ovn-seed --type=ovn network=none
fi

if [[ $STORAGE_POOL != "default" ]]; then
  echo "==> Setting the default profiles root device storage pool"
  incus exec "${CONTAINER_NAME}" -- incus profile device set default root "pool=${STORAGE_POOL}"
fi

# Inject client certificate into trust store
echo "==> Adding client certificate to nested Incus trust store"
incus file push -- "${CLIENT_CERT}" "${CONTAINER_NAME}/root/client.crt"
incus exec "${CONTAINER_NAME}" -- incus config trust add-certificate /root/client.crt --restricted=false
incus exec "${CONTAINER_NAME}" -- rm -f /root/client.crt
echo "    Certificate added with unrestricted access"
echo ""

nested_address="$(incus exec "${CONTAINER_NAME}" -- incus config get core.https_address || true)"
if [[ -n "${LISTEN}" ]]; then
    nested_port="${nested_address//[^0-9]/}"
    echo ""
    echo ""
    echo "==> Adding a a proxy device to the nested container ${LISTEN} -> 12.7.0.0.1:${nested_port}"

    set +e
    incus config device add "${CONTAINER_NAME}" "proxy-${LISTEN}" proxy listen="tcp:${LISTEN}" connect="tcp:127.0.0.1:${nested_port}"
    set -e
fi

echo ""
echo ""
echo "==> Nested container info:"
incus exec "${CONTAINER_NAME}" -- incus info
