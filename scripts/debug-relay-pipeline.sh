#!/usr/bin/env bash
#
# Debug relay pipeline across platform and nodes contexts
#
# This script provides debugging commands for the relay flow:
#   relay-router-mainnet (platform) → relayers (nodes) → blockchain
#
# Usage:
#   ./scripts/debug-relay-pipeline.sh <command> [options]
#
# Commands:
#   status              Show status of all components
#   logs-gateway        Show relay-router-mainnet logs (platform context)
#   logs-relayers       Show relayer pod logs (nodes context)
#   logs-miners         Show miner pod logs (nodes context)
#   metrics-relayers    Show relayer metrics (nodes context)
#   redis-streams       Check Redis streams (nodes context)
#   redis-sessions      Check active sessions (nodes context)
#   trace               Run full pipeline trace
#   watch               Watch relay flow in real-time
#
# Examples:
#   ./scripts/debug-relay-pipeline.sh status
#   ./scripts/debug-relay-pipeline.sh logs-gateway --tail 100
#   ./scripts/debug-relay-pipeline.sh trace

set -euo pipefail

# Contexts
PLATFORM_CTX="${PLATFORM_CTX:-platform}"
NODES_CTX="${NODES_CTX:-nodes}"

# Namespaces
GATEWAY_NS="${GATEWAY_NS:-mainnet-gateway}"
NODES_NS="${NODES_NS:-mainnet}"

# Component labels
GATEWAY_LABEL="app=relay-router-mainnet"
RELAYER_LABEL="app=relayer"
MINER_LABEL="app=miner"
REDIS_LABEL="app=redis"

# Colors
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m' # No Color

# Helper functions
print_header() {
  echo ""
  echo -e "${BLUE}============================================${NC}"
  echo -e "${BLUE}$1${NC}"
  echo -e "${BLUE}============================================${NC}"
}

print_success() {
  echo -e "${GREEN}✓${NC} $1"
}

print_error() {
  echo -e "${RED}✗${NC} $1"
}

print_warning() {
  echo -e "${YELLOW}⚠${NC} $1"
}

# Command: status
cmd_status() {
  print_header "Relay Pipeline Status"

  echo ""
  echo "=== Platform Context: ${PLATFORM_CTX} ==="
  echo "Gateway Pods (${GATEWAY_NS}):"
  kubectl --context="${PLATFORM_CTX}" -n "${GATEWAY_NS}" get pods -l "${GATEWAY_LABEL}" -o wide || print_error "Failed to get gateway pods"

  echo ""
  echo "=== Nodes Context: ${NODES_CTX} ==="
  echo "Relayer Pods (${NODES_NS}):"
  kubectl --context="${NODES_CTX}" -n "${NODES_NS}" get pods -l "${RELAYER_LABEL}" -o wide || print_error "Failed to get relayer pods"

  echo ""
  echo "Miner Pods (${NODES_NS}):"
  kubectl --context="${NODES_CTX}" -n "${NODES_NS}" get pods -l "${MINER_LABEL}" -o wide || print_error "Failed to get miner pods"

  echo ""
  echo "Redis Pods (${NODES_NS}):"
  kubectl --context="${NODES_CTX}" -n "${NODES_NS}" get pods -l "${REDIS_LABEL}" -o wide || print_error "Failed to get Redis pods"
}

# Command: logs-gateway
cmd_logs_gateway() {
  TAIL_LINES="${1:-50}"
  print_header "Relay Router Gateway Logs (last ${TAIL_LINES} lines)"

  GATEWAY_POD=$(kubectl --context="${PLATFORM_CTX}" -n "${GATEWAY_NS}" get pods -l "${GATEWAY_LABEL}" -o jsonpath='{.items[0].metadata.name}' 2>/dev/null)

  if [ -z "$GATEWAY_POD" ]; then
    print_error "No gateway pod found in ${PLATFORM_CTX}/${GATEWAY_NS}"
    return 1
  fi

  echo "Pod: ${GATEWAY_POD}"
  echo ""
  kubectl --context="${PLATFORM_CTX}" -n "${GATEWAY_NS}" logs "${GATEWAY_POD}" --tail="${TAIL_LINES}" "$@"
}

# Command: logs-relayers
cmd_logs_relayers() {
  TAIL_LINES="${1:-50}"
  print_header "Relayer Logs (last ${TAIL_LINES} lines per pod)"

  kubectl --context="${NODES_CTX}" -n "${NODES_NS}" logs -l "${RELAYER_LABEL}" --tail="${TAIL_LINES}" --prefix --max-log-requests=10 "$@"
}

# Command: logs-miners
cmd_logs_miners() {
  TAIL_LINES="${1:-50}"
  print_header "Miner Logs (last ${TAIL_LINES} lines per pod)"

  kubectl --context="${NODES_CTX}" -n "${NODES_NS}" logs -l "${MINER_LABEL}" --tail="${TAIL_LINES}" --prefix --max-log-requests=10 "$@"
}

