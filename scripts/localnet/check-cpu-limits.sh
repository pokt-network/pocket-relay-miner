#!/usr/bin/env bash
# check-cpu-limits.sh -- the parallelism the Go runtime HAS must equal the
# container's CPU limit, for the relayer and for the miner.
#
# Nobody sets GOMAXPROCS. `main.go:7` imports go.uber.org/automaxprocs, which
# reads the cgroup quota and sets it. That is why this check has TWO halves, and
# the first one is the one that matters:
#
#   1. the GOMAXPROCS env must NOT exist. If it does, automaxprocs logs and
#      returns WITHOUT TOUCHING ANYTHING (maxprocs/maxprocs.go:105-111 in
#      v1.6.0), so the env is not a redundant value: it DEFEATS the mechanism.
#      That is how the relayer ran, env at 4 and limit at 8.
#   2. the real parallelism must equal the limit.
#
# IT READS THE RUNTIME, NOT THE ENV AND NOT THE TEMPLATE. The `ha_runtime_threads`
# gauge is `runtime.GOMAXPROCS(0)` (observability/runtime_metrics.go:308): what the
# process DOES, not what we asked for. A check against the env certified our own
# request -- the same class of mistake as measuring Redis's pod CPU instead of its
# main thread.
#
# It prints what it read even when it passes: a jsonpath matching nothing returns
# "" and "" == "" is true, so a silent green and "I looked nowhere" would be the
# same signal without the numbers on screen.
#
# Usage: scripts/localnet/check-cpu-limits.sh
#   APPS="relayer miner" overrides which deployments are checked.
#
# Exit: 0 all agree, 1 a divergence or a missing value, 2 could not look.
set -uo pipefail
cd "$(git rev-parse --show-toplevel)" || exit 2

command -v kubectl >/dev/null || { echo "check-cpu-limits: no kubectl on PATH" >&2; exit 2; }
command -v curl >/dev/null || { echo "check-cpu-limits: no curl on PATH" >&2; exit 2; }

apps="${APPS:-relayer miner}"
rc=0

# The metrics port Tilt proxies, per app.
port_of() {
    case "$1" in
        relayer) echo 9190 ;;
        miner)   echo 9092 ;;
        *)       echo "" ;;
    esac
}

# cores_of turns a Kubernetes CPU quantity into a whole number of cores, or prints
# nothing when it is not whole. A fractional limit cannot equal a parallelism,
# which is a count, and saying so beats rounding it into agreement.
cores_of() {
    local q="$1"
    case "$q" in
        *m) local milli="${q%m}"
            [ $((milli % 1000)) -eq 0 ] || return 0
            echo $((milli / 1000)) ;;
        *)  echo "$q" ;;
    esac
}

for app in $apps; do
    pod="$(kubectl get pods -l "app=$app" \
        -o jsonpath='{.items[?(@.status.phase=="Running")].metadata.name}' 2>/dev/null \
        | tr ' ' '\n' | head -1)"
    if [ -z "$pod" ]; then
        echo "check-cpu-limits: FAIL $app -- no Running pod with label app=$app." >&2
        echo "  Nothing was compared. This is not a pass: bring the stack up first." >&2
        rc=1
        continue
    fi

    limit="$(kubectl get pod "$pod" \
        -o jsonpath='{.spec.containers[?(@.name=="'"$app"'")].resources.limits.cpu}' 2>/dev/null)"
    env_gomaxprocs="$(kubectl get pod "$pod" \
        -o jsonpath='{.spec.containers[?(@.name=="'"$app"'")].env[?(@.name=="GOMAXPROCS")].value}' 2>/dev/null)"

    # Half 1: the env must NOT be there.
    if [ -n "$env_gomaxprocs" ]; then
        echo "check-cpu-limits: FAIL $app ($pod) -- GOMAXPROCS=$env_gomaxprocs is set on the pod." >&2
        echo "  With the env present automaxprocs (main.go:7) returns without touching anything, so" >&2
        echo "  the parallelism stops following the limit and has to be maintained by hand. Remove" >&2
        echo "  it from the template: the cgroup limit is already the single source." >&2
        rc=1
        continue
    fi

    port="$(port_of "$app")"
    if [ -z "$port" ]; then
        echo "check-cpu-limits: no metrics port known for '$app'" >&2
        exit 2
    fi

    body="$(curl -s --max-time 5 "http://localhost:$port/metrics" 2>/dev/null)"
    if [ -z "$body" ]; then
        echo "check-cpu-limits: could not read $app metrics on localhost:$port" >&2
        echo "  (Tilt proxies that port; no answer means nothing was measured, which is not a failure)" >&2
        exit 2
    fi
    threads="$(printf '%s\n' "$body" | awk '$1=="ha_runtime_threads"{print $2; exit}')"

    if [ -z "$limit" ] || [ -z "$threads" ]; then
        echo "check-cpu-limits: FAIL $app ($pod) -- read limits.cpu='$limit' ha_runtime_threads='$threads'." >&2
        echo "  An empty read is not agreement: one of the two is missing." >&2
        rc=1
        continue
    fi

    limit_cores="$(cores_of "$limit")"
    if [ -z "$limit_cores" ]; then
        echo "check-cpu-limits: FAIL $app ($pod) -- limits.cpu=$limit is not a whole number of cores," >&2
        echo "  so it cannot equal a parallelism of $threads, which is a count." >&2
        rc=1
        continue
    fi

    if [ "$limit_cores" != "$threads" ]; then
        echo "check-cpu-limits: FAIL $app ($pod) -- the runtime schedules on $threads and limits.cpu=$limit ($limit_cores cores)." >&2
        echo "  automaxprocs should have equalised them. If it did not, either the env is set somewhere" >&2
        echo "  else, or the limit changed without the process restarting." >&2
        rc=1
        continue
    fi

    echo "check-cpu-limits: OK   $app ($pod) -- runtime=$threads, limits.cpu=$limit ($limit_cores cores), no GOMAXPROCS env"
done

exit $rc
