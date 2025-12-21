# Testing Guide

This document describes how to verify the relay lifecycle is working correctly.

## Prerequisites

- Tilt localnet running (`tilt up`)
- PATH gateway accessible at `localhost:3069`
- Redis accessible at `localhost:6379`
- `hey` load testing tool installed (`go install github.com/rakyll/hey@latest`)

## Quick Verification Test

This test verifies the complete relay lifecycle:
1. Relays are published to Redis streams
2. Miners consume relays and create sessions
3. Claims are submitted on-chain
4. Proofs are submitted (when required)
5. Sessions settle successfully

### Step 1: Check Baseline State

```bash
# Check supplier distribution (should show 3 miners with ~5 suppliers each)
# Note: Suppliers are auto-discovered from keyring keys
pocket-relay-miner redis supplier --claims --redis redis://localhost:6379

# Check current block height
curl -s http://localhost:26657/status | jq -r '.result.sync_info.latest_block_height'
```

### Step 2: Send Test Relays

```bash
# Send 3000 relays via PATH with 50 concurrent workers
hey -n 3000 -c 50 \
  -H "Target-Service-Id: develop-http" \
  -H "Content-Type: application/json" \
  -m POST \
  -d '{"jsonrpc":"2.0","method":"eth_blockNumber","params":[],"id":1}' \
  http://localhost:3069/v1
```

Expected output:
- All 3000 requests should return status code 200
- Throughput should be 150-300 RPS depending on hardware

### Step 3: Monitor Relay Consumption

```bash
# Run the lifecycle monitor
./scripts/monitor-relay-lifecycle.sh

# Or check manually for a specific supplier
pocket-relay-miner redis streams --supplier pokt1... --redis redis://localhost:6379
pocket-relay-miner redis sessions --supplier pokt1... --redis redis://localhost:6379
```

Expected state after sending relays:
- All suppliers should show PENDING = 0 (relays consumed)
- Each supplier should have 1 session with ~200 relays
- Total should match number of relays sent

### Step 4: Wait for Claim/Proof Windows

Session timing (from genesis):
- **Blocks per session**: 10
- **Claim window**: Opens 1 block after session end, closes 4 blocks later
- **Proof window**: Opens when claim closes, closes 4 blocks later

Example for session 580-589:
- Claim window: blocks 590-594
- Proof window: blocks 594-598
- Settlement: after block 598

### Step 5: Verify Final State

```bash
# Check all sessions are settled
./scripts/monitor-relay-lifecycle.sh
```

Expected final state:
- All sessions in "settled" state
- Relay counts preserved (no data loss)
- No LATE_RELAY warnings in miner logs

### Step 6: Check for Issues

```bash
# Check for late relay warnings (should be none)
kubectl logs -l app=miner --tail=500 | grep -i "LATE"

# Check for claim/proof submissions
kubectl logs -l app=miner --tail=100 | grep -E "claims submitted|proofs submitted"
```

## Monitoring Commands Reference

### Supplier Distribution
```bash
go run main.go redis supplier --claims --redis redis://localhost:6379
```

### Session States
```bash
# All sessions for a supplier
go run main.go redis sessions --supplier pokt1... --redis redis://localhost:6379

# Sessions by state
go run main.go redis sessions --supplier pokt1... --state active --redis redis://localhost:6379
go run main.go redis sessions --supplier pokt1... --state claimed --redis redis://localhost:6379
go run main.go redis sessions --supplier pokt1... --state settled --redis redis://localhost:6379
```

### Stream Consumption
```bash
go run main.go redis streams --supplier pokt1... --redis redis://localhost:6379
```

### Real-time Lifecycle Monitor
```bash
./scripts/monitor-relay-lifecycle.sh          # One-shot
./scripts/monitor-relay-lifecycle.sh --watch  # Continuous
```

## Expected Test Results

| Metric | Expected |
|--------|----------|
| Relay consumption | 100% (0 pending) |
| Session creation | 1 per supplier |
| Claim submission | 100% success |
| Proof submission | ~25% (random sampling) |
| Late relays | 0 |
| Final state | All settled |

## Troubleshooting

### Relays not being consumed
- Check miner logs for errors
- Verify Redis streams exist: `redis streams --supplier X`
- Check consumer group status in stream output

### Claims not submitted
- Check block height vs claim window timing
- Look for tx errors in miner logs
- Verify validator is producing blocks

### Late relay warnings
- Indicates relays arrived after claim submission
- Check relay throughput and backend latency
- May indicate slow consumption from streams
