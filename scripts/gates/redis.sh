#!/usr/bin/env bash
#
# Provide a REAL Redis for the test gates and print its URL on stdout.
#
# miniredis is not Redis where the interesting behaviour lives -- it answers a
# blocking XREADGROUP immediately, does not age PEL entries, and approximates
# expiry -- and this repository has already paid for that gap with a consumer
# that could not be shut down while the whole suite stayed green.
#
# Deliberately NOT the localnet's Redis. Locally that is the running Tilt fleet
# holding live relay traffic, so tests would compete with the thing they are
# meant to measure, on a port a stray FLUSHALL could ruin. This starts its own
# container on its own port instead.
#
# One container for the WHOLE run, not one per package: `go test ./...` runs
# package binaries in parallel, and a container plus a reaper per binary is how
# the first attempt at this timed out.
#
#   eval "$(scripts/gates/redis.sh up)"   # exports REDIS_TEST_URL
#   scripts/gates/redis.sh down

set -uo pipefail

REDIS_TEST_IMAGE="${REDIS_TEST_IMAGE:-redis:8-alpine}"
REDIS_TEST_PORT="${REDIS_TEST_PORT:-6399}"
REDIS_TEST_NAME="${REDIS_TEST_NAME:-prm-gate-redis}"
# Written when this script STARTS the container, so `down` can tell "mine" from
# "somebody else's". Keyed by container name so two differently-named servers do
# not share a marker.
REDIS_OWNED_MARKER="${TMPDIR:-/tmp}/.${REDIS_TEST_NAME}.started-by-gate"

url() { printf 'redis://127.0.0.1:%s' "$REDIS_TEST_PORT"; }

case "${1:-up}" in
up)
    # An externally provided server wins: CI may supply one as a service, and a
    # developer may already have the container up.
    if [ -n "${REDIS_TEST_URL:-}" ]; then
        printf 'export REDIS_TEST_URL=%q\n' "$REDIS_TEST_URL"
        exit 0
    fi

    if ! command -v docker >/dev/null 2>&1; then
        printf 'scripts/gates/redis.sh: docker is not installed and REDIS_TEST_URL is unset;\n' >&2
        printf '  tests that assert real-Redis semantics cannot run\n' >&2
        exit 1
    fi

    if [ -z "$(docker ps -q --filter "name=^${REDIS_TEST_NAME}$")" ]; then
        docker rm -f "$REDIS_TEST_NAME" >/dev/null 2>&1 || true
        if ! docker run -d --rm --name "$REDIS_TEST_NAME" \
            -p "127.0.0.1:${REDIS_TEST_PORT}:6379" "$REDIS_TEST_IMAGE" >/dev/null; then
            printf 'scripts/gates/redis.sh: could not start %s\n' "$REDIS_TEST_IMAGE" >&2
            exit 1
        fi
        # Mark it as ours. `down` removes the container only when this marker
        # is present, so a run that merely REUSED a container somebody else
        # started -- another `make gate` on this workstation, or a developer who
        # ran `redis.sh up` in their shell -- cannot delete it out from under
        # them. Killing another session's work is the failure this repository
        # has a standing rule about.
        : >"$REDIS_OWNED_MARKER"
    fi

    # Wait for it to answer rather than sleeping a guess.
    for _ in $(seq 1 60); do
        if docker exec "$REDIS_TEST_NAME" redis-cli ping 2>/dev/null | grep -q PONG; then
            printf 'export REDIS_TEST_URL=%q\n' "$(url)"
            exit 0
        fi
        sleep 0.5
    done
    printf 'scripts/gates/redis.sh: %s never answered PING\n' "$REDIS_TEST_NAME" >&2
    exit 1
    ;;
down)
    if [ -f "$REDIS_OWNED_MARKER" ]; then
        docker rm -f "$REDIS_TEST_NAME" >/dev/null 2>&1 || true
        rm -f "$REDIS_OWNED_MARKER"
    fi
    ;;
url)
    url
    ;;
*)
    printf 'usage: %s [up|down|url]\n' "$0" >&2
    exit 2
    ;;
esac
