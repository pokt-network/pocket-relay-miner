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
# One SHARED container for the whole run, not one per package: `go test ./...` runs
# package binaries in parallel, and a container per binary is how the first attempt
# at this timed out -- that was 24 CONTAINERS, not 24 reapers: testcontainers starts
# a single Ryuk per run (it groups by a hash of the parent PID), measured live on
# 2026-09-18.
#
# The exception is the handful of tests that change SERVER-wide config (`maxmemory`,
# `maxmemory-policy`), which prefix isolation cannot contain: each asks for its own
# with `testredis.Exclusive(t)` -- one container per TEST on an ephemeral port. See
# `internal/testredis/exclusive.go`.
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
# Liveness, which the marker cannot express. The marker says WHO started the
# container; it cannot say whether anyone is still USING it, and that is the
# question `down` has to answer. A run holds a SHARED lock on this file for its
# whole duration, so `down` asking for the EXCLUSIVE one is asking "is anybody
# else alive?" -- and the kernel answers, because it releases the lock when the
# holder dies, SIGKILL included. That is the property no marker, refcount or
# trap can offer: two runs died by SIGKILL today and `all.sh`, the only caller
# of `down`, does not run under it. A refcount would have stayed pinned forever
# with no signal; this cannot, because nobody writes it.
REDIS_LOCK="${TMPDIR:-/tmp}/.${REDIS_TEST_NAME}.lock"

# holders_alive -- true when another run holds the shared lock. Non-blocking on
# purpose: waiting here would turn "somebody else is testing" into a hang.
holders_alive() {
    command -v flock >/dev/null 2>&1 || return 1
    if ( exec 9>"$REDIS_LOCK"; flock -x -n 9 ) 2>/dev/null; then
        return 1
    fi
    return 0
}

url() { printf 'redis://127.0.0.1:%s' "$REDIS_TEST_PORT"; }

# answers_at_url -- is the server reachable FROM WHERE THE TESTS WILL LOOK?
#
# NOT `docker exec <container> redis-cli ping`. That probe runs INSIDE the
# container: it proves the server is alive in its own namespace and says nothing
# about the port mapping the client actually uses, so a broken or unpublished
# `-p` mapping reads as ready. Measured in the pair 2026-08-26: budgetkit waited
# with `docker exec budgetkit-test-pg pg_isready`, reported its infra up with the
# database unreachable from the URL, and 374 tests then blamed the product for
# the harness talking to another tool's database. Same shape, same file, here.
answers_at_url() {
    if command -v redis-cli >/dev/null 2>&1; then
        redis-cli -u "$(url)" ping 2>/dev/null | grep -q PONG
        return
    fi
    # No redis-cli on the host: a TCP connect is weaker -- it does not prove the
    # server speaks RESP -- but it crosses the same path the client will.
    (exec 3<>"/dev/tcp/127.0.0.1/${REDIS_TEST_PORT}") 2>/dev/null
}

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
        # A STOPPED container with this name may be somebody else's. The
        # ownership guard used to live only in `down`, so this line deleted it
        # without asking -- the same failure `down` is careful about, one
        # function up. Reuse what is not ours; only replace what is.
        if [ -n "$(docker ps -aq --filter "name=^${REDIS_TEST_NAME}$")" ] && [ ! -f "$REDIS_OWNED_MARKER" ]; then
            if ! docker start "$REDIS_TEST_NAME" >/dev/null 2>&1; then
                printf 'scripts/gates/redis.sh: a stopped %s exists but is not ours and will not start;\n' "$REDIS_TEST_NAME" >&2
                printf '  remove it by hand if it is stale: docker rm -f %s\n' "$REDIS_TEST_NAME" >&2
                exit 1
            fi
        else
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
    fi

    # Wait for it to answer rather than sleeping a guess.
    for _ in $(seq 1 60); do
        if answers_at_url; then
            printf 'export REDIS_TEST_URL=%q\n' "$(url)"
            exit 0
        fi
        sleep 0.5
    done
    printf 'scripts/gates/redis.sh: %s never answered PING\n' "$REDIS_TEST_NAME" >&2
    exit 1
    ;;
down)
    # Two conditions, and both are needed. The marker is OWNERSHIP: this run
    # started it, so it is not a developer's container. The lock is LIVENESS:
    # nobody else is mid-run. Ownership alone is what was here, and it is what
    # let one session delete the Redis another session was testing against --
    # the second session REUSES and writes no marker, so the first stays
    # "owner" and its `down` killed a live run.
    if [ -f "$REDIS_OWNED_MARKER" ]; then
        if holders_alive; then
            printf 'scripts/gates/redis.sh: another run is using %s; leaving it up\n' "$REDIS_TEST_NAME" >&2
        else
            docker rm -f "$REDIS_TEST_NAME" >/dev/null 2>&1 || true
            rm -f "$REDIS_OWNED_MARKER"
        fi
    fi
    ;;
url)
    url
    ;;
lockpath)
    # So a caller can hold the shared lock without repeating the path. The path
    # is declared once, here: a second copy of it is a mirror, and a mirror of a
    # LOCK path that drifts means two runs locking different files and neither
    # seeing the other.
    printf '%s\n' "$REDIS_LOCK"
    ;;
*)
    printf 'usage: %s [up|down|url|lockpath]\n' "$0" >&2
    exit 2
    ;;
esac
