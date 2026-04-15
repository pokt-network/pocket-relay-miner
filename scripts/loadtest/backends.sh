#!/usr/bin/env bash
#
# Direct backend load test for relay-miner upstream chain nodes.
#
# Bypasses the relayer entirely. Hits each backend RPC URL directly with a
# chain-appropriate JSON-RPC payload. The point is to know each upstream's
# ceiling before we look at relayer-imposed overhead.
#
# This script ships **no** infrastructure data. Backend URLs and the SSH
# host live in a config file outside the tracked tree:
#
#   default: scripts/localonly/loadtest/backends.conf
#   override: BACKENDS_CONF=/path/to/your.conf
#
# Copy `backends.conf.example` (next to this script) to that path, edit
# in your own endpoints, and run. The `localonly/` directory is gitignored
# so your real URLs never get committed.
#
# Usage:
#   scripts/loadtest/backends.sh list
#   scripts/loadtest/backends.sh probe                          # 1 req per backend, no hey
#   scripts/loadtest/backends.sh test <service> [--reqs N] [--conns N]
#   scripts/loadtest/backends.sh ramp <service>                 # c=10,50,100,250,500,1000
#   scripts/loadtest/backends.sh optimal <service> [--max-p99-ms N]
#   scripts/loadtest/backends.sh sweep [--include-broken]
#   scripts/loadtest/backends.sh sweep-optimal [--max-p99-ms N] # recommended pool sizes
#
# `optimal` and `sweep-optimal` find the **best** concurrency: the highest
# RPS where p99 is still ≤ MAX_P99_MS (default 200 ms). That's the value
# you'd want to set as the per-service pool cap — past that point the
# backend gets more RPS but PATH penalises latency, so net revenue
# probably drops.
#
# Model: we don't ask for a target RPS — we tell hey "send N requests with
# C concurrent workers, as fast as you can" and measure the RPS it
# sustained. That's the honest answer for "what can this backend handle
# at concurrency C". Find the knee by ramping C until p99 explodes or
# RPS stops growing.
#
# Output: one CSV line per run on stdout —
#   service,backend,conns,reqs,total_secs,rps,success_pct,p50_ms,p95_ms,p99_ms

set -euo pipefail

# Resolve the conf file. Default sits in localonly/ so it is never
# tracked. Operators can point BACKENDS_CONF anywhere.
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"
BACKENDS_CONF="${BACKENDS_CONF:-$REPO_ROOT/scripts/localonly/loadtest/backends.conf}"

if [[ ! -f "$BACKENDS_CONF" ]]; then
  cat >&2 <<EOF
ERROR: backends config not found at: $BACKENDS_CONF

Create it by copying the example and editing in your endpoints:

  mkdir -p $(dirname "$BACKENDS_CONF")
  cp $SCRIPT_DIR/backends.conf.example $BACKENDS_CONF
  \$EDITOR $BACKENDS_CONF

Or set BACKENDS_CONF=/path/to/your.conf
EOF
  exit 1
fi

# Source the conf. It must define:
#   SSH_HOST          (string, "" for local execution)
#   SERVICE_URL       (assoc array: service → http URL)
#   BROKEN_BACKENDS   (assoc array, optional: service → reason)
declare -A SERVICE_URL=()
declare -A BROKEN_BACKENDS=()
SSH_HOST=""
# shellcheck disable=SC1090
source "$BACKENDS_CONF"

if [[ ${#SERVICE_URL[@]} -eq 0 ]]; then
  echo "ERROR: $BACKENDS_CONF defines no SERVICE_URL entries" >&2
  exit 1
fi

HEY_IMAGE="${HEY_IMAGE:-williamyeh/hey}"
DEFAULT_REQS="${DEFAULT_REQS:-2000}"
DEFAULT_CONNS="${DEFAULT_CONNS:-100}"
RAMP_CONNS="${RAMP_CONNS:-10 50 100 250 500 1000}"
FAIL_ON_P99_MS="${FAIL_ON_P99_MS:-2000}"
FAIL_ON_SUCCESS_PCT="${FAIL_ON_SUCCESS_PCT:-98}"
# Maximum acceptable p99 (ms) for the `optimal` finder. Above this we
# assume PATH's quality router would penalise the supplier and any
# extra RPS isn't worth chasing.
MAX_P99_MS="${MAX_P99_MS:-200}"

# Service → JSON-RPC payload family.
# Most chains are EVM and accept eth_blockNumber. Cosmos chains use the
# CometBFT /status method. Sui uses its own RPC. Near uses status.
EVM_PAYLOAD='{"jsonrpc":"2.0","method":"eth_blockNumber","params":[],"id":1}'
SUI_PAYLOAD='{"jsonrpc":"2.0","method":"sui_getLatestCheckpointSequenceNumber","params":[],"id":1}'
NEAR_PAYLOAD='{"jsonrpc":"2.0","method":"status","params":[],"id":1}'
COSMOS_PAYLOAD='{"jsonrpc":"2.0","method":"status","params":[],"id":1}'

# Service → JSON-RPC payload selector.
# 13 of the 14 staked services are EVM and accept eth_blockNumber. sui
# uses its own RPC. NEAR and the cosmos chains aren't in the staked set
# so their payloads are unused here — kept for future expansion.
payload_for() {
  case "$1" in
    sui)              echo "$SUI_PAYLOAD" ;;
    near)             echo "$NEAR_PAYLOAD" ;;
    pocket|sei)       echo "$COSMOS_PAYLOAD" ;;
    *)                echo "$EVM_PAYLOAD" ;;
  esac
}

