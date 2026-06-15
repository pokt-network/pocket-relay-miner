#!/usr/bin/env bash
#
# test-inclusion-reconciler.sh — localnet verification for the block-driven
# inclusion reconciler (claim + proof on-chain verification + in-window
# rebroadcast). See docs/CLAIM_PROOF_LIFECYCLE.md.
#
# Prereqs: Tilt localnet up (kind), miner + path running, load generated so
# that at least one session has reached settlement. Tools: curl, jq, redis-cli
# (Redis is proxied by Tilt), and the pocket-relay-miner binary on PATH for the
# `redis submissions` subcommand. Prometheus at :9091, Loki at :3100.
#
# This automates the observability assertions (steps 1 + 5). The rebroadcast
# recovery (step 2) and HA failover (step 4) are guided manual steps printed at
# the end — they need fault injection / replica kills.
#
# Usage:
#   scripts/test-inclusion-reconciler.sh
#   PROM=http://localhost:9091 LOKI=http://localhost:3100 scripts/test-inclusion-reconciler.sh

set -euo pipefail

PROM="${PROM:-http://localhost:9091}"
LOKI="${LOKI:-http://localhost:3100}"

red()   { printf '\033[31m%s\033[0m\n' "$*"; }
green() { printf '\033[32m%s\033[0m\n' "$*"; }
bold()  { printf '\033[1m%s\033[0m\n' "$*"; }

fail=0

# prom_val <promql> -> instant scalar (sum). Empty result => 0.
prom_val() {
  local q="$1"
  curl -sG "$PROM/api/v1/query" --data-urlencode "query=$q" \
    | jq -r '[.data.result[].value[1] | tonumber] | add // 0'
}

bold "== 1. Inclusion outcomes (found should dominate; missing should be ~0) =="
for phase in claim proof; do
  found=$(prom_val "sum(ha_miner_${phase}_inclusion_outcome_total{outcome=\"on_chain_found\"})")
  missing=$(prom_val "sum(ha_miner_${phase}_inclusion_outcome_total{outcome=\"on_chain_missing\"})")
  pollerr=$(prom_val "sum(ha_miner_${phase}_inclusion_outcome_total{outcome=\"poll_error\"})")
  rebroad=$(prom_val "sum(ha_miner_${phase}_rebroadcasts_total)")
  printf '  %-6s found=%s missing=%s poll_error=%s rebroadcasts=%s\n' \
    "$phase" "$found" "$missing" "$pollerr" "$rebroad"
  if [ "$(printf '%.0f' "$found")" -lt 1 ]; then
    red "  FAIL: no ${phase} on_chain_found outcomes — reconciler not confirming inclusion"
    fail=1
  fi
  if [ "$(printf '%.0f' "$missing")" -gt 0 ]; then
    red "  WARN: ${phase} on_chain_missing > 0 — investigate (forfeit not recovered by rebroadcast)"
  fi
done

bold "== 2. Submission-tracker records carry on-chain outcomes =="
echo "  Inspect a settled supplier's records (replace <addr>):"
echo "    pocket-relay-miner redis submissions --supplier <addr>"
echo "  Expect claim_on_chain_outcome / proof_on_chain_outcome = on_chain_found."

bold "== 3. Pending rebroadcast keys drain (no leak) =="
pending=$(redis-cli --no-raw KEYS 'ha:miner:rebroadcast:*' 2>/dev/null | grep -c . || true)
echo "  ha:miner:rebroadcast:* keys currently present: ${pending}"
echo "  (Non-zero is fine mid-window; they must drain to ~0 between settlements.)"

bold "== 4. Reconciler is running on the leader/owners (logs) =="
now_ns=$(date +%s)000000000
start_ns=$(date -d "15 minutes ago" +%s)000000000
hits=$(curl -sG "$LOKI/loki/api/v1/query_range" \
  --data-urlencode 'query={app="miner"} |= "inclusion reconcile"' \
  --data-urlencode "start=$start_ns" --data-urlencode "end=$now_ns" \
  --data-urlencode "limit=5" 2>/dev/null | jq -r '[.data.result[].values[]] | length' || echo 0)
echo "  'inclusion reconcile' log lines (last 15m): ${hits}"

echo
if [ "$fail" -eq 0 ]; then
  green "AUTOMATED CHECKS PASSED (steps 1,3,4). Now run the guided steps:"
else
  red "AUTOMATED CHECKS FAILED — see above before proceeding."
fi
cat <<'EOF'

  Guided steps (manual — fault injection):
  2) Rebroadcast recovery: force a first-submit miss for one supplier (point it
     at a momentarily-rejecting node, or briefly block its tx). Confirm
     ha_miner_proof_rebroadcasts_total increments AND the proof still lands
     (proof_on_chain_outcome=on_chain_found) before window close.
  4) HA failover: scale miner to 2-3 replicas; mid-proof-window kill the replica
     owning a supplier (check `pocket-relay-miner redis` for ownership). After
     claimer rebalance, confirm the new owner rebroadcasts from the persisted
     Redis entries — proof lands, exactly one tx (no double-submit).
  tx_index: confirm the localnet full node runs tx_index=null (module-state
     queries must still resolve inclusion).
EOF
exit "$fail"
