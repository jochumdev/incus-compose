# Standing fleets to point CoreDNS at.

# Builds a standing fleet and leaves it up.
#
#   just fleet flat   up 160 20      # 160 projects, 20 at a time
#   just fleet nested up 160 20
#   just fleet flat   down 20        # tear it down, 20 at a time
#
# The two topologies differ in one axis, and it is the axis worth isolating:
#
#   flat     three hosts on one network per project, so views scale with
#            projects. What the design assumes.
#   nested   one gateway per network and two workers on one each, so no two
#            instances share a network set - one view per instance, nothing
#            collapsing. The pathological case for byView, which is views x names.
#
# Both stamp user.coredns.stress=true, so one scope_marker serves either.
#
# down forces out everything whose name starts with PREFIX, and it is the same
# PREFIX up built with, so nothing is looked up.
#
# Environment:
#
#   PREFIX    project name prefix, default "flat" or "stress" by topology
#   NETWORKS  bridges per project, nested only, default 3
#   SERVICE   the shared fan-out name, flat only, default "web". Empty drops it
#   OVN=0     opts out of OVN. On by default: projects then own OVN networks of
#             their own rather than referencing bridges in the default project,
#             which is the case netKey exists for - two projects can each own a
#             network of the same name and they are genuinely different wires
#   UPLINK    the OVN uplink, default incusbr0. Needs ipv4.ovn.ranges set
#   IMAGE     instance image, POOL storage pool
[doc("Build or tear down a stress fleet: just fleet <flat|nested> <up|down> ...")]
fleet topology action *args:
    #!/usr/bin/env bash
    # -u only: the loops below lean on `test && action`, which is a non-zero
    # statement whenever the test is false.
    set -u

    topology="{{ topology }}"
    action="{{ action }}"

    usage() {
      echo "usage: just fleet <flat|nested> up PROJECTS [CONCURRENCY]" >&2
      echo "       just fleet <flat|nested> down [CONCURRENCY]" >&2

      exit 1
    }

    # Fixed per topology, because the shape is the experiment rather than a
    # parameter of it.
    case "${topology}" in
      flat)
        HOSTS=3
        default_prefix=flat
        ;;
      nested)
        WORKERS=2
        NETWORKS="${NETWORKS:-3}"
        default_prefix=stress
        ;;
      *) usage ;;
    esac

    PREFIX="${PREFIX:-${default_prefix}}"
    IMAGE="${IMAGE:-docker.io:library/nginx:alpine}"
    POOL="${POOL:-default}"
    UPLINK="${UPLINK:-incusbr0}"
    MARKER="user.coredns.stress"
    OVN="${OVN:-1}"

    RESULTS_DIR=$(mktemp -d)

    # flat: one network, three hosts on it, all answering to one shared name
    # beside their own - three instances behind web.<project> is what Render
    # packs into one reply. One profile for all three: same network, so the same
    # instance as far as visibility goes.
    project_up_flat() {
      local project=$1
      local h

      local -a svc=()
      [[ -n "${SERVICE-web}" ]] && svc=(-c "user.label.coredns.service=${SERVICE-web}")

      incus network create "${net_args[@]}" "${project}-net1" "${ovn_type[@]}" || return 1

      incus profile create "${inproj[@]}" "${project}-host" || return 1
      incus profile device add "${inproj[@]}" "${project}-host" root disk path=/ pool="${POOL}" || return 1
      incus profile device add "${inproj[@]}" "${project}-host" eth0 nic \
        network="${project}-net1" || return 1

      for ((h = 1; h <= HOSTS; h++)); do
        INCUS_PROJECT="${project}" incus launch "${IMAGE}" "host${h}" \
          -p "${project}-host" "${svc[@]}" || return 1
      done
    }

    # nested: NETWORKS bridges, a gateway spanning all of them and two workers on
    # one each, round-robin. Views then differ per instance, two workers on
    # different networks cannot see each other, and the gateway exercises the
    # join Gather only takes for a multi-homed name.
    project_up_nested() {
      local project=$1
      local n w on
      local -a gw

      for ((n = 1; n <= NETWORKS; n++)); do
        incus network create "${net_args[@]}" "${project}-net${n}" "${ovn_type[@]}" || return 1
      done

      # Only eth0 offers a default route. Every subsequent NIC is
      # ipv4.gateway=none, or each one adds a default route of its own and an OCI
      # instance egresses through whichever was applied last.
      incus profile create "${inproj[@]}" "${project}-gw" || return 1
      incus profile device add "${inproj[@]}" "${project}-gw" root disk path=/ pool="${POOL}" || return 1

      for ((n = 1; n <= NETWORKS; n++)); do
        gw=()
        ((n > 1)) && gw=(ipv4.gateway=none)

        incus profile device add "${inproj[@]}" "${project}-gw" "eth$((n - 1))" nic \
          network="${project}-net${n}" "${gw[@]}" || return 1
      done

      for ((w = 1; w <= WORKERS; w++)); do
        on=$(((w - 1) % NETWORKS + 1))

        incus profile create "${inproj[@]}" "${project}-w${w}" || return 1
        incus profile device add "${inproj[@]}" "${project}-w${w}" root disk path=/ pool="${POOL}" || return 1
        incus profile device add "${inproj[@]}" "${project}-w${w}" eth0 nic \
          network="${project}-net${on}" || return 1
      done

      # -p names the whole profile list, so these carry the root disk themselves
      # and the default project's default profile is never involved.
      INCUS_PROJECT="${project}" incus launch "${IMAGE}" gateway -p "${project}-gw" || return 1

      for ((w = 1; w <= WORKERS; w++)); do
        INCUS_PROJECT="${project}" incus launch "${IMAGE}" "worker${w}" \
          -p "${project}-w${w}" || return 1
      done
    }

    # A project that owns its networks has to own its profiles too: a NIC device
    # is validated when it is added, in the project the profile is in, so a
    # profile in default naming a network the project owns fails to load.
    project_up() {
      local project=$1
      local owned=false

      ((OVN)) && owned=true

      inproj=()
      net_args=()
      ovn_type=()

      if ((OVN)); then
        inproj=(--project "${project}")
        net_args=(--project "${project}")
        ovn_type=(--type ovn network="${UPLINK}")
      fi

      incus project create "${project}" \
        -c "features.networks=${owned}" \
        -c "features.profiles=${owned}" \
        -c "${MARKER}=true" || return 1

      "project_up_${topology}" "${project}"
    }

    # The project goes first: Incus refuses to delete a network or a profile
    # while anything still references it.
    project_down() {
      local project=$1
      local profile network

      echo yes | incus project delete --force "${project}"

      for profile in $(incus profile list -f csv 2>/dev/null | cut -d, -f1); do
        [[ "${profile}" == "${project}-"* ]] || continue

        incus profile delete "${profile}" 2>/dev/null
      done

      for network in $(incus network list -f csv 2>/dev/null | cut -d, -f1); do
        [[ "${network}" == "${project}-"* ]] || continue

        incus network delete "${network}" 2>/dev/null
      done
    }

    run_timed() {
      local label=$1 project=$2
      shift 2

      local start end duration status

      start=$(date +%s.%N)
      if "$@" >"${RESULTS_DIR}/${project}.log" 2>&1; then
        status="ok"
      else
        status="FAIL"
      fi
      end=$(date +%s.%N)
      duration=$(awk "BEGIN{printf \"%.2f\", ${end}-${start}}")

      echo "${duration}" >"${RESULTS_DIR}/${project}.time"
      echo "${status}" >"${RESULTS_DIR}/${project}.status"
      printf "  [%s] %-10s %-20s %6ss\n" "${status}" "${label}" "${project}" "${duration}"
    }

    summarize() {
      local -a times=()
      local f

      for f in "${RESULTS_DIR}"/*.time; do
        [[ -e "$f" ]] || continue
        times+=("$(cat "$f")")
      done

      [[ ${#times[@]} -eq 0 ]] && return 0

      local fail_count
      fail_count=$(grep -l "^FAIL$" "${RESULTS_DIR}"/*.status 2>/dev/null | wc -l)

      printf "%s\n" "${times[@]}" | awk -v fails="$fail_count" '
        { sum += $1; if (NR==1 || $1<min) min=$1; if (NR==1 || $1>max) max=$1; n++ }
        END {
          printf "  projects=%-4d fails=%-4s avg=%6.2fs min=%6.2fs max=%6.2fs\n", n, fails, sum/n, min, max
        }
      '
    }

    # Rolling rather than batched. Waiting for a whole batch leaves the slowest
    # project running on its own while the daemon has nothing else to do, which
    # reads as incusd idling halfway through every batch.
    rolling() {
      local concurrency=$1 label=$2 fn=$3
      shift 3

      local project pid
      local -a pids=() alive=()

      for project in "$@"; do
        run_timed "${label}" "${project}" "${fn}" "${project}" &
        pids+=("$!")

        if ((${#pids[@]} >= concurrency)); then
          wait -n

          alive=()
          for pid in "${pids[@]}"; do
            kill -0 "${pid}" 2>/dev/null && alive+=("${pid}")
          done

          pids=("${alive[@]}")
        fi
      done

      for pid in "${pids[@]}"; do wait "${pid}"; done
    }

    # Checked once rather than discovered PROJECTS times. An OVN network is
    # allocated an uplink address out of ipv4.ovn.ranges, and a fresh Incus sets
    # no such range - so every create fails for the same reason, one project at a
    # time.
    check_ovn() {
      if [[ -z "$(incus network get "${UPLINK}" ipv4.ovn.ranges 2>/dev/null)" ]]; then
        echo "uplink ${UPLINK} has no ipv4.ovn.ranges, so no OVN network can be allocated one." >&2
        echo "give it a slice of its own subnet, clear of the DHCP pool:" >&2
        echo "  incus network get ${UPLINK} ipv4.address" >&2
        echo "  incus network set ${UPLINK} ipv4.ovn.ranges <first>-<last>" >&2

        exit 1
      fi

      if ! incus network list -f csv 2>/dev/null | grep -q "^br-int,"; then
        echo "no br-int: OVS/OVN is not running on this daemon." >&2
        echo "incus-compose's setup-nested-incus.sh installs it, but only with -o:" >&2
        echo "  ./scripts/setup-nested-incus.sh -o ..." >&2
        echo "The uplink's ipv4.ovn.ranges is in its preseed either way, so the range" >&2
        echo "being set is not evidence that OVN is there." >&2

        exit 1
      fi
    }

    case "${action}" in
      up)
        projects="${3:-}"
        concurrency="${4:-8}"

        [[ -n "${projects}" ]] || usage

        ((OVN)) && check_ovn

        if [[ "${topology}" == flat ]]; then
          echo "Building ${projects} projects x 1 network x ${HOSTS} hosts"
          echo "  topology   every host on the same network, so one view per project"
        else
          echo "Building ${projects} projects x ${NETWORKS} networks x $((WORKERS + 1)) instances"
          echo "  topology   1 gateway on every network, ${WORKERS} workers on one each"
        fi

        echo "  image      ${IMAGE}"
        echo "  marker     ${MARKER}=true"
        echo "  logs       ${RESULTS_DIR}"
        echo

        wanted=()
        for ((p = 1; p <= projects; p++)); do
          wanted+=("$(printf '%s-%03d' "${PREFIX}" "${p}")")
        done

        start=$(date +%s.%N)
        rolling "${concurrency}" up project_up "${wanted[@]}"
        end=$(date +%s.%N)

        echo
        summarize
        printf "  wall-clock=%.2fs\n" "$(awk "BEGIN{print ${end}-${start}}")"
        echo

        if [[ "${topology}" == flat ]]; then
          echo "Instances: $((projects * HOSTS)), views: ${projects}"
          echo "Check with: PREFIX=${PREFIX} NAMES='host1 host2' just coredns-verify-dns 53 ${PREFIX}"
        else
          echo "Instances: $((projects * (WORKERS + 1))), NICs: $((projects * (NETWORKS + WORKERS)))"
          echo "Check with: PREFIX=${PREFIX} just coredns-verify-dns 53 ${PREFIX}"
        fi

        echo "Tear down with: just fleet ${topology} down ${concurrency}"
        ;;

      down)
        concurrency="${3:-8}"

        doomed=()
        for project in $(incus project list -f csv 2>/dev/null | cut -d, -f1 | sed 's/ (current)//'); do
          [[ "${project}" == "${PREFIX}"* ]] || continue

          doomed+=("${project}")
        done

        if ((${#doomed[@]} == 0)); then
          echo "no ${PREFIX}* projects to remove"

          exit 0
        fi

        echo "Removing ${#doomed[@]} projects, ${concurrency} at a time"
        echo "  logs       ${RESULTS_DIR}"
        echo

        start=$(date +%s.%N)
        rolling "${concurrency}" down project_down "${doomed[@]}"
        end=$(date +%s.%N)

        echo
        summarize
        printf "  wall-clock=%.2fs\n" "$(awk "BEGIN{print ${end}-${start}}")"
        ;;

      *) usage ;;
    esac
