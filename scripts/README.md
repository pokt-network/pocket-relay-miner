# Integration Testing Scripts

Automated testing scripts for pocket-relay-miner integration testing with Tilt.

## Quick Start

```bash
# Start Tilt first (in a separate terminal)
tilt up

# Run all integration tests
./scripts/test-all.sh

# Or run individual tests
./scripts/test-simple-relay.sh
./scripts/test-load-relay.sh
./scripts/test-claim-proof.sh
```

## Prerequisites

- **Tilt** - Running with localnet deployment (`tilt up`)
- **jq** - JSON parsing (`apt install jq` or `brew install jq`)
- **curl** - HTTP requests (usually pre-installed)
- **nc** (netcat) - Port checking (usually pre-installed)
- **pocket-relay-miner** - Built binary (`make build`)

## Scripts Overview

### `test-all.sh` - Run All Tests

Orchestration script that runs all integration tests in sequence.

```bash
./scripts/test-all.sh [options]

Options:
  --quick       Use reduced counts for faster testing
  --skip-load   Skip load test
  --help        Show help message

Examples:
  ./scripts/test-all.sh                 # Full test suite
  ./scripts/test-all.sh --quick         # Quick mode (reduced counts)
  ./scripts/test-all.sh --skip-load     # Skip load test
```

**Test Sequence:**
1. Wait for Tilt deployment readiness
2. Simple relay test (all protocols)
3. Load test (configurable)
4. Claim+proof lifecycle test

### `test-tilt-ready.sh` - Tilt Readiness Check

Waits for Tilt deployment to be fully ready before running tests.

```bash
./scripts/test-tilt-ready.sh [options]

Options:
  --timeout <seconds>   Timeout for waiting (default: 300)
  --help                Show help message
```

**Checks:**
- Tilt UI responsive (localhost:10350)
- All Tilt resources ready
- Redis port (6379)
- Validator RPC port (26657)
- Validator gRPC port (9090)
- Relayer HTTP port (8180)

### `test-simple-relay.sh` - Simple Relay Test

Sends one relay request per protocol to verify basic functionality.

```bash
./scripts/test-simple-relay.sh [options]

Options:
  --service <id>         Service ID (default: develop)
  --protocols <list>     Comma-separated protocols (default: jsonrpc,websocket,grpc,stream)
  --help                 Show help message

Examples:
  ./scripts/test-simple-relay.sh                           # Test all protocols
  ./scripts/test-simple-relay.sh --service eth-mainnet     # Test on eth-mainnet
  ./scripts/test-simple-relay.sh --protocols jsonrpc,grpc  # Test only HTTP and gRPC
```

**Protocols Tested:**
- `jsonrpc` - HTTP/JSON-RPC
- `websocket` - WebSocket
- `grpc` - gRPC
- `stream` - REST/Streaming (SSE/NDJSON)

### `test-load-relay.sh` - Load Test

Runs high-volume concurrent relay requests to test performance.

```bash
./scripts/test-load-relay.sh [options]

Options:
  --protocol <name>       Protocol to test (default: jsonrpc)
  --count, -n <num>       Number of requests (default: 10000)
  --concurrency, -c <num> Concurrent workers (default: 100)
  --service <id>          Service ID (default: develop)
  --help                  Show help message

Examples:
  ./scripts/test-load-relay.sh                                # 10k jsonrpc, 100 workers
  ./scripts/test-load-relay.sh -n 50000 -c 200                # 50k requests, 200 workers
  ./scripts/test-load-relay.sh --protocol websocket -n 5000   # 5k WebSocket requests
```

**Metrics Displayed:**
- Total requests
- Success rate
- Error rate
- Latency percentiles (p50, p95, p99)
- Throughput (req/sec)

### `test-claim-proof.sh` - Claim+Proof Lifecycle Test

Monitors full session lifecycle from relay generation through claim and proof submission.

```bash
./scripts/test-claim-proof.sh [options]

Options:
  --supplier <address>   Supplier operator address (default: localnet supplier 1)
  --service <id>         Service ID (default: develop)
  --relay-count <num>    Number of relays to send (default: 100)
  --wait-timeout <sec>   Max time to wait for settlement (default: 600)
  --help                 Show help message

Examples:
  ./scripts/test-claim-proof.sh                    # Use defaults
  ./scripts/test-claim-proof.sh --relay-count 200  # Send 200 relays
```

**Lifecycle Phases:**
1. **Generate Relays** - Send relays to create active session
2. **Monitor State** - Track transitions: active → claiming → claimed → proving → settled
3. **Verify Claim** - Check claim submission and SMST root hash
4. **Verify Proof** - Check proof submission (or skip if not required)
5. **Summary** - Display timeline and verification checklist

## Library Functions

### `lib/common.sh`

Shared utility functions used by all scripts.

