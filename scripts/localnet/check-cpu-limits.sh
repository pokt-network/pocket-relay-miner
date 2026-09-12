#!/usr/bin/env bash
# check-cpu-limits.sh -- GOMAXPROCS and the container's CPU limit must be the
# same number of cores, for the relayer and for the miner.
#
# They are rendered from ONE knob per binary (relayer.cpu_cores / miner.cpu_cores,
# defaults in tilt/k8s/defaults.Tiltfile, overridable in the gitignored
# tilt_config.yaml). Before that they were two literals under a comment saying
# "# Match CPU limit", and they had stopped matching: the relayer's limit was
# raised to 8 cores and its GOMAXPROCS literal stayed at 4. The pod was allowed
# 8 cores and the Go runtime only ever scheduled on 4, so a load ramp reaching
# that ceiling would have reported the RUNTIME's limit as this software's -- the
# same class of mistake as measuring a throttled load generator.
#
# IT READS THE LIVE POD, not the Tiltfile. Comparing the template against itself
# would pass while the running relayer still carried the old spec: a rollout that
# did not happen, a `kubectl edit`, or a second literal someone adds later are
# all invisible from the source and obvious from the pod.
#
# It prints what it read even when it passes. A jsonpath that matches nothing
# returns an empty string, and "" == "" is true -- so a silent green and "I
# looked nowhere" would be the same signal without the numbers on screen.
#
# Usage: scripts/localnet/check-cpu-limits.sh
#   APPS="relayer miner" overrides which deployments are checked.
#
# Exit: 0 all agree, 1 a divergence or a missing value, 2 could not look.
set -uo pipefail
cd "$(git rev-parse --show-toplevel)" || exit 2

command -v kubectl >/dev/null || { echo "check-cpu-limits: no kubectl on PATH" >&2; exit 2; }

apps="${APPS:-relayer miner}"
rc=0

# cores_of turns a Kubernetes CPU quantity into a whole number of cores, or
# prints nothing when it is not a whole number. A fractional limit cannot equal
# an integer GOMAXPROCS, and saying so beats rounding it into agreement.
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
    gomaxprocs="$(kubectl get pod "$pod" \
        -o jsonpath='{.spec.containers[?(@.name=="'"$app"'")].env[?(@.name=="GOMAXPROCS")].value}' 2>/dev/null)"

    if [ -z "$limit" ] || [ -z "$gomaxprocs" ]; then
        echo "check-cpu-limits: FAIL $app ($pod) -- read limits.cpu='$limit' GOMAXPROCS='$gomaxprocs'." >&2
        echo "  An empty read is not agreement: one of the two fields is missing from the pod spec." >&2
        rc=1
        continue
    fi

    limit_cores="$(cores_of "$limit")"
    if [ -z "$limit_cores" ]; then
        echo "check-cpu-limits: FAIL $app ($pod) -- limits.cpu=$limit is not a whole number of cores," >&2
        echo "  so it cannot equal GOMAXPROCS=$gomaxprocs, which the Go runtime reads as a count." >&2
        rc=1
        continue
    fi

    if [ "$limit_cores" != "$gomaxprocs" ]; then
        echo "check-cpu-limits: FAIL $app ($pod) -- GOMAXPROCS=$gomaxprocs but limits.cpu=$limit ($limit_cores cores)." >&2
        echo "  The pod may use $limit_cores cores and the Go runtime will schedule on $gomaxprocs." >&2
        echo "  Both must be rendered from ${app}.cpu_cores: if they differ, one stopped using it." >&2
        rc=1
        continue
    fi

    echo "check-cpu-limits: OK   $app ($pod) -- GOMAXPROCS=$gomaxprocs, limits.cpu=$limit ($limit_cores cores)"
done

exit $rc
