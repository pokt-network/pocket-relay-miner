#!/usr/bin/env bash
# test-cache-cleanup-live.sh — Level-3 live validation for
# `redis cache --type all --invalidate --all` under sustained traffic.
#
# Proves the cleanup is hot-safe: run it against the Tilt localnet while
# relays flow, then assert zero rejected relays, cache repopulation, and an
# uninterrupted claim/proof pipeline.
#
# Load is driven by the built-in relay CLI (`pocket-relay-miner relay
# jsonrpc --localnet`) DIRECTLY against the relayer (localhost:8180), NOT
# through the PATH gateway: PATH masks relayer 503s as 200 + empty body
# (known issue), which hid a real rejection burst during development. The
# CLI validates each relay end-to-end (supplier signature, JSON-RPC error
# field), so its Failed count reports the relayer's true behavior.
#
# Requirements: Tilt localnet up (relayer/miner/validator/redis),
# redis-cli, jq. PATH is NOT required.
#
# Usage: ./scripts/test-cache-cleanup-live.sh [--relays 9000] [--concurrency 10]

set -euo pipefail

# Counts are sized to fit under the relay meter's per-session claimable cap
# (localnet: ~999 relays per session+supplier; the budget is shared by
# everything hitting the session). Exceeding it yields legitimate HTTP 429
# "claimable portion fully consumed" responses — an economic limit, not a
# cleanup failure — so runs that hit ONLY that error wait for the next
# session and retry instead of failing.
RELAYS=${RELAYS:-600}
CONCURRENCY=${CONCURRENCY:-10}
BASELINE_RELAYS=${BASELINE_RELAYS:-200}
SESSION_WAIT_S=${SESSION_WAIT_S:-30}
SERVICE_ID="develop-http"
REDIS_CLI="redis-cli"

# Build once instead of `go run` per invocation: the main phase issues many
# short CLI bursts and per-burst compilation would distort pacing.
cd "$(dirname "$0")/.."
BIN_PATH=$(mktemp -d)/pocket-relay-miner
trap 'rm -rf "$(dirname "$BIN_PATH")"' EXIT
go build -o "$BIN_PATH" . || { echo "FAIL: go build" >&2; exit 1; }
BIN="$BIN_PATH"

while [[ $# -gt 0 ]]; do
  case "$1" in
    --relays) RELAYS="$2"; shift 2 ;;
    --concurrency) CONCURRENCY="$2"; shift 2 ;;
    *) echo "unknown arg: $1" >&2; exit 2 ;;
  esac
done

log() { printf '\n=== %s ===\n' "$*"; }
fail() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }

command -v jq >/dev/null || fail "jq not installed"

# parse_failed <output-file>: extracts the error count from a CLI summary
# ("Errors: N"; older builds print "Failed: N"). A clean run may omit the
# line entirely, so a 100% success rate parses as 0. Never exits non-zero
# (set -e safe): returns empty when the summary is unparseable.
parse_failed() {
  local n
  n=$(grep -oE "(Errors|Failed):[[:space:]]+[0-9]+" "$1" 2>/dev/null | grep -oE "[0-9]+" | head -1 || true)
  if [[ -z "$n" ]] && grep -q "Success Rate: 100" "$1" 2>/dev/null; then
    n=0
  fi
  echo "$n"
}

# only_meter_capped <output-file>: true when EVERY failure is the meter's
# HTTP 429 claimable-cap response (economic limit, not a relayer defect).
only_meter_capped() {
  local breakdown
  breakdown=$(grep -A10 -i "error breakdown" "$1" | grep -E "^\s+[0-9]+:" || true)
  [[ -n "$breakdown" ]] && ! echo "$breakdown" | grep -qv "claimable portion fully consumed"
}

# run_load <label> <count> <output-file>: runs the CLI load and asserts zero
# TRUE failures. Retries (next session) when the only failures are 429
# claimable-cap. The CLI validates supplier signatures and JSON-RPC errors
# per relay, so Failed>0 here is a REAL relayer-side failure — no PATH
# masking.
run_load() {
  local label="$1" count="$2" out="$3" attempt failed
  for attempt in 1 2 3; do
    $BIN relay jsonrpc --localnet --service "$SERVICE_ID" --load-test \
      -n "$count" --concurrency "$CONCURRENCY" > "$out" 2>&1 || true
    grep -E "Total Requests|Successful|Failed|Success Rate" "$out" || true
    failed=$(parse_failed "$out")
    [[ -n "$failed" ]] || fail "${label}: could not parse CLI summary (run died?)"
    if [[ "$failed" == "0" ]]; then
      echo "PASS: ${label} — 0 failed relays (signature-validated, no gateway masking)"
      return 0
    fi
    if only_meter_capped "$out"; then
      echo "NOTE: ${label} hit the per-session claimable cap (429) — waiting ${SESSION_WAIT_S}s for the next session (attempt ${attempt}/3)"
      sleep "$SESSION_WAIT_S"
      continue
    fi
    grep -A10 -i "error breakdown" "$out" || true
    fail "${label}: ${failed} relays failed (true relayer errors)"
  done
  fail "${label}: still meter-capped after 3 sessions — lower RELAYS/BASELINE_RELAYS"
}

