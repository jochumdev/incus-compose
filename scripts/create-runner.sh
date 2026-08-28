#!/usr/bin/env bash
set -euo pipefail
# Copyright (c) 2026 René Jochum <rene@jochum.dev>
# This script is released into the public domain or under CC0-1.0.
# Use it however you want, no restrictions.

# Rebuilds the ic-runner project and its "runner" container from nothing, the
# steps of docs/root/developer/github-runner.md as far as they go without a
# GitHub registration token.
#
# The GitHub Actions runner is unpacked into ~/gh01 and copied to gh02, gh03 and
# gh04. Registering and starting them needs a token, so that is left to do by
# hand; the last step prints the commands.
#
# Afterwards work/runner.crt holds the runner's client certificate, which is
# what runner.sh wants to bring the nested daemons up again.

if [[ -f .env ]]; then
    # shellcheck source=/dev/null
    source .env
fi

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

RUNNER="${RUNNER:-runner}"
PROJECT="${RUNNER_INCUS_PROJECT:-ic-runner}"
IMAGE="${RUNNER_IMAGE:-images:debian/trixie}"
BRIDGE="${RUNNER_BRIDGE:-icrunner0}"
CERT="${RUNNER_CERT:-work/runner.crt}"

# The pool the project's default profile puts a root disk on.
ROOT_POOL="${RUNNER_ROOT_POOL:-default}"

# The pool the runner itself lands on, empty for whatever the profile says.
POOL="${RUNNER_POOL:-}"

# The domain the OCI mirrors live under. Without it there are no mirror remotes.
REGISTRY_DOMAIN="${RUNNER_REGISTRY_DOMAIN:-}"

# The apt-cacher-ng the container's apt goes through, a host or a full URL.
APT_CACHER="${APT_CACHER_NG:-}"
APT_CACHER_PORT="${APT_CACHER_PORT:-3142}"
APT_PROXY=""

GH_RUNNER_VERSION="${GH_RUNNER_VERSION:-2.335.1}"
GH_RUNNER_URL="https://github.com/actions/runner/releases/download/v${GH_RUNNER_VERSION}/actions-runner-linux-x64-${GH_RUNNER_VERSION}.tar.gz"

# One directory per runner. The first is the one that is downloaded, the rest
# are copies of it, so they only differ once each is registered.
GH_RUNNERS=(gh01 gh02 gh03 gh04)

# What every runner's .env carries, the parallelism the test suite runs at.
GH_TEST_PROCS="${GH_TEST_PROCS:-12}"

step() { echo "==> $*" >&2; }
warn() { echo "!!  $*" >&2; }
die() {
    echo "!!  $*" >&2
    exit 1
}

