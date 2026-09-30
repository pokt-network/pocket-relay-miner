# Deploying Pocket RelayMiner v0.1.2

Start here, pick 1 path, and follow its runbook from the first step.

## Words you will meet

- **POKT**: Pocket Network's token. Stakes, fees and rewards are paid in it;
  amounts on chain are in uPOKT (1 POKT = 1,000,000 uPOKT).
- **Beta and mainnet**: beta (chain id `pocket-lego-testnet`) is the test
  network, mainnet (`pocket`) the real one. Start on beta.
- **Supplier**: your account on Pocket Network. It is staked for one or more
  services, it is paid for the relays it serves, and its private key signs every
  response, claim and proof ([more](../protocol/SUPPLIER.md)).
- **Stake**: POKT locked on chain. A supplier must be staked for a service to be
  sent relays for it; the minimum is set by the network. You stake with
  `pocketd`, the Pocket Network CLI, not with this repository
  ([how](../SUPPLIER_KEYS.md#creating-a-supplier-key-and-staking-it)).
- **Application**: the on-chain account that pays for relays; its stake is the
  budget each relay is charged against ([more](../protocol/APPLICATION.md)).
- **Gateway**: the client that sends relays to your relayer on behalf of
  applications ([more](../protocol/GATEWAY.md)).
- **Relay**: 1 request from a gateway and the signed response to it.
- **Service**: an on-chain service id, such as one blockchain's RPC, that a
  supplier is staked for ([more](../protocol/SERVICE.md)). A compose file also
  calls each container a service; where the two could be confused, the
  runbooks say "on-chain service id".
- **Backend**: the node that answers a relay for a service, such as your own RPC
  node for that blockchain. The relayer forwards each relay to it.
- **Session**: a fixed number of blocks in which a set of suppliers serves 1
  application on 1 service. Work is claimed and paid per session
  ([more](../protocol/SESSION.md)).
- **Claim and proof**: the 2 transactions the miner submits after each session.
  The claim states the work, the proof backs it when the chain asks for one; a
  claim whose required proof misses its window is not paid. Each costs a fee,
  paid from the supplier's account ([more](../CLAIM_PROOF_LIFECYCLE.md)).
- **Full node**: a server that follows the Pocket chain and answers queries
  (CometBFT RPC and gRPC). The compose example uses the public
  `sauron-*.infra.pocket.network` endpoints; in production use your own or a
  provider's.
- **Redis**: the database the relayer and the miner share. It holds every relay
  until it is claimed and proved, which is why it must never evict a key.
- **Keyring**: an encrypted store of keys created by `pocketd`; one of the 2 ways
  to give the relay miner its keys ([more](../SUPPLIER_KEYS.md)).

## Choose a path

| Path | Use it when | Runbook | Verified |
|---|---|---|---|
| Docker Compose | you want the fastest start, on beta through public endpoints | [DOCKER_COMPOSE.md](DOCKER_COMPOSE.md) | on beta with an unstaked key: configs validated, Redis, node connection, blocks reaching both processes, relayer ready. Not verified: relays, claims and proofs on beta or mainnet |
| Host (binary + systemd) | you run services on VMs or bare metal without containers | [HOST.md](HOST.md) | configs and units checked; not verified end to end under systemd |
| Kubernetes | you already run Kubernetes | no example in v0.1.0 | not verified |

v0.1.0 ships no Kubernetes example or runbook. The Tilt setup in `tilt/` runs
the relayer, the miner and Redis on a local kind cluster for development: it is
a starting point for your own manifests, not a production config. The
[invariants](../../AGENTS.md#invariants) hold on any platform.

## How the pieces fit

```
  gateways ──> relayer :8080 ──> your backends
                  │  │
                  │  └──── reads the chain (applications, sessions, params)
                  ▼
                Redis  (relays, claim trees, stake meter, all shared state)
                  ▲
                  │
                miner ────> the chain (claims, proofs)
```

- **Relayer**: verifies each relay (ring signature, session, the application's
  remaining stake), forwards it to the backend of its service, signs the
  response with the supplier's key and queues the relay in Redis.
- **Miner**: reads the queued relays, builds 1 claim tree per session and
  supplier, and submits the claim and then the proof in their on-chain windows.
- **Redis**: holds all shared state. It is not a cache: a lost key is a claim
  that cannot be proved.

**Topology: 1 Redis shared by the relayers and miners.** v0.1.0 was tested on
1 relayer + 1 miner and on 2 relayers + 2 miners, the latter at lower load. The
load tests and every capacity figure are from 1 relayer + 1 miner.

## Invariants

Each row says what breaking it does: some stop a binary at startup, some make
it serve nothing, and some are only unsupported. Each links to the error it
produces, where there is one.

| # | Invariant | If broken |
|---|---|---|
| 1 | Relayer and miner run the same version: `ghcr.io/pokt-network/pocket-relay-miner:v0.1.2`, or binaries built from tag `v0.1.2` | mixed versions are not supported |
| 2 | Redis 8.10 or newer with `maxmemory` set and `maxmemory-policy noeviction` ([config.redis.example.conf](../../config.redis.example.conf)). Checked when the process starts, not by `validate` | Redis older than 8.10, `maxmemory` 0 or another policy: [both refuse to start](TROUBLESHOOTING.md#redis) |
| 3 | `GOMEMLIMIT` and `GOMAXPROCS` set, or container / systemd memory and CPU limits | [each process sizes itself from the whole host](TROUBLESHOOTING.md#memory-and-cpu) |
| 4 | Miner config has `block_time_seconds` and the right `pocket_node.chain_id`, and the node is reachable | [the miner exits](TROUBLESHOOTING.md#miner-does-not-start) |
| 5 | The miner runs before relays are expected: the relayer's `/ready` is 503 until the miner publishes its service factor manifest | [relayer up, every relay refused](TROUBLESHOOTING.md#relayer-up-but-not-ready) |
| 6 | `relayer validate` and `miner validate` exit 0 on the exact config files you start | [config rejected](TROUBLESHOOTING.md#config-rejected) |

## Prerequisites

| You need | For | Where it comes from |
|---|---|---|
| A Pocket full node: CometBFT RPC and gRPC | both processes; the miner submits transactions through it | yours, or a provider's. The compose example uses the public Sauron endpoints |
| At least 1 staked supplier and its private key (64 hex characters) | signing responses, claims and proofs | your staking process ([how, with `pocketd`](../SUPPLIER_KEYS.md#creating-a-supplier-key-and-staking-it)). **A human provides it; an agent never generates or moves funds** |
| A backend node for every service your suppliers are staked for, **with an active health check** ([why](#backend-health-checks-turn-them-on)) | answering relays | yours |
| Redis 8.10 or newer (both processes refuse an older one at startup), `maxmemory` set, `noeviction` | shared state | [config.redis.example.conf](../../config.redis.example.conf) |
| The supplier's account funded for transaction fees | claims and proofs cost fees | your wallet |

What a relay pays, and every step between a served relay and the reward:
[docs/protocol/INTERACTIONS.md, "The money chain"](../protocol/INTERACTIONS.md#the-money-chain-end-to-end).

The miner's `block_time_seconds` must match the network: beta
(`pocket-lego-testnet`) is roughly 30 seconds, mainnet (`pocket`) roughly 60
seconds; measure yours.

## Services: what you serve, and how to find yours

A **service** is registered on chain under an id (`eth`, `base`, ...), per
network: beta and mainnet have different lists. You stake a supplier for
service ids, and the relayer config lists the same ids under `services:`, each
with 1 backend per transport. Never invent an id.

Most services carry a **card**: what the service is, which transports
(`rpc_types`) it expects and what backend answers them, and a health request
that proves a node is the right one. List every service of a network with its
card (`sauron-api.infra.pocket.network` for mainnet,
`sauron-api.beta.infra.pocket.network` for beta):

```bash
curl -s "https://sauron-api.infra.pocket.network/pokt-network/poktroll/service/service?pagination.limit=1000" | python3 -c '
import base64, json, sys
for s in json.load(sys.stdin)["service"]:
    card = json.loads(base64.b64decode(s["metadata"]["card"])) if (s.get("metadata") or {}).get("card") else {}
    types = ", ".join(t["type"] + " (" + t.get("backend_hint", "") + ")" for t in card.get("rpc_types", []))
    print(s["id"], "|", s.get("name", ""), "|", card.get("description", "no card"), "|", types)
'
```

A line reads `id | name | description | transports`, for example (mainnet,
2026-09-26):

```
eth | Ethereum | Ethereum mainnet execution layer JSON-RPC (chain id 1). ... | JSON_RPC (execution client HTTP RPC, default :8545), WEBSOCKET (execution client WS RPC, default :8546)
```

**To find the service your node serves, ask the node, not the name.** For an
EVM node, `eth_chainId` gives its chain id; the service is the one whose card
says that chain id (many descriptions mention Ethereum; only 1 says chain id 1):

```bash
curl -s -X POST -H 'Content-Type: application/json' -d '{"jsonrpc":"2.0","method":"eth_chainId","params":[],"id":1}' http://<node>:8545
```

`{"result":"0x1"}` is chain id 1 (`eth`), `0x2105` is 8453 (`base`). If no
service on the network matches, that network has no service for your node.

**Transports.** The card's `rpc_types` name what the service expects, in the
stake file's words; the relayer config names the same transports its own way:

| `rpc_type` (stake file, card) | backend key in `relayer.yaml` |
|---|---|
| `JSON_RPC` | `jsonrpc` |
| `WEBSOCKET` | `websocket` |
| `REST` | `rest` |
| `GRPC` | `grpc` |
| `COMET_BFT` | `cometbft` |

Serve each transport the card expects and your node answers: 1 backend per
transport in `relayer.yaml`, and 1 endpoint per transport in the stake file.
The card's health request is a good `health_check` probe for an HTTP backend
(see below).

## Backend health checks: turn them on

**Give every HTTP backend (`jsonrpc`, `rest`, `cometbft`) an active health
check.** Without one, the relayer learns that a backend is down only from the
relays it forwards: it marks the backend unhealthy after 5 consecutive failed
relays, and every 30 seconds it sends real relays to it again to see whether it
came back. Each of those failures is a relay a gateway sent you that got an
error instead of an answer.

With an active health check, the relayer probes the backend on its own, every
`interval_seconds`: after `unhealthy_threshold` failed probes it stops sending
relays there, and after `healthy_threshold` good ones it sends them again. With
2 or more `urls` in a backend, relays go to the healthy ones. Probe something
that proves the node can answer, not only that its port is open: for an EVM
node, a JSON-RPC `eth_blockNumber` with `expected_body: '"result"'`.

```yaml
services:
  eth:
    backends:
      jsonrpc:
        url: "http://10.0.0.5:8545"
        health_check:
          enabled: true
          endpoint: "/"
          method: "POST"
          request_body: '{"jsonrpc":"2.0","method":"eth_blockNumber","params":[],"id":1}'
          expected_body: '"result"'
          interval_seconds: 10
          timeout_seconds: 5
          unhealthy_threshold: 3
          healthy_threshold: 2
```

Every key, with its default: [config.relayer.example.yaml](../../config.relayer.example.yaml)
(the `health_check` block under a backend). `/ready/<service-id>` on the
relayer's health address lists each backend's state and answers 503 when none
is healthy: `$C exec relayer curl -s http://localhost:8081/ready/<service-id>`
in the compose example, `curl -s http://127.0.0.1:8081/ready/<service-id>` on a
host.

**HTTP backends only in v0.1.0**: the probe is a plain HTTP/1.1 request. On a
`websocket` backend (`ws://`, `wss://`) or a `grpc` backend it fails every
probe and marks a working backend unhealthy (measured 2026-09-26 against a
WebSocket server and a gRPC server with the standard health service). Leave
`health_check` off on `websocket` and `grpc` backends.

## Ports

| Process | Port | Serves |
|---|---|---|
| relayer | 8080 | relay traffic (`listen_addr`) |
| relayer | 8081 | `GET /health` (always 200 while running), `GET /ready` (200 only when it can serve) |
| relayer | 9090 | Prometheus metrics (`metrics.addr`) |
| relayer | 6060 | pprof profiling (`pprof.addr`). The default is `127.0.0.1:6060`, loopback only; in a container that means `docker exec` or an explicit `pprof.addr: "0.0.0.0:6060"` to reach it. Never expose it publicly |
| miner | 9092 | Prometheus metrics and `GET /health` (`metrics.addr`) |
| miner | 6060 | pprof profiling (`pprof.addr`), off by default. When enabled with no `addr` it also listens on `127.0.0.1:6060`, the relayer's default: on 1 host, give one of them another port |
| Redis | 6379 | never expose it outside the host or the compose network |

Relayer metrics on 9090 and a node's gRPC on 9090 collide when both run on the
same host network. The host runbook binds metrics and pprof to `127.0.0.1`; move one of
them if your node is on the same host. Both binaries default pprof to
`127.0.0.1:6060`, so a relayer and a miner with pprof enabled on 1 host collide:
the second one logs `pprof server failed` and keeps running without pprof.

## Startup order

1. Redis, and check `maxmemory-policy` is `noeviction`.
2. Validate both configs: exit 0.
3. Miner. It exits within about 10 seconds if it cannot read the chain.
4. Relayer. Its `/ready` turns 200 once the miner's manifest is in Redis.
5. Send a relay, then watch the claim land after the session ends.

## Upgrading and rolling back

- Read the release notes of the version you are moving to first: what changed,
  what it breaks (config keys, metrics, dashboards) and what to do before
  upgrading. They are on each release, at
  <https://github.com/pokt-network/pocket-relay-miner/releases>.
- Upgrade the relayer and the miner together.
- Validate the new configs with the new binary before switching: retired keys
  fail `validate`.
- v0.1.0 is the first release, so there is no earlier release to roll back to.
  Builds from before it cannot read the compacted trees: do not switch to one
  while claimed sessions still await their proof.