usage() {
  sed -n '3,30p' "$0"
  exit 1
}

cmd_list() {
  printf '%-22s  %-50s  %s\n' "service" "backend" "status"
  printf '%-22s  %-50s  %s\n' "-------" "-------" "------"
  for svc in $(echo "${!SERVICE_URL[@]}" | tr ' ' '\n' | sort); do
    local note="${BROKEN_BACKENDS[$svc]:-}"
    local label="OK"
    [[ -n "$note" ]] && label="BROKEN: $note"
    printf '%-22s  %-50s  %s\n' "$svc" "${SERVICE_URL[$svc]}" "$label"
  done
}

# Run a shell snippet either locally or via ssh, depending on SSH_HOST.
remote_run() {
  local snippet="$1"
  if [[ -z "$SSH_HOST" ]]; then
    bash -c "$snippet"
  else
    ssh.exe "$SSH_HOST" "$snippet"
  fi
}

# Sends one POST per backend via curl (no hey, no docker). Useful before
# a sweep to confirm every endpoint is alive and the payload is accepted.
cmd_probe() {
  local script
  script='set -e; '
  script+='EVM='"'"'{"jsonrpc":"2.0","method":"eth_blockNumber","params":[],"id":1}'"'"'; '
  script+='SUI='"'"'{"jsonrpc":"2.0","method":"sui_getLatestCheckpointSequenceNumber","params":[],"id":1}'"'"'; '
  script+='probe() { local s=$1 u=$2 p=$3; '
  script+='out=$(curl -sS -m 5 -o /tmp/.b -w "%{http_code}" -X POST '
  script+='-H "Content-Type: application/json" -d "$p" "$u" 2>&1) || true; '
  script+='body=$(head -c 80 /tmp/.b 2>/dev/null); '
  script+='printf "%-22s %-3s %s\n" "$s" "$out" "$body"; '
  script+='}; '
  for svc in $(echo "${!SERVICE_URL[@]}" | tr ' ' '\n' | sort); do
    local payload_var="EVM"
    [[ "$svc" == "sui" ]] && payload_var="SUI"
    script+="probe $svc ${SERVICE_URL[$svc]} \"\$$payload_var\"; "
  done
  remote_run "$script"
}

# Parse `hey` text output → CSV fields.
# hey prints: "Total: 0.4321 secs", "Requests/sec: N", "  50% in 0.0008 secs",
# and a "Status code distribution:" block with one "[CODE] N responses" line
# per status code. We want: total_secs, rps, success_pct, p50/p95/p99 ms.
parse_hey() {
  awk '
    /^  Total:/             { total_secs = $2 + 0 }
    /Requests\/sec:/        { rps = $2 + 0 }
    /Status code distribution/ { in_status = 1; in_lat = 0; next }
    in_status && /^  \[[0-9]+\]/ {
      # line: "  [200] 1234 responses"
      gsub(/[\[\]]/, "")
      code = $1 + 0
      n = $2 + 0
      total_resp += n
      if (code >= 200 && code < 300) ok += n
      next
    }
    in_status && NF == 0    { in_status = 0 }
    /Latency distribution/  { in_lat = 1; in_status = 0; next }
    in_lat && /^  50%/      { p50 = $3 + 0 }
    in_lat && /^  95%/      { p95 = $3 + 0 }
    in_lat && /^  99%/      { p99 = $3 + 0 }
    END {
      success = (total_resp > 0) ? (ok * 100.0 / total_resp) : 0
      printf "%.3f,%.1f,%d,%.2f,%.1f,%.1f,%.1f\n",
             total_secs, rps, total_resp, success, p50*1000, p95*1000, p99*1000
    }
  '
}