log "Pre-flight (single validated relay via CLI, direct to relayer :8180)"
$BIN relay jsonrpc --localnet --service "$SERVICE_ID" -n 1 > /tmp/cleanup-preflight.txt 2>&1 \
  || { tail -5 /tmp/cleanup-preflight.txt; fail "pre-flight relay failed (Tilt up? relayer ready?)"; }
$REDIS_CLI ping >/dev/null || fail "redis not reachable"

cache_keys_before=$($REDIS_CLI --scan --pattern 'ha:cache:*' | wc -l)
supplier_keys_before=$($REDIS_CLI --scan --pattern 'ha:supplier:*' | wc -l)
smst_count_before=$($REDIS_CLI --scan --pattern 'ha:smst:*' | wc -l)
echo "cache keys: ${cache_keys_before}, supplier keys: ${supplier_keys_before}, smst keys: ${smst_count_before}"
[[ "$cache_keys_before" -gt 0 ]] || fail "no ha:cache:* keys — localnet not warmed up yet"

log "Baseline load (${BASELINE_RELAYS} relays, no cleanup)"
run_load "baseline" "$BASELINE_RELAYS" /tmp/cleanup-baseline.txt

log "Dry-run (must delete nothing)"
$BIN redis cache --type all --invalidate --all --dry-run
cache_keys_after_dry=$($REDIS_CLI --scan --pattern 'ha:cache:*' | wc -l)
[[ "$cache_keys_after_dry" -ge "$cache_keys_before" ]] || fail "dry-run deleted keys (${cache_keys_before} -> ${cache_keys_after_dry})"

# Fresh session before the main phase so the claimable budget is full.
echo "waiting ${SESSION_WAIT_S}s for a fresh session budget before the main phase"
sleep "$SESSION_WAIT_S"

# Main phase: sustained bursts with the cleanup mid-flight. A single
# --load-test run fires all relays in <1s (no rate limiting), finishing
# BEFORE the cleanup would run — no overlap, vacuous test. Instead: 20-relay
# bursts spread over ~30s, cleanup fired at burst 5, so bursts provably run
# before, during, and after it. Total volume (BURSTS*BURST_SIZE=400) stays
# under the per-session claimable cap.
BURSTS=${BURSTS:-20}
BURST_SIZE=${BURST_SIZE:-20}
log "Load ${BURSTS}x${BURST_SIZE} relay bursts (~30s) with cleanup mid-flight"
window_start_s=$(date +%s)
total_errors=0
total_ok=0
meter_capped=0
for burst in $(seq 1 "$BURSTS"); do
  out="/tmp/cleanup-burst-${burst}.txt"
  $BIN relay jsonrpc --localnet --service "$SERVICE_ID" --load-test \
    -n "$BURST_SIZE" --concurrency 5 > "$out" 2>&1 || true

  errs=$(parse_failed "$out")
  [[ -n "$errs" ]] || fail "burst ${burst}: could not parse CLI summary"
  if [[ "$errs" != "0" ]]; then
    if only_meter_capped "$out"; then
      meter_capped=$((meter_capped + errs))
    else
      grep -A10 -i "error breakdown" "$out" || true
      fail "burst ${burst}: ${errs} relays failed (true relayer errors)"
    fi
  fi
  total_errors=$((total_errors + errs))
  total_ok=$((total_ok + BURST_SIZE - errs))

  if [[ "$burst" == "5" ]]; then
    log "Executing cleanup (no dry-run) under load (burst ${burst}/${BURSTS})"
    # SMST snapshot tightly around the command itself: the miner legitimately
    # prunes trees of settled sessions on its own schedule, so a
    # whole-window count comparison false-positives. Only a drop across the
    # command's own execution would implicate the cleanup.
    smst_pre_cmd=$($REDIS_CLI --scan --pattern 'ha:smst:*' | sort)
    $BIN redis cache --type all --invalidate --all --yes | tee /tmp/cleanup-output.txt
    smst_post_cmd=$($REDIS_CLI --scan --pattern 'ha:smst:*' | sort)
  fi
  sleep 1
