# Multi-Context Relay Debugging - Quick Reference

This guide provides quick commands for debugging relay issues across `platform` and `nodes` Kubernetes contexts.

## Architecture

```
relay-router-mainnet (platform/mainnet-gateway)
    ↓
    → relayers (nodes/mainnet) [4 replicas]
    → miners (nodes/mainnet) [4 replicas]
    → Redis (nodes/mainnet)
    → Blockchain
```

## Quick Commands

### 1. Send Test Relay

```bash
# Basic test
./scripts/test-relay-gateway.sh

# With verbose output
./scripts/test-relay-gateway.sh --verbose

# Custom URL
GATEWAY_URL=https://staging.example.com ./scripts/test-relay-gateway.sh
```

### 2. Check Overall Status

```bash
# See all components
./scripts/debug-relay-pipeline.sh status

# Custom contexts
PLATFORM_CTX=staging NODES_CTX=staging-nodes \
  ./scripts/debug-relay-pipeline.sh status
```

### 3. View Logs

```bash
# Gateway logs (platform)
./scripts/debug-relay-pipeline.sh logs-gateway --tail 100

# Relayer logs (nodes)
./scripts/debug-relay-pipeline.sh logs-relayers --tail 50

# Miner logs (nodes)
./scripts/debug-relay-pipeline.sh logs-miners --tail 50

# Follow logs in real-time
./scripts/debug-relay-pipeline.sh logs-gateway -f
```

### 4. Check Metrics

```bash
# Relayer metrics
./scripts/debug-relay-pipeline.sh metrics-relayers

# Specific metric
./scripts/debug-relay-pipeline.sh metrics-relayers | grep relays_total
```

### 5. Check Redis

```bash
# Check Redis streams (WAL)
./scripts/debug-relay-pipeline.sh redis-streams

# Check active sessions
./scripts/debug-relay-pipeline.sh redis-sessions
```

### 6. Full Pipeline Trace

```bash
# Complete trace
./scripts/debug-relay-pipeline.sh trace
```

### 7. Watch Live

```bash
# Watch all logs in real-time
./scripts/debug-relay-pipeline.sh watch
```

## Debugging Workflow

### Problem: Gateway Not Receiving Relays

```bash
# 1. Check gateway status
./scripts/debug-relay-pipeline.sh status | grep -A 5 "Gateway Pods"

# 2. Send test relay
./scripts/test-relay-gateway.sh --verbose

# 3. Check gateway logs
./scripts/debug-relay-pipeline.sh logs-gateway --tail 50

# 4. Direct kubectl check
kubectl --context=platform -n mainnet-gateway logs -l app=relay-router-mainnet --tail=50
```

### Problem: Relayers Not Processing

```bash
# 1. Check relayer status
./scripts/debug-relay-pipeline.sh status | grep -A 5 "Relayer Pods"

# 2. Check relayer logs
./scripts/debug-relay-pipeline.sh logs-relayers --tail 50 | grep -i error

# 3. Check metrics
./scripts/debug-relay-pipeline.sh metrics-relayers

# 4. Direct kubectl check
kubectl --context=nodes -n mainnet logs -l app=relayer --tail=50 --prefix
```

### Problem: Relays Not Reaching Redis

```bash
# 1. Check Redis streams
./scripts/debug-relay-pipeline.sh redis-streams

# 2. Check Redis directly
REDIS_POD=$(kubectl --context=nodes -n mainnet get pods -l app=redis -o jsonpath='{.items[0].metadata.name}')
kubectl --context=nodes -n mainnet exec $REDIS_POD -- redis-cli KEYS "ha:relays:*"

# 3. Check stream length
kubectl --context=nodes -n mainnet exec $REDIS_POD -- redis-cli XLEN "ha:relays:pokt1abc..."
```

### Problem: Sessions Not Settling

```bash
# 1. Check active sessions
./scripts/debug-relay-pipeline.sh redis-sessions

# 2. Check miner logs
./scripts/debug-relay-pipeline.sh logs-miners --tail 100 | grep -i "claim\|proof"

# 3. Check metrics
./scripts/debug-relay-pipeline.sh metrics-relayers | grep -E "claims|proofs"
```

## Direct kubectl Commands

### Platform Context (relay-router-mainnet)

```bash
# List pods
kubectl --context=platform -n mainnet-gateway get pods -l app=relay-router-mainnet

# Logs
kubectl --context=platform -n mainnet-gateway logs -l app=relay-router-mainnet --tail=50

# Follow logs
kubectl --context=platform -n mainnet-gateway logs -l app=relay-router-mainnet -f

# Exec into pod
POD=$(kubectl --context=platform -n mainnet-gateway get pods -l app=relay-router-mainnet -o jsonpath='{.items[0].metadata.name}')
kubectl --context=platform -n mainnet-gateway exec -it $POD -- sh

# Port-forward
kubectl --context=platform -n mainnet-gateway port-forward svc/relay-router-mainnet 8080:8080
```

### Nodes Context (relayers + miners)

```bash
# List relayer pods
kubectl --context=nodes -n mainnet get pods -l app=relayer

# List miner pods
kubectl --context=nodes -n mainnet get pods -l app=miner

# Relayer logs (all replicas)
kubectl --context=nodes -n mainnet logs -l app=relayer --tail=50 --prefix --max-log-requests=10

# Miner logs (all replicas)
kubectl --context=nodes -n mainnet logs -l app=miner --tail=50 --prefix --max-log-requests=10

# Exec into specific relayer
kubectl --context=nodes -n mainnet exec -it relayer-0 -- sh

# Check relayer metrics
kubectl --context=nodes -n mainnet exec relayer-0 -- curl -s http://localhost:9090/metrics | grep ha_relayer

# Check Redis
REDIS_POD=$(kubectl --context=nodes -n mainnet get pods -l app=redis -o jsonpath='{.items[0].metadata.name}')
kubectl --context=nodes -n mainnet exec -it $REDIS_POD -- redis-cli
```

