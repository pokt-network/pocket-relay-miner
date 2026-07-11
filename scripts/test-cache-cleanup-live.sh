#!/usr/bin/env bash
# test-cache-cleanup-live.sh — Level-3 live validation for
# `redis cache --type all --invalidate --all` under sustained traffic.
#
# Proves the cleanup is hot-safe: run it against the Tilt localnet while
# relays flow, then assert zero rejected relays, cache repopulation, and an
# uninterrupted claim/proof pipeline.
#
# Requirements: Tilt localnet up (relayer/miner/path/validator/redis),
# `hey` (go install github.com/rakyll/hey@latest), redis-cli, jq.
#
# Usage: ./scripts/test-cache-cleanup-live.sh [--duration 60] [--rps 200]

set -euo pipefail

DURATION=${DURATION:-60}
RPS=${RPS:-200}
CONCURRENCY=${CONCURRENCY:-10}
PATH_URL="http://localhost:3069/v1"
SERVICE_ID="develop-http"
REDIS_CLI="redis-cli"
BIN="go run ."

# `go run .` needs the repo root as cwd regardless of where the script is
# invoked from.
cd "$(dirname "$0")/.."

while [[ $# -gt 0 ]]; do
  case "$1" in
    --duration) DURATION="$2"; shift 2 ;;
    --rps) RPS="$2"; shift 2 ;;
    *) echo "unknown arg: $1" >&2; exit 2 ;;
  esac
done

log() { printf '\n=== %s ===\n' "$*"; }
fail() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }

command -v hey >/dev/null || fail "hey not installed"
command -v jq >/dev/null || fail "jq not installed"

log "Pre-flight"
code=$(curl -s -o /dev/null -w "%{http_code}" -X POST \
  -H "Content-Type: application/json" -H "Target-Service-Id: ${SERVICE_ID}" \
  -d '{"jsonrpc":"2.0","method":"eth_blockNumber","params":[],"id":1}' "${PATH_URL}")
[[ "$code" == "200" ]] || fail "PATH gateway pre-flight returned ${code} (Tilt up?)"
$REDIS_CLI ping >/dev/null || fail "redis not reachable"

cache_keys_before=$($REDIS_CLI --scan --pattern 'ha:cache:*' | wc -l)
supplier_keys_before=$($REDIS_CLI --scan --pattern 'ha:supplier:*' | wc -l)
state_fingerprint_before=$($REDIS_CLI --scan --pattern 'ha:miner:sessions:*' | sort | sha256sum | cut -d' ' -f1)
smst_count_before=$($REDIS_CLI --scan --pattern 'ha:smst:*' | wc -l)
echo "cache keys: ${cache_keys_before}, supplier keys: ${supplier_keys_before}, smst keys: ${smst_count_before}"
[[ "$cache_keys_before" -gt 0 ]] || fail "no ha:cache:* keys — localnet not warmed up yet"

# hey's -q is queries-per-second PER WORKER (-c), so divide the target rate
# across the workers to actually drive ~RPS total.
PER_WORKER_QPS=$(( (RPS + CONCURRENCY - 1) / CONCURRENCY ))

log "Baseline load (10s, no cleanup, ~${RPS} rps total)"
hey -z 10s -q "$PER_WORKER_QPS" -c "$CONCURRENCY" -m POST \
  -H "Content-Type: application/json" -H "Target-Service-Id: ${SERVICE_ID}" \
  -d '{"jsonrpc":"2.0","method":"eth_blockNumber","params":[],"id":1}' \
  "${PATH_URL}" > /tmp/cleanup-baseline.txt
grep -A5 'Status code distribution' /tmp/cleanup-baseline.txt
baseline_non200=$(grep -A20 'Status code distribution' /tmp/cleanup-baseline.txt | { grep '\[' || true; } | { grep -cv '\[200\]' || true; })
[[ "$baseline_non200" == "0" ]] || fail "baseline already returns non-200 responses; environment unhealthy before cleanup"

log "Dry-run (must delete nothing)"
$BIN redis cache --type all --invalidate --all --dry-run
cache_keys_after_dry=$($REDIS_CLI --scan --pattern 'ha:cache:*' | wc -l)
[[ "$cache_keys_after_dry" -ge "$cache_keys_before" ]] || fail "dry-run deleted keys (${cache_keys_before} -> ${cache_keys_after_dry})"

log "Load ${DURATION}s @ ~${RPS} rps with cleanup mid-flight"
hey -z "${DURATION}s" -q "$PER_WORKER_QPS" -c "$CONCURRENCY" -m POST \
  -H "Content-Type: application/json" -H "Target-Service-Id: ${SERVICE_ID}" \
  -d '{"jsonrpc":"2.0","method":"eth_blockNumber","params":[],"id":1}' \
  "${PATH_URL}" > /tmp/cleanup-load.txt &
HEY_PID=$!

sleep 5  # let load stabilize
log "Executing cleanup (no dry-run) under load"
$BIN redis cache --type all --invalidate --all --yes | tee /tmp/cleanup-output.txt