usage() {
    cat <<EOF
Usage: $(basename "$0") [-h]

Rebuild the ${PROJECT} project and the ${RUNNER} container from scratch.

Environment:
  RUNNER                  container name (default: ${RUNNER})
  RUNNER_INCUS_PROJECT    project to build in (default: ${PROJECT})
  RUNNER_IMAGE            base image (default: ${IMAGE})
  RUNNER_BRIDGE           bridge the container gets a nic on (default: ${BRIDGE})
  RUNNER_ROOT_POOL        pool for the project profile's root disk (default: ${ROOT_POOL})
  RUNNER_POOL             pool for the runner itself (default: the profile's)
  RUNNER_CERT             where to leave the client certificate (default: ${CERT})
  RUNNER_REGISTRY_DOMAIN  domain of the OCI mirrors, unset skips them
  GH_RUNNER_VERSION       actions/runner release (default: ${GH_RUNNER_VERSION})
  GH_TEST_PROCS           TEST_PROCS in each runner's .env (default: ${GH_TEST_PROCS})
  APT_CACHER_NG           apt-cacher-ng host or URL (default: ${APT_CACHER:-<none>},
                          assumed port: ${APT_CACHER_PORT})
  APT_CACHER_PORT         port to assume (default: ${APT_CACHER_PORT})
  SKIP_ICTS=1             leave the nested daemons to runner.sh
EOF
}

if [[ "${1:-}" == "-h" || "${1:-}" == "--help" ]]; then
    usage
    exit 0
fi

# Normalise the cacher into a URL, the rules setup-nested-incus.sh uses: a bare
# host gets a scheme, a portless one gets the default port.
if [[ -n "${APT_CACHER}" ]]; then
    APT_PROXY="${APT_CACHER%/}"

    if [[ "${APT_PROXY}" != http://* && "${APT_PROXY}" != https://* ]]; then
        APT_PROXY="http://${APT_PROXY}"
    fi

    if [[ ! "${APT_PROXY}" =~ :[0-9]+$ ]]; then
        APT_PROXY="${APT_PROXY}:${APT_CACHER_PORT}"
    fi
fi

cd "${SCRIPT_DIR}/.."

# inc runs an incus command against the runner's project.
inc() { incus --project="${PROJECT}" "$@"; }

# as_root runs a command inside the container, as root.
as_root() { inc exec "${RUNNER}" -- "$@"; }

# as_runner runs a shell command inside the container as the runner user. It is
# a login shell, so ~/.profile has already put ~/.local/bin and ~/go/bin on PATH.
#
# The cd is not optional: exec lands in /root, a login shell does not leave it,
# and the runner user cannot stat it, which go reports as "cannot determine
# current directory".
as_runner() { inc exec "${RUNNER}" -- sudo -u runner -H bash -lc "cd \"\${HOME}\" && $*"; }

# --- 1. The openvswitch module ----------------------------------------------

load_openvswitch() {
    if lsmod | grep -q '^openvswitch'; then
        step "openvswitch is already loaded"
        return
    fi

    step "Loading openvswitch, ovn needs it"
    sudo bash -c "echo 'openvswitch' > /etc/modules-load.d/50-openvswitch.conf"
    sudo modprobe openvswitch
}

# --- 2. The container -------------------------------------------------------

create_container() {
    # Every step below is safe to repeat, so an existing container is resumed
    # rather than refused. Delete it first for a clean rebuild.
    if inc info "${RUNNER}" >/dev/null 2>&1; then
        step "${RUNNER} already exists, picking up where the last run stopped"
        return
    fi

    step "Creating the project ${PROJECT}"
    incus project create "${PROJECT}" || true

    step "Giving its default profile a root disk and a nic"
    inc profile device add default root disk path=/ pool="${ROOT_POOL}" || true
    inc profile device add default eth0 nic network="${BRIDGE}" || true

    # security.privileged is what makes podman builds work, nothing else.
    local -a launch=(
        incus --project="${PROJECT}" launch "${IMAGE}" "${RUNNER}"
        -c security.nesting=true -c security.privileged=true
    )

    if [[ -n "${POOL}" ]]; then
        launch+=(--storage "${POOL}")
    fi

    step "Launching ${RUNNER} from ${IMAGE}"
    "${launch[@]}"

    step "Waiting for its network"
    for _ in {1..60}; do
        if as_root getent hosts deb.debian.org >/dev/null 2>&1; then
            return
        fi

        sleep 1
    done

    warn "${RUNNER} still has no DNS, the package steps are about to fail"
}

# --- 3. Base packages -------------------------------------------------------

# configure_apt_proxy points the container's apt at the cacher. The cacher's own
# host is an exception: the Zabbly URI below is fetched through it, and without
# the exception apt asks the cacher to proxy to itself, which answers "503 Host
# not found".
configure_apt_proxy() {
    if [[ -z "${APT_PROXY}" ]]; then
        return
    fi

    local proxy_host="${APT_PROXY#*://}"
    proxy_host="${proxy_host%%:*}"

    step "Pointing apt at ${APT_PROXY}"
    as_root bash -c "cat > /etc/apt/apt.conf.d/00proxy <<EOF
Acquire::http::Proxy \"${APT_PROXY}\";
Acquire::http::Proxy::${proxy_host} \"DIRECT\";
EOF"
}

install_base() {
    configure_apt_proxy

    step "Installing the base packages"
    as_root apt-get -q update

    # curl and ca-certificates are not in the guide's list, but every step below
    # this one fetches something over HTTPS.
    as_root env DEBIAN_FRONTEND=noninteractive apt-get install -qy \
        sudo sudo-rs vim golang git shellcheck podman jq curl ca-certificates

    as_root ln -sf /usr/sbin/sudo-rs /usr/local/sbin/sudo
    as_root ln -sf /usr/share/zoneinfo/Europe/Vienna /etc/timezone
}

# --- 4. The Incus client ----------------------------------------------------

install_incus_client() {
    step "Adding the Zabbly repository"
    as_root mkdir -p /etc/apt/keyrings

    # The key is small and signed, so it goes direct rather than through the cacher.
    as_root curl -fsSL https://pkgs.zabbly.com/key.asc -o /etc/apt/keyrings/zabbly.asc

    local repo_url="https://pkgs.zabbly.com/incus/stable"

    if [[ -n "${APT_PROXY}" ]]; then
        # apt-cacher-ng's remap: it fetches, and caches, the https repo for us.
        repo_url="${APT_PROXY}/HTTPS///${repo_url#https://}"
    fi

    # The URI is ours, the suite and architecture are the container's.
    as_root bash -c "cat <<EOF > /etc/apt/sources.list.d/zabbly-incus-stable.sources
Enabled: yes
Types: deb
URIs: ${repo_url}
Suites: \$(. /etc/os-release && echo \${VERSION_CODENAME})
Components: main
Architectures: \$(dpkg --print-architecture)
Signed-By: /etc/apt/keyrings/zabbly.asc

EOF"

    step "Installing incus-client"
    as_root apt-get -q update

    # apt-get update only warns when a repository fails and Debian ships an Incus
    # of its own, so an unreachable Zabbly quietly installs the wrong one.
    as_root bash -c 'apt-cache policy incus-client | grep -q zabbly' ||
        die "the Zabbly repository is not usable, incus-client would come from Debian instead"

    as_root env DEBIAN_FRONTEND=noninteractive apt-get install -qy incus-client
}

# --- 5. The runner user -----------------------------------------------------

create_user() {
    if as_root id runner >/dev/null 2>&1; then
        step "The runner user already exists"
        return
    fi

    step "Creating the runner user"
    as_root adduser --disabled-password --gecos "" --shell /usr/bin/bash runner
}

# --- 6+7. The runner user's tools -------------------------------------------

install_tools() {
    # ~/.local/bin has to exist at login or Debian's ~/.profile leaves it off
    # PATH, so it is created before anything logs in.
    step "Installing golangci-lint"
    # shellcheck disable=SC2016 # $HOME is the runner's, expanded in the container
    as_root sudo -u runner -H bash -c \
        'cd "${HOME}" && mkdir -p ~/.local/bin && curl -sSfL https://golangci-lint.run/install.sh | sh -s -- -b ~/.local/bin'

    step "Installing gotestsum and just"
    as_runner 'go install gotest.tools/gotestsum@latest'
    # shellcheck disable=SC2016 # $HOME is the runner's, expanded at its login
    as_runner 'grep -q "HOME/go/bin" ~/.profile ||
        echo "if [ -d \"\$HOME/go/bin\" ]; then PATH=\"\$HOME/go/bin:\$PATH\"; fi" >> ~/.profile'
    as_runner 'curl --proto "=https" --tlsv1.2 -sSf https://just.systems/install.sh |
        bash -s -- --to ~/.local/bin'

    step "Pointing podman at cgroupfs"
    as_runner 'mkdir -p ~/.config/containers/'
    as_runner 'printf "[engine]\ncgroup_manager = \"cgroupfs\"\n" > ~/.config/containers/containers.conf'
    as_root loginctl enable-linger runner

    step "Restarting ${RUNNER} to pick all of it up"
    inc restart "${RUNNER}"

    as_runner 'command -v golangci-lint >/dev/null' ||
        warn "golangci-lint is not on the runner's PATH"
}

# --- 8. The OCI mirrors -----------------------------------------------------

add_registry_remotes() {
    if [[ -z "${REGISTRY_DOMAIN}" ]]; then
        warn "RUNNER_REGISTRY_DOMAIN is unset, skipping the OCI mirror remotes"
        return
    fi

    local -A mirrors=(
        [docker.io]=docker-registry
        [ghcr.io]=ghcr-registry
        [registry.gitlab.com]=gitlab-registry
    )

    for remote in "${!mirrors[@]}"; do
        step "Adding the ${remote} mirror"
        as_runner "incus remote rm ${remote} || true"
        as_runner "incus remote add --protocol=oci ${remote} https://${mirrors[${remote}]}.${REGISTRY_DOMAIN}"
    done
}

# --- 9+10. The certificate and the nested daemons ---------------------------

generate_certificate() {
    step "Generating the runner's client certificate"
    as_runner 'test -f ~/.config/incus/client.crt || incus remote generate-certificate'

    mkdir -p "$(dirname "${CERT}")"
    inc file pull "${RUNNER}/home/runner/.config/incus/client.crt" "${CERT}"
    step "The certificate is in ${CERT}"
}

# --- 12+13. The GitHub Actions runners --------------------------------------

install_gh_runners() {
    local first="${GH_RUNNERS[0]}"

    step "Downloading actions/runner ${GH_RUNNER_VERSION} into ~/${first}"
    as_runner "rm -rf ~/${first}; mkdir -p ~/${first}"
    as_runner "cd ~/${first} &&
        curl -o actions-runner.tar.gz -L ${GH_RUNNER_URL} &&
        tar xf actions-runner.tar.gz &&
        rm -f actions-runner.tar.gz"

    # The dependency installer wants root, and one run covers every copy.
    step "Installing the runner's dependencies"
    as_root "/home/runner/${first}/bin/installdependencies.sh"

    for dir in "${GH_RUNNERS[@]:1}"; do
        step "Copying ~/${first} to ~/${dir}"
        as_runner "rm -rf ~/${dir}; cp -a ~/${first} ~/${dir}"
    done

    # config.sh writes the rest of .env at registration, these two are ours.
    for dir in "${GH_RUNNERS[@]}"; do
        as_runner "printf 'HOME=/home/runner\nTEST_PROCS=${GH_TEST_PROCS}\n' > ~/${dir}/.env"
    done
}

# --- 14+15. What is left ----------------------------------------------------

print_next_steps() {
    cat <<EOF >&2

==> ${RUNNER} is built. Registering needs a token, which is why it stops here.

Take a token from
https://github.com/lxc/incus-compose/settings/actions/runners/new
and register each runner. A token is good for one registration, so fetch a
fresh one per directory:

  incus --project=${PROJECT} exec ${RUNNER} -- sudo -u runner -iH
$(for dir in "${GH_RUNNERS[@]}"; do
        echo "  cd ~/${dir} && ./config.sh --url https://github.com/lxc/incus-compose --token XXX"
    done)

Name each one after its directory and label it 'incus-compose', which is what
.github/workflows/test-e2e.yml runs on. Then, as root in the container:

$(for dir in "${GH_RUNNERS[@]}"; do
        echo "  (cd /home/runner/${dir} && ./svc.sh install runner && ./svc.sh start)"
    done)

Publish the container afterwards so runner.sh can restore it:

  ./scripts/runner.sh down
EOF
}

# --- Run it -----------------------------------------------------------------

load_openvswitch
create_container
install_base
install_incus_client
create_user
install_tools
generate_certificate
install_gh_runners
print_next_steps