## Environment Variables

Set these to customize the scripts:

```bash
# Contexts
export PLATFORM_CTX=platform
export NODES_CTX=nodes

# Namespaces
export GATEWAY_NS=mainnet-gateway
export NODES_NS=mainnet

# Gateway
export GATEWAY_URL=http://mainnet-gateway.pnyxai.com
export SERVICE_ID=text-generation
export AUTH_TOKEN=48fa9602-27af-4b43-9cff-82fd15f97701

# Now run commands
./scripts/test-relay-gateway.sh
./scripts/debug-relay-pipeline.sh status
```

## Common Issues & Solutions

### Issue: 404 Not Found on Relay Submission

**Symptoms:**
```
HTTP/1.1 404 Not Found
```

**Diagnosis:**
```bash
# Check if gateway is running
./scripts/debug-relay-pipeline.sh status | grep Gateway

# Check gateway logs for errors
./scripts/debug-relay-pipeline.sh logs-gateway --tail 20
```

**Solution:**
- Verify `GATEWAY_URL` is correct
- Check if service route is configured
- Verify namespace and service name

### Issue: Connection Refused

**Symptoms:**
```
curl: (7) Failed to connect to mainnet-gateway.pnyxai.com port 80: Connection refused
```

**Diagnosis:**
```bash
# Check if gateway service exists
kubectl --context=platform -n mainnet-gateway get svc

# Check if pods are running
kubectl --context=platform -n mainnet-gateway get pods
```

**Solution:**
- Ensure gateway pods are running
- Check service configuration
- Verify DNS resolution

### Issue: Relays Not Appearing in Redis

**Symptoms:**
- Gateway receives relays but Redis streams are empty

**Diagnosis:**
```bash
# Check relayer logs for errors
./scripts/debug-relay-pipeline.sh logs-relayers | grep -i error

# Check Redis connectivity
./scripts/debug-relay-pipeline.sh redis-streams
```

**Solution:**
- Check relayer → Redis connectivity
- Verify supplier address configuration
- Check Redis authentication

### Issue: High Latency

**Symptoms:**
- Slow relay responses

**Diagnosis:**
```bash
# Check relayer metrics
./scripts/debug-relay-pipeline.sh metrics-relayers | grep duration

# Watch live metrics
watch -n 1 './scripts/debug-relay-pipeline.sh metrics-relayers | grep duration'
```

**Solution:**
- Check backend service health
- Verify connection pooling settings
- Check network latency between contexts

## Useful kubectl Aliases

Add these to your `~/.bashrc` or `~/.zshrc`:

```bash
# Platform context
alias kp='kubectl --context=platform -n mainnet-gateway'
alias kp-logs='kubectl --context=platform -n mainnet-gateway logs'
alias kp-get='kubectl --context=platform -n mainnet-gateway get'
alias kp-exec='kubectl --context=platform -n mainnet-gateway exec'

# Nodes context
alias kn='kubectl --context=nodes -n mainnet'
alias kn-logs='kubectl --context=nodes -n mainnet logs'
alias kn-get='kubectl --context=nodes -n mainnet get'
alias kn-exec='kubectl --context=nodes -n mainnet exec'

# Quick commands
alias gateway-logs='kp-logs -l app=relay-router-mainnet --tail=50'
alias relayer-logs='kn-logs -l app=relayer --tail=50 --prefix'
alias miner-logs='kn-logs -l app=miner --tail=50 --prefix'
```

Then use:

```bash
# View gateway logs
gateway-logs

# View relayer logs
relayer-logs -f

# Get all pods
kp-get pods
kn-get pods
```

## Quick Debugging Session

**Complete flow to diagnose relay issues:**

```bash
# 1. Check everything is running
./scripts/debug-relay-pipeline.sh status

# 2. Send a test relay
./scripts/test-relay-gateway.sh --verbose

# 3. Watch what happens
./scripts/debug-relay-pipeline.sh watch
# (In another terminal, run step 2 again)

# 4. If relay fails, trace the pipeline
./scripts/debug-relay-pipeline.sh trace

# 5. Deep dive into specific component
./scripts/debug-relay-pipeline.sh logs-gateway --tail 100
./scripts/debug-relay-pipeline.sh logs-relayers --tail 100
./scripts/debug-relay-pipeline.sh redis-streams
```

## Pro Tips

1. **Always use `--context` flag** to avoid changing global kubectl context
2. **Use `--tail` to limit log output** (default: 50 lines)
3. **Use `--prefix` for multi-pod logs** to identify which pod logged what
4. **Use `watch` command** for real-time monitoring
5. **Check metrics first** before diving into logs
6. **Trace Redis streams** to see if relays are being stored
7. **Compare timestamps** across gateway → relayer → Redis to identify bottlenecks

## Additional Resources

- Full documentation: `scripts/README.md`
- Script help: `./scripts/test-relay-gateway.sh --help`
- Debug help: `./scripts/debug-relay-pipeline.sh --help`
- Redis debug: `./bin/pocket-relay-miner redis-debug --help`
