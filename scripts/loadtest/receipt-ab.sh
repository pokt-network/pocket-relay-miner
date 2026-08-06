#!/usr/bin/env bash
#
# A/B measurement of the Pocket-Relay-Receipt cost.
#
# The question: what does one extra secp256k1 signature per relay cost.
#
# The feature is request-driven, so within a mode every arm runs against the
# SAME process, without restart and without config reload. No cold caches, no
# re-warm, no machine-state change between arms. The only difference is whether
# the loader sends the Pocket-Sign-Receipt header.
#
# Six arms, and the two modes are NOT redundant — they bracket the answer:
#
#   simulated  no meter, no publish, no second signature, no SMST. The least
#              work per relay, so the receipt is the LARGEST share of CPU it can
#              ever be. Cleanest signal, upper bound on the percentage.
#   real       the full pipeline including the asynchronous publish and the
#              second signature it performs. The receipt is a SMALLER share.
#              This is the honest production number.
#
# Reporting only the simulated figure overstates the cost; reporting only the
# real figure buries it in noise. Report both, and say which is which.
#
#   scripts/loadtest/receipt-ab-preflight.sh      # must pass first
#   scripts/loadtest/receipt-ab.sh
#   RPS=50 DURATION=60 scripts/loadtest/receipt-ab.sh    # dry run
#
# Results land in OUT_DIR (default scripts/localonly/receipt-ab/). They are
# never committed.

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"

RELAYER_URL="${RELAYER_URL:-http://localhost:8180}"
PPROF_URL="${PPROF_URL:-http://localhost:6060}"
PROM_URL="${PROM_URL:-http://localhost:9091}"
SERVICE_ID="${SERVICE_ID:-develop-http}"
SIM_KEY_ID="${SIM_KEY_ID:-sim-http}"
RPS="${RPS:-400}"
DURATION="${DURATION:-600}"
CONCURRENCY="${CONCURRENCY:-100}"
PROFILE_SECONDS="${PROFILE_SECONDS:-60}"
PROFILE_AT="${PROFILE_AT:-$((DURATION / 2))}"
OUT_DIR="${OUT_DIR:-$REPO_ROOT/scripts/localonly/receipt-ab}"
CLI="${CLI:-$REPO_ROOT/bin/pocket-relay-miner}"

BOLD=$'\033[1m'; GREEN=$'\033[0;32m'; RED=$'\033[0;31m'; YELLOW=$'\033[1;33m'; NC=$'\033[0m'
say()  { printf '\n%s==> %s%s\n' "$BOLD" "$*" "$NC"; }
ok()   { printf '  %sok%s   %s\n' "$GREEN" "$NC" "$*"; }
warn() { printf '  %swarn%s %s\n' "$YELLOW" "$NC" "$*"; }
die()  { printf '  %sFAIL%s %s\n' "$RED" "$NC" "$*"; exit 1; }

[ -x "$CLI" ] || die "CLI not found at $CLI — build it with 'make build' (never 'go build')"
mkdir -p "$OUT_DIR"

# Fail early if the pod changed shape since preflight: a resource change between
# arms makes the comparison meaningless.
BASELINE_RESOURCES=/tmp/receipt-ab-resources.json
[ -f "$BASELINE_RESOURCES" ] || die "run receipt-ab-preflight.sh first"

check_pod_unchanged() {
    local replicas restarts resources
    replicas="$(kubectl get deploy relayer -o jsonpath='{.spec.replicas}' 2>/dev/null)"
    [ "$replicas" = "1" ] || die "relayer replicas changed to $replicas mid-run"

    restarts="$(kubectl get pods -l app=relayer \
        -o jsonpath='{.items[0].status.containerStatuses[0].restartCount}' 2>/dev/null)"
    [ "${restarts:-0}" = "0" ] || die "relayer restarted mid-run (restartCount=$restarts); results are void"

    resources="$(kubectl get deploy relayer \
        -o jsonpath='{.spec.template.spec.containers[0].resources}' 2>/dev/null)"
    [ "$resources" = "$(cat "$BASELINE_RESOURCES")" ] \
        || die "relayer pod resources changed since preflight; arms are not comparable"
}

# prom_scalar QUERY — prints the scalar value, or nothing.
# Null-guards any label that may be absent: a jq expression without the guard
# explodes on series lacking the label. That trap already cost a leak-test run.
prom_scalar() {
    curl -fsS -m 10 --data-urlencode "query=$1" "$PROM_URL/api/v1/query" 2>/dev/null \
        | jq -r '.data.result[0].value[1] // empty' 2>/dev/null
}