# Command: metrics-relayers
cmd_metrics_relayers() {
  print_header "Relayer Metrics"

  RELAYER_PODS=($(kubectl --context="${NODES_CTX}" -n "${NODES_NS}" get pods -l "${RELAYER_LABEL}" -o jsonpath='{.items[*].metadata.name}'))

  if [ ${#RELAYER_PODS[@]} -eq 0 ]; then
    print_error "No relayer pods found"
    return 1
  fi

  for POD in "${RELAYER_PODS[@]}"; do
    echo ""
    echo "=== ${POD} ==="
    kubectl --context="${NODES_CTX}" -n "${NODES_NS}" exec "${POD}" -- curl -s http://localhost:9090/metrics 2>/dev/null | grep -E "^ha_relayer_(relays_total|relay_duration|relay_meter)" || echo "No metrics available"
  done
}

# Command: redis-streams
cmd_redis_streams() {
  print_header "Redis Streams (WAL)"

  REDIS_POD=$(kubectl --context="${NODES_CTX}" -n "${NODES_NS}" get pods -l "${REDIS_LABEL}" -o jsonpath='{.items[0].metadata.name}' 2>/dev/null)

  if [ -z "$REDIS_POD" ]; then
    print_error "No Redis pod found"
    return 1
  fi

  echo "Checking streams for all suppliers..."
  echo ""

  # Get all stream keys
  kubectl --context="${NODES_CTX}" -n "${NODES_NS}" exec "${REDIS_POD}" -- redis-cli KEYS "ha:relays:*" | while read -r stream_key; do
    if [ -n "$stream_key" ]; then
      echo "Stream: ${stream_key}"
      kubectl --context="${NODES_CTX}" -n "${NODES_NS}" exec "${REDIS_POD}" -- redis-cli XLEN "${stream_key}"
    fi
  done
}

# Command: redis-sessions
cmd_redis_sessions() {
  print_header "Active Sessions in Redis"

  REDIS_POD=$(kubectl --context="${NODES_CTX}" -n "${NODES_NS}" get pods -l "${REDIS_LABEL}" -o jsonpath='{.items[0].metadata.name}' 2>/dev/null)

  if [ -z "$REDIS_POD" ]; then
    print_error "No Redis pod found"
    return 1
  fi

  echo "Active sessions:"
  kubectl --context="${NODES_CTX}" -n "${NODES_NS}" exec "${REDIS_POD}" -- redis-cli KEYS "ha:miner:sessions:*:index" | while read -r index_key; do
    if [ -n "$index_key" ]; then
      SESSION_COUNT=$(kubectl --context="${NODES_CTX}" -n "${NODES_NS}" exec "${REDIS_POD}" -- redis-cli SCARD "${index_key}")
      echo "  ${index_key}: ${SESSION_COUNT} sessions"
    fi
  done
}

# Command: trace
cmd_trace() {
  print_header "Full Pipeline Trace"

  echo "1. Checking gateway status..."
  kubectl --context="${PLATFORM_CTX}" -n "${GATEWAY_NS}" get pods -l "${GATEWAY_LABEL}" -o wide

  echo ""
  echo "2. Checking relayer status..."
  kubectl --context="${NODES_CTX}" -n "${NODES_NS}" get pods -l "${RELAYER_LABEL}" -o wide

  echo ""
  echo "3. Recent gateway logs..."
  cmd_logs_gateway 20

  echo ""
  echo "4. Recent relayer logs..."
  cmd_logs_relayers 20

  echo ""
  echo "5. Relayer metrics..."
  cmd_metrics_relayers

  echo ""
  echo "6. Redis streams..."
  cmd_redis_streams

  print_success "Trace complete"
}

# Command: watch
cmd_watch() {
  print_header "Watching Relay Flow (press Ctrl+C to stop)"

  echo "Gateway logs:"
  kubectl --context="${PLATFORM_CTX}" -n "${GATEWAY_NS}" logs -l "${GATEWAY_LABEL}" -f --tail=10 &
  PID_GATEWAY=$!

  echo ""
  echo "Relayer logs:"
  kubectl --context="${NODES_CTX}" -n "${NODES_NS}" logs -l "${RELAYER_LABEL}" -f --tail=10 --prefix &
  PID_RELAYER=$!

  # Cleanup on exit
  trap "kill ${PID_GATEWAY} ${PID_RELAYER} 2>/dev/null" EXIT

  wait
}

# Main
COMMAND="${1:-}"
shift || true

case "$COMMAND" in
  status)
    cmd_status "$@"
    ;;
  logs-gateway)
    cmd_logs_gateway "$@"
    ;;
  logs-relayers)
    cmd_logs_relayers "$@"
    ;;
  logs-miners)
    cmd_logs_miners "$@"
    ;;
  metrics-relayers)
    cmd_metrics_relayers "$@"
    ;;
  redis-streams)
    cmd_redis_streams "$@"
    ;;
  redis-sessions)
    cmd_redis_sessions "$@"
    ;;
  trace)
    cmd_trace "$@"
    ;;
  watch)
    cmd_watch "$@"
    ;;
  "")
    echo "Error: No command specified"
    echo ""
    grep '^#' "$0" | grep -v '#!/usr/bin/env' | sed 's/^# \?//'
    exit 1
    ;;
  *)
    echo "Error: Unknown command '$COMMAND'"
    echo ""
    grep '^#' "$0" | grep -v '#!/usr/bin/env' | sed 's/^# \?//'
    exit 1
    ;;
esac