# Run one hey burst against a service. Echoes a single CSV line.
# Model: send N requests with C concurrent workers as fast as possible.
# Resulting RPS = N / total_secs is the sustained throughput at concurrency C.
run_one() {
  local svc="$1" reqs="$2" conns="$3"
  local url="${SERVICE_URL[$svc]:-}"
  if [[ -z "$url" ]]; then
    echo "ERROR: unknown service '$svc'" >&2
    return 1
  fi
  local payload
  payload="$(payload_for "$svc")"

  local raw
  raw="$(remote_run "docker run --rm --network host '$HEY_IMAGE' \
    -n $reqs -c $conns -t 10 \
    -m POST -H 'Content-Type: application/json' \
    -d '$payload' '$url'" 2>/dev/null)" || true

  local fields
  fields="$(printf '%s\n' "$raw" | parse_hey)"
  # CSV columns: service,backend,conns,reqs_target,total_secs,rps,reqs,success_pct,p50,p95,p99
  printf '%s,%s,%d,%d,%s\n' "$svc" "$url" "$conns" "$reqs" "$fields"
}

csv_header() {
  echo "service,backend,conns,reqs_target,total_secs,rps,reqs,success_pct,p50_ms,p95_ms,p99_ms"
}

# Returns 0 (true) if this row is "still healthy" — keep ramping.
is_healthy() {
  local line="$1"
  local success p99
  success="$(echo "$line" | awk -F, '{print $8}')"
  p99="$(echo "$line" | awk -F, '{print $11}')"
  awk -v s="$success" -v p="$p99" -v fp="$FAIL_ON_P99_MS" -v fs="$FAIL_ON_SUCCESS_PCT" \
      'BEGIN { exit !(s >= fs && p <= fp) }'
}

cmd_test() {
  local svc="${1:-}"; shift || true
  local reqs="$DEFAULT_REQS" conns="$DEFAULT_CONNS"
  while [[ $# -gt 0 ]]; do
    case "$1" in
      --reqs)  reqs="$2"; shift 2 ;;
      --conns) conns="$2"; shift 2 ;;
      *) echo "unknown flag $1" >&2; exit 1 ;;
    esac
  done
  [[ -z "$svc" ]] && { echo "service required" >&2; exit 1; }
  csv_header
  run_one "$svc" "$reqs" "$conns"
}

# Ramp concurrency until p99/success degrades, then report the best healthy
# RPS observed for this service.
cmd_ramp() {
  local svc="${1:-}"
  [[ -z "$svc" ]] && { echo "service required" >&2; exit 1; }
  csv_header
  local best_rps="0" best_line=""
  for c in $RAMP_CONNS; do
    local line; line="$(run_one "$svc" "$DEFAULT_REQS" "$c")"
    echo "$line"
    if is_healthy "$line"; then
      local rps; rps="$(echo "$line" | awk -F, '{print $6}')"
      awk -v a="$rps" -v b="$best_rps" 'BEGIN { exit !(a > b) }' && {
        best_rps="$rps"; best_line="$line"
      }
    else
      echo "# $svc: degraded at c=$c, stopping ramp" >&2
      break
    fi
  done
  if [[ -n "$best_line" ]]; then
    echo "# $svc: max sustained RPS = $best_rps" >&2
    echo "# best: $best_line" >&2
  fi
}

# Find the best concurrency for one service: the highest RPS where p99
# is still within MAX_P99_MS. Echoes one CSV summary line.
find_optimal() {
  local svc="$1" max_p99="$2"
  local best_rps="0" best_c="0" best_p99="0"
  for c in $RAMP_CONNS; do
    local line; line="$(run_one "$svc" "$DEFAULT_REQS" "$c")"
    echo "$line"
    local success rps p99
    success="$(echo "$line" | awk -F, '{print $8}')"
    rps="$(echo "$line" | awk -F, '{print $6}')"
    p99="$(echo "$line" | awk -F, '{print $11}')"
    # Must be healthy AND under p99 budget AND beat current best.
    if awk -v s="$success" -v fs="$FAIL_ON_SUCCESS_PCT" \
            -v p="$p99" -v fp="$max_p99" \
            -v r="$rps" -v br="$best_rps" \
            'BEGIN { exit !(s >= fs && p <= fp && r > br) }'; then
      best_rps="$rps"; best_c="$c"; best_p99="$p99"
    fi
  done
  printf '# OPTIMAL %-22s rps=%s conns=%s p99=%sms (budget %sms)\n' \
    "$svc" "$best_rps" "$best_c" "$best_p99" "$max_p99" >&2
  # Echo a parsable line on stdout for sweep-optimal to aggregate.
  printf 'OPTIMAL,%s,%s,%s,%s\n' "$svc" "$best_c" "$best_rps" "$best_p99"
}