record_metrics() {
    local arm="$1" when="$2"
    {
        printf 'arm=%s when=%s\n' "$arm" "$when"
        printf 'cpu_seconds_rate=%s\n'  "$(prom_scalar 'rate(process_cpu_seconds_total{job=~".*relayer.*"}[2m])')"
        printf 'rss_bytes=%s\n'         "$(prom_scalar 'process_resident_memory_bytes{job=~".*relayer.*"}')"
        printf 'goroutines=%s\n'        "$(prom_scalar 'go_goroutines{job=~".*relayer.*"}')"
        printf 'relays_rejected=%s\n'   "$(prom_scalar 'sum(ha_relayer_relays_rejected_total)')"
        printf 'receipts_total=%s\n'    "$(prom_scalar 'sum(ha_relayer_relay_receipts_total)')"
        printf 'receipt_errors=%s\n'    "$(prom_scalar 'sum(ha_relayer_relay_receipt_errors_total)')"
    } >> "$OUT_DIR/$arm.metrics"
}

# run_arm NAME MODE RECEIPT
#   MODE    = sim | real
#   RECEIPT = yes | no
run_arm() {
    local arm="$1" mode="$2" receipt="$3"
    say "arm $arm  (mode=$mode receipt=$receipt, ${RPS} RPS for ${DURATION}s)"
    check_pod_unchanged

    local args=(
        relay jsonrpc --localnet
        --service "$SERVICE_ID"
        --relayer-url "$RELAYER_URL"
        --load-test
        --count "$((RPS * DURATION))"
        --rps "$RPS"
        --concurrency "$CONCURRENCY"
        --timeout 30
    )
    [ "$mode" = "sim" ] && args+=(--simulate --sim-key-id "$SIM_KEY_ID")
    [ "$receipt" = "yes" ] && args+=(--request-receipt)

    record_metrics "$arm" before

    "$CLI" "${args[@]}" > "$OUT_DIR/$arm.load.txt" 2>&1 &
    local load_pid=$!
    ok "loader started (pid $load_pid)"

    # Profile at steady state, not at ramp-up.
    ( sleep "$PROFILE_AT"
      curl -fsS -m $((PROFILE_SECONDS + 30)) \
          "$PPROF_URL/debug/pprof/profile?seconds=$PROFILE_SECONDS" \
          -o "$OUT_DIR/$arm.cpu.pprof" 2>/dev/null
      curl -fsS -m 30 "$PPROF_URL/debug/pprof/heap" \
          -o "$OUT_DIR/$arm.heap.pprof" 2>/dev/null
      ) &
    local prof_pid=$!

    # Only ever wait on processes this script started; never pattern-kill.
    wait "$load_pid"; local load_rc=$?
    wait "$prof_pid" 2>/dev/null || true

    record_metrics "$arm" after
    check_pod_unchanged

    [ -s "$OUT_DIR/$arm.cpu.pprof" ] || warn "no CPU profile captured for $arm"
    if [ "$load_rc" -ne 0 ]; then
        warn "loader exited non-zero ($load_rc) for arm $arm — see $arm.load.txt"
    else
        ok "arm $arm complete"
    fi
}

say "relay receipt A/B — 6 arms, ~$((6 * DURATION / 60)) minutes of load"
printf '  results: %s\n' "$OUT_DIR"

# Simulated first: the cheaper mode, so a failure there costs less to redo.
run_arm S-A1 sim  no
run_arm S-B  sim  yes
run_arm S-A2 sim  no

run_arm R-A1 real no
run_arm R-B  real yes
run_arm R-A2 real no

say "analysis"
cat <<EOF
  go tool pprof -base $OUT_DIR/S-A1.cpu.pprof $OUT_DIR/S-B.cpu.pprof
  go tool pprof -base $OUT_DIR/R-A1.cpu.pprof $OUT_DIR/R-B.cpu.pprof

  ACCEPTANCE RULE — fixed before the run, not renegotiable after seeing numbers.
  Applied PER MODE: if |A1 - A2| is not clearly smaller than |B - A1|, that
  mode's live measurement is NOT significant and must be reported as such.

  It is entirely possible for simulated mode to yield a significant result and
  real mode not to. That outcome is informative, not a failure, and reporting
  it honestly is the point.

  Where neither mode is significant, the microbenchmark
  (go test -tags test -bench Receipt -benchmem -run '^\$' ./relayer/)
  is the answer, and the live run supports only the weaker claim: no regression
  in p99 or RSS.

  Report the receipt's share of total CPU as a percentage in each mode,
  comparable to the ~9% gzip cost at 200 RPS recorded in relayer/config.go.

  Finally: tilt_config.yaml is TRACKED. Revert the replica change before
  pushing — 'git diff --stat tilt_config.yaml' must be empty.
EOF