**Functions:**
- `log_info()`, `log_success()`, `log_error()`, `log_warn()` - Colored logging
- `wait_for_port()` - Wait for TCP port to be open
- `parse_json()` - Parse JSON using jq
- `format_duration()` - Format milliseconds to human-readable
- `require_commands()` - Check for required commands
- `find_miner_binary()` - Locate pocket-relay-miner binary

### `lib/tilt-helpers.sh`

Tilt API interaction helpers.

**Functions:**
- `check_tilt_running()` - Check if Tilt is running
- `wait_for_tilt_ui()` - Wait for Tilt UI to respond
- `wait_for_tilt_ready()` - Wait for all resources to be ready
- `list_tilt_resources()` - Display resource statuses

### `lib/metrics-helpers.sh`

Prometheus metrics query helpers.

**Functions:**
- `query_metric()` - Query Prometheus metric by name
- `get_metric_value()` - Get single metric value
- `get_metric_sum()` - Sum all values for a metric
- `get_claims_submitted()` - Get claims count for supplier
- `get_proofs_submitted()` - Get proofs count for supplier
- `wait_for_metric()` - Wait for metric to reach value

## Localnet Configuration

Scripts use `--localnet` flag which auto-populates these defaults:

- **App Private Key:** `2d00ef074d9b51e46886dc9a1df11e7b986611d0f336bdcf1f0adce3e037ec0a`
- **Supplier:** `pokt19a3t4yunp0dlpfjrp7qwnzwlrzd5fzs2gjaaaj`
- **gRPC Endpoint:** `localhost:9090`
- **RPC Endpoint:** `http://localhost:26657`
- **Chain ID:** `poktroll`
- **Relayer URL:** `http://localhost:8180`
- **Default Service:** `develop`

## Monitoring Endpoints

- **Tilt UI:** http://localhost:10350
- **Validator RPC:** http://localhost:26657
- **Validator gRPC:** localhost:9090
- **Relayer HTTP:** http://localhost:8180
- **Prometheus Metrics:** http://localhost:9090/metrics

## Exit Codes

Scripts use consistent exit codes:

- **0** - Success
- **1** - General failure / Claim submission failed
- **2** - Proof submission failed (when required)
- **3** - Timeout waiting for settlement

## Example Workflows

### Quick Smoke Test

```bash
# Quick verification that everything works
./scripts/test-all.sh --quick --skip-load
```

### Full Integration Test

```bash
# Comprehensive test suite
./scripts/test-all.sh
```

### Performance Testing

```bash
# High-volume load test
./scripts/test-load-relay.sh --count 100000 --concurrency 500
```

### Claim/Proof Validation

```bash
# Detailed session lifecycle monitoring
./scripts/test-claim-proof.sh --relay-count 200 --wait-timeout 900
```

### Protocol-Specific Testing

```bash
# Test only WebSocket
./scripts/test-simple-relay.sh --protocols websocket

# Load test WebSocket
./scripts/test-load-relay.sh --protocol websocket --count 20000
```

## Troubleshooting

### Tilt Not Ready

```bash
# Check Tilt status
tilt up

# View Tilt logs
tilt logs

# Check specific resource
tilt get all
```

### Tests Failing

```bash
# Check if services are running
./scripts/test-tilt-ready.sh

# Check miner logs (via Tilt UI)
# http://localhost:10350

# Check Redis
redis-cli ping

# Check relayer
curl http://localhost:8180/health
```

### Session Not Settling

```bash
# Monitor session manually
./bin/pocket-relay-miner redis-debug sessions \
  --supplier pokt19a3t4yunp0dlpfjrp7qwnzwlrzd5fzs2gjaaaj

# Check metrics
curl http://localhost:9090/metrics | grep "ha_miner"
```

### Quick Diagnosis Commands

```bash
# Is the gateway receiving requests?
./scripts/debug-relay-pipeline.sh logs-gateway --tail 20 | grep -i relay

# Are relayers processing requests?
./scripts/debug-relay-pipeline.sh logs-relayers --tail 20 | grep -i "relay processed"

# Check relayer performance
./scripts/debug-relay-pipeline.sh metrics-relayers | grep relays_total

# Check if relays are in Redis WAL
./scripts/debug-relay-pipeline.sh redis-streams

# Check active sessions
./scripts/debug-relay-pipeline.sh redis-sessions
```

## Development

### Adding New Tests

1. Create script in `scripts/` directory
2. Source `lib/common.sh` for utilities
3. Add `--help` flag for documentation
4. Use consistent exit codes
5. Add colored output for better UX
6. Update `test-all.sh` to include new test

### Script Template

```bash
#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "$SCRIPT_DIR/lib/common.sh"

main() {
    print_header "My Test"

    require_commands curl jq || exit 1

    # Test logic here

    log_success "Test passed!"
    exit 0
}

main "$@"
```

## Contributing

When modifying scripts:
- Follow existing patterns and conventions
- Add proper error handling
- Include helpful log messages
- Update this README
- Test with `--quick` and full modes