cmd_optimal() {
  local svc="${1:-}"; shift || true
  local max_p99="$MAX_P99_MS"
  while [[ $# -gt 0 ]]; do
    case "$1" in
      --max-p99-ms) max_p99="$2"; shift 2 ;;
      *) echo "unknown flag $1" >&2; exit 1 ;;
    esac
  done
  [[ -z "$svc" ]] && { echo "service required" >&2; exit 1; }
  csv_header
  find_optimal "$svc" "$max_p99"
}

cmd_sweep_optimal() {
  local max_p99="$MAX_P99_MS"
  local include_broken=0
  while [[ $# -gt 0 ]]; do
    case "$1" in
      --max-p99-ms)     max_p99="$2"; shift 2 ;;
      --include-broken) include_broken=1; shift ;;
      *) echo "unknown flag $1" >&2; exit 1 ;;
    esac
  done
  csv_header
  declare -A SVC_OPT_RPS SVC_OPT_C SVC_OPT_P99
  for svc in $(echo "${!SERVICE_URL[@]}" | tr ' ' '\n' | sort); do
    if [[ -n "${BROKEN_BACKENDS[$svc]:-}" && $include_broken -eq 0 ]]; then
      echo "# === $svc SKIPPED (broken: ${BROKEN_BACKENDS[$svc]}) ===" >&2
      continue
    fi
    echo "# === $svc (max_p99=${max_p99}ms) ===" >&2
    while IFS= read -r line; do
      if [[ "$line" == OPTIMAL,* ]]; then
        IFS=, read -r _ s c r p <<<"$line"
        SVC_OPT_C[$s]="$c"; SVC_OPT_RPS[$s]="$r"; SVC_OPT_P99[$s]="$p"
      else
        echo "$line"
      fi
    done < <(find_optimal "$svc" "$max_p99")
  done
  echo >&2
  echo "# === recommended pool config (max_p99 ≤ ${max_p99}ms) ===" >&2
  printf '# %-22s %-12s %-12s %s\n' "service" "max_conns" "rps" "p99_ms" >&2
  for svc in $(echo "${!SVC_OPT_RPS[@]}" | tr ' ' '\n' | sort); do
    printf '# %-22s %-12s %-12s %s\n' \
      "$svc" "${SVC_OPT_C[$svc]}" "${SVC_OPT_RPS[$svc]}" "${SVC_OPT_P99[$svc]}" >&2
  done
}

cmd_sweep() {
  local include_broken=0
  [[ "${1:-}" == "--include-broken" ]] && include_broken=1
  csv_header
  printf '# sweep summary will be appended at the end\n' >&2
  declare -A SVC_BEST_RPS SVC_BEST_LINE
  for svc in $(echo "${!SERVICE_URL[@]}" | tr ' ' '\n' | sort); do
    if [[ -n "${BROKEN_BACKENDS[$svc]:-}" && $include_broken -eq 0 ]]; then
      echo "# === $svc SKIPPED (broken: ${BROKEN_BACKENDS[$svc]}) ===" >&2
      continue
    fi
    echo "# === $svc ===" >&2
    local best_rps="0" best_line=""
    for c in $RAMP_CONNS; do
      local line; line="$(run_one "$svc" "$DEFAULT_REQS" "$c")"
      echo "$line"
      if is_healthy "$line"; then
        local rps; rps="$(echo "$line" | awk -F, '{print $6}')"
        awk -v a="$rps" -v b="$best_rps" 'BEGIN { exit !(a > b) }' && {
          best_rps="$rps"; best_line="$line"
        }
      else
        break
      fi
    done
    SVC_BEST_RPS[$svc]="$best_rps"
    SVC_BEST_LINE[$svc]="$best_line"
  done
  echo >&2
  echo "# === sweep summary: max sustained RPS per backend ===" >&2
  printf '# %-22s %s\n' "service" "max_rps" >&2
  for svc in $(echo "${!SVC_BEST_RPS[@]}" | tr ' ' '\n' | sort); do
    printf '# %-22s %s\n' "$svc" "${SVC_BEST_RPS[$svc]}" >&2
  done
}

case "${1:-}" in
  list)          cmd_list ;;
  probe)         cmd_probe ;;
  test)          shift; cmd_test "$@" ;;
  ramp)          shift; cmd_ramp "$@" ;;
  optimal)       shift; cmd_optimal "$@" ;;
  sweep)         shift; cmd_sweep "$@" ;;
  sweep-optimal) shift; cmd_sweep_optimal "$@" ;;
  ""|-h|--help)  usage ;;
  *) echo "unknown command: $1" >&2; usage ;;
esac