done
window_end_s=$(date +%s)
echo "bursts complete: ${total_ok} ok, ${total_errors} errors (${meter_capped} meter-capped 429s)"

log "Assertions"
# 1. Zero TRUE failures across every burst (signature-validated per relay by
#    the CLI — no gateway masking). Meter-capped 429s are an economic limit,
#    tolerated only if they are the ONLY error type AND some bursts spanned
#    the cleanup.
true_failures=$((total_errors - meter_capped))
[[ "$true_failures" == "0" ]] && echo "PASS: 0 true relay failures across ${BURSTS} bursts spanning the cleanup" \
  || fail "${true_failures} true relay failures during the cleanup window"
[[ "$meter_capped" == "0" ]] && echo "PASS: no meter-capped relays either (fully clean run)" \
  || echo "NOTE: ${meter_capped} relays hit the per-session claimable cap (429; economic limit, unrelated to cleanup)"

# 2. Defense in depth: relayer logs must show no supplier rejections either.
#    These are the ACTUAL log messages of the rejection paths in
#    relayer/proxy.go.
now_ns=$((window_end_s + 5))000000000
start_ns=$((window_start_s - 5))000000000
rejects=$(curl -sG 'http://localhost:3100/loki/api/v1/query_range' \
  --data-urlencode 'query={app="relayer"} |~ "supplier not (found in cache|active|staked for service)|supplier has no services registered"' \
  --data-urlencode "limit=5" --data-urlencode "start=${start_ns}" --data-urlencode "end=${now_ns}" \
  | jq -r '.data.result | length')
[[ "$rejects" == "0" ]] && echo "PASS: 0 supplier-rejection logs in relayer during cleanup window" \
  || fail "relayer logged supplier rejections during cleanup window"

# 2b. Sanity check the assertion itself is not vacuous: the same window with
#     a match-anything filter must return log volume.
sanity=$(curl -sG 'http://localhost:3100/loki/api/v1/query_range' \
  --data-urlencode 'query={app="relayer"}' \
  --data-urlencode "limit=5" --data-urlencode "start=${start_ns}" --data-urlencode "end=${now_ns}" \
  | jq -r '.data.result | length')
[[ "$sanity" != "0" ]] || fail "Loki returned no relayer logs at all — assertion #2 would be vacuous (is Loki scraping?)"

# 3. State untouched by the command itself: every SMST key present right
#    before the cleanup must still exist right after it (keys the miner adds
#    concurrently are fine; a key DELETED across the command implicates it).
missing_smst=$(comm -23 <(echo "$smst_pre_cmd") <(echo "$smst_post_cmd") | grep -c . || true)
[[ "$missing_smst" == "0" ]] && echo "PASS: no SMST key deleted across the cleanup command" \
  || fail "${missing_smst} SMST keys disappeared across the cleanup command execution"

# 4. Healthy supplier entries preserved.
supplier_keys_after=$($REDIS_CLI --scan --pattern 'ha:supplier:*' | wc -l)
echo "supplier keys: ${supplier_keys_before} -> ${supplier_keys_after}"
[[ "$supplier_keys_after" -gt 0 ]] || fail "all supplier entries deleted — healthy entries must survive"

# 5. Caches repopulate (leader refresh runs every 4 blocks).
log "Waiting up to 60s for cache repopulation"
repop=0
for i in $(seq 1 12); do
  sleep 5
  # grep -v exits 1 when nothing survives the filter (zero keys right after
  # cleanup) — mask it so pipefail doesn't kill the script mid-wait.
  repop=$($REDIS_CLI --scan --pattern 'ha:cache:*' | { grep -v 'ha:cache:lock:' || true; } | wc -l)
  echo "  t+$((i*5))s: ${repop} ha:cache:* keys"
  [[ "$repop" -gt 0 ]] && break
done
[[ "$repop" -gt 0 ]] && echo "PASS: caches repopulating (${repop} keys)" || fail "caches did not repopulate in 60s"

# 6. Claim/proof pipeline alive: no claim/proof failures in miner logs.
#    Regexes match the miner's ACTUAL failure messages.
errs=$(curl -sG 'http://localhost:3100/loki/api/v1/query_range' \
  --data-urlencode 'query={app="miner"} |~ "batched (claim|proof) (submission|callback) failed|(claim|proof) tx failed|(claim|proof)_tx_error"' \
  --data-urlencode "limit=5" --data-urlencode "start=${start_ns}" --data-urlencode "end=$(date +%s)000000000" \
  | jq -r '.data.result | length')
[[ "$errs" == "0" ]] && echo "PASS: no claim/proof failures in miner logs" \
  || fail "miner logged claim/proof failures after cleanup"

log "ALL LIVE ASSERTIONS PASSED"