wait "$HEY_PID"

log "Assertions"
grep -A10 'Status code distribution' /tmp/cleanup-load.txt

# 1. Zero non-200 during load-with-cleanup.
non200=$(grep -A20 'Status code distribution' /tmp/cleanup-load.txt | grep '\[' | grep -v '\[200\]' || true)
[[ -z "$non200" ]] && echo "PASS: 0 non-200 responses under cleanup" \
  || fail "non-200 responses during cleanup: ${non200}"

# 2. PATH masks 503 as 200+empty body (known issue) — cross-check relayer
#    rejects via Loki instead of trusting hey alone. These are the ACTUAL log
#    messages the relayer emits on the supplier-rejection paths
#    (relayer/proxy.go): "supplier not found in cache", "supplier not active",
#    "supplier has no services registered", "supplier not staked for service".
now_ns=$(date +%s)000000000
start_ns=$(date -d "-$((DURATION + 10)) seconds" +%s)000000000
rejects=$(curl -sG 'http://localhost:3100/loki/api/v1/query_range' \
  --data-urlencode 'query={app="relayer"} |~ "supplier not (found in cache|active|staked for service)|supplier has no services registered"' \
  --data-urlencode "limit=5" --data-urlencode "start=${start_ns}" --data-urlencode "end=${now_ns}" \
  | jq -r '.data.result | length')
[[ "$rejects" == "0" ]] && echo "PASS: 0 supplier-rejection logs in relayer during cleanup window" \
  || fail "relayer logged supplier rejections during cleanup window"

# 2b. Sanity check the assertion itself is not vacuous: the same query with a
#     match-anything filter over the same window must return log volume.
sanity=$(curl -sG 'http://localhost:3100/loki/api/v1/query_range' \
  --data-urlencode 'query={app="relayer"}' \
  --data-urlencode "limit=5" --data-urlencode "start=${start_ns}" --data-urlencode "end=${now_ns}" \
  | jq -r '.data.result | length')
[[ "$sanity" != "0" ]] || fail "Loki returned no relayer logs at all — assertion #2 would be vacuous (is Loki scraping?)"

# 3. State untouched.
state_fingerprint_after=$($REDIS_CLI --scan --pattern 'ha:miner:sessions:*' | sort | sha256sum | cut -d' ' -f1)
smst_count_after=$($REDIS_CLI --scan --pattern 'ha:smst:*' | wc -l)
[[ "$state_fingerprint_before" == "$state_fingerprint_after" ]] || echo "NOTE: session key set changed (sessions naturally rotate; verify manually)"
[[ "$smst_count_after" -ge "$smst_count_before" ]] && echo "PASS: SMST keys not deleted (${smst_count_before} -> ${smst_count_after})" \
  || fail "SMST keys decreased (${smst_count_before} -> ${smst_count_after})"

# 4. Healthy supplier entries preserved.
supplier_keys_after=$($REDIS_CLI --scan --pattern 'ha:supplier:*' | wc -l)
echo "supplier keys: ${supplier_keys_before} -> ${supplier_keys_after}"
[[ "$supplier_keys_after" -gt 0 ]] || fail "all supplier entries deleted — healthy entries must survive"

# 5. Caches repopulate (leader refresh runs every 4 blocks; block=2s local).
log "Waiting up to 60s for cache repopulation"
for i in $(seq 1 12); do
  sleep 5
  # `grep -v` exits 1 when nothing survives the filter (zero keys right after
  # cleanup) — mask it so pipefail doesn't kill the script mid-wait.
  repop=$($REDIS_CLI --scan --pattern 'ha:cache:*' | { grep -v 'ha:cache:lock:' || true; } | wc -l)
  echo "  t+$((i*5))s: ${repop} ha:cache:* keys"
  [[ "$repop" -gt 0 ]] && break
done
[[ "$repop" -gt 0 ]] && echo "PASS: caches repopulating (${repop} keys)" || fail "caches did not repopulate in 60s"

# 6. Claim/proof pipeline alive: no new claim/proof errors in miner logs
#    post-cleanup. These regexes match the miner's ACTUAL failure messages
#    ("batched claim submission failed", "batched proof callback failed",
#    "claim tx failed", "proof tx failed", claim_tx_error/proof_tx_error
#    state transitions).
errs=$(curl -sG 'http://localhost:3100/loki/api/v1/query_range' \
  --data-urlencode 'query={app="miner"} |~ "batched (claim|proof) (submission|callback) failed|(claim|proof) tx failed|(claim|proof)_tx_error"' \
  --data-urlencode "limit=5" --data-urlencode "start=${start_ns}" --data-urlencode "end=$(date +%s)000000000" \
  | jq -r '.data.result | length')
[[ "$errs" == "0" ]] && echo "PASS: no claim/proof failures in miner logs" \
  || fail "miner logged claim/proof failures after cleanup"

log "ALL LIVE ASSERTIONS PASSED"
