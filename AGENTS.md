# AGENTS.md

Read this first. It routes you to the right document and lists what stops a
deployment from starting.

## Hard rules (read these first)

Each one was broken by an agent in a real first-time run.

1. Run the runbook's commands exactly as written, with its `$C` (project
   `prm-example`, or `prm-standalone` in the standalone-mode runbook). Do not
   swap in another tool: `netstat` is missing on many hosts and prints
   nothing, which reads as "all ports free".
2. Never install software or pipe a downloaded script into a shell on the
   human's machine. `pocketd` is installed by the human, with the commands in
   [docs/SUPPLIER_KEYS.md](docs/SUPPLIER_KEYS.md#creating-a-supplier-key-and-staking-it).
3. Never ask for, accept or handle a private key, a mnemonic or a passphrase,
   and never ask the human to send one to you. Say instead: "put your key in
   `examples/docker-compose/config/supplier-keys.local.yaml` yourself (runbook
   step 9) and tell me when it is there"; then check it with `validate`.
4. Never invent a service id, a URL, a port, a flag or an amount; take each
   from the docs or the chain, or say you do not know.
5. Stake and configure only services the human's backends serve. A service
   with another name is another service, even if it sounds related.
6. Stop and ask before anything on mainnet, and before spending or staking.
7. The stack needs ports 8180, 9091 and 3000 free. Check them (runbook step
   0). If one is taken, stop and tell the human which.
8. You have a shell on this machine: run the runbook's commands yourself.
   Ask the human only for decisions and for what only they can do (their
   key, their funds, their stake).
9. After you edit any config, run the runbook's `validate` commands (step 3:
   2 in high-availability mode, 1 in standalone mode) and get exit 0 before
   you restart anything. It catches a key in the wrong place and a key that
   does not exist.
10. To switch keys or networks, follow the runbook's section for it (step 9,
    "Switching to mainnet") line by line, not from memory: the keys file
    also has to be mounted in the compose file (`docker-compose.yaml`, or
    `docker-compose.standalone.yaml` in standalone mode), only once the human
    has written it (a mount of a missing file breaks the next start), and the
    mainnet values are listed there.

## What this is

Pocket RelayMiner serves relays for Pocket Network suppliers and gets them paid.
The **relayer** verifies each relay, forwards it to your backend and signs the
response; the **miner** builds the claim and the proof from the served relays
and submits them to the chain. It runs in 1 of 2 modes:

- **High-availability mode**: 2 processes from 1 binary (`relayer`, `miner`),
  sharing 1 Redis; relayers and miners can have replicas. Released (v0.1.2),
  and its Docker Compose runbook was verified on beta.
- **Standalone mode**: 1 process (`standalone`) that runs both, its state in an
  embedded store on local disk, no Redis, no replicas. Built from source until
  a release ships it; not yet run on beta.

[docs/deploy/README.md, "Choose a mode"](docs/deploy/README.md#choose-a-mode)
compares them. **Choosing the mode is the human's decision**: when they have
not said, start with high-availability mode (the verified one) and tell them
standalone mode exists, in 1 line.

## Where to go

- **Deploying**: [docs/deploy/README.md](docs/deploy/README.md) to choose a mode
  and a path, then its runbook. High-availability mode:
  [DOCKER_COMPOSE.md](docs/deploy/DOCKER_COMPOSE.md) or
  [HOST.md](docs/deploy/HOST.md). Standalone mode:
  [DOCKER_COMPOSE_STANDALONE.md](docs/deploy/DOCKER_COMPOSE_STANDALONE.md) or
  [HOST_STANDALONE.md](docs/deploy/HOST_STANDALONE.md); the steps have the
  same numbers. When it does not start or does not serve:
  [TROUBLESHOOTING.md](docs/deploy/TROUBLESHOOTING.md). Words such as
  supplier, stake, session or backend are explained at the top of
  [docs/deploy/README.md](docs/deploy/README.md#words-you-will-meet).
- **The human has nothing yet** (no key, no POKT, no stake): the usual case,
  and not a blocker. In this order:
  1. Start now: the Docker Compose runbook of the mode they chose (high
     availability when they have not chosen) on beta with the public example
     key, steps 0 to 8, with the dashboards if they want graphs. It starts
     as shipped, with the example's `my-service`: nothing about their
     services, keys or service ids is needed to start.
  2. Find the service each of their nodes serves by asking the node, not by
     its name: [docs/deploy/README.md, "Services"](docs/deploy/README.md#services-what-you-serve-and-how-to-find-yours)
     lists each service's card (its transports and a health request) and
     matches an EVM node by its chain id. Tell the human in 1 line what you
     found ("your Ethereum node is service `eth` on mainnet, JSON_RPC and
     WEBSOCKET; beta has no such service") and ask only if the node's answer
     leaves more than 1 candidate. Then configure the backends they gave you
     (step 10) under that id, 1 per transport, and check each answers. If that service is
     not registered on the network you are testing on (beta has its own
     list), leave the example's service as it is for this test and configure
     theirs when they stake on the network that has it. Never put a backend
     under another service's id, and never invent a URL or a port.
  3. Key, POKT and stake: paste these into your reply exactly; the human runs
     them. See more (the stake file): [docs/SUPPLIER_KEYS.md](docs/SUPPLIER_KEYS.md#creating-a-supplier-key-and-staking-it).

     ```bash
     curl -sLO https://github.com/pokt-network/poktroll/releases/download/v0.1.35/pocket_linux_amd64.tar.gz
     tar -xzf pocket_linux_amd64.tar.gz      # extracts a single file: pocketd
     sudo install pocketd /usr/local/bin/
     pocketd keys add supplier1 --keyring-backend file --keyring-dir ~/.pocket
     pocketd keys export supplier1 --unarmored-hex --unsafe --keyring-backend file --keyring-dir ~/.pocket
     pocketd tx supplier stake-supplier --config stake_config.yaml --from supplier1 \
       --keyring-backend file --keyring-dir ~/.pocket --network main
     pocketd query supplier show-supplier <pokt1-address> --network main
     ```

     Beta: `--network beta`, POKT from <https://faucet.beta.pocket.network>.
     The human puts the exported key in `config/supplier-keys.local.yaml`
     (runbook step 9), never in your chat.
     Facts they will ask about: beta POKT is free from the faucet (100,000
     POKT per request, at most 2 per account; mainnet POKT is bought); the minimum stake is large, read it from the chain
     (59,500 POKT on 2026-09-26, per supplier, not per service; divide upokt
     by 1,000,000); the URL in the
     stake is the relayer's public `https://` address, on a DNS name, behind
     a TLS proxy on 443 (`https://relayer.example.com`, no `:8180`), and must
     not change (never this machine's IP and port, a
     private address, or a tunnel URL that changes on restart).
     Before the stake file, check that each service they want exists on
     that network (the query below): beta registers its own services, and
     one missing there cannot be staked on beta.
  4. When they are staked, step 9 switches the key and step 11 checks it.
- **Dashboards** (optional): Prometheus and Grafana with 7 dashboards, started
  with the compose example's `observability` profile:
  [examples/observability/](examples/observability/README.md).
- **Anything else** -- configuring, operating, testing, understanding the
  protocol, changing the code: the index in
  [README.md, "Where to start"](README.md#where-to-start) says, for what you
  want to do, which document or tool to open.

v0.1.0 ships no Kubernetes example or runbook. The Tilt setup in `tilt/` runs
the relayer, the miner and Redis (high-availability mode) on a local kind
cluster for development: it is
a starting point for your own manifests, not a production config. The
invariants below hold on any platform.

## Invariants

Some of these are checked at startup and stop a binary when broken; the rest
are not checked, and breaking them is unsupported. Items 1 to 3 and 6 are
high-availability mode's; standalone mode's are after the list.

1. **Topology: 1 Redis shared by the relayers and miners.** v0.1.0 was tested
   on 1 relayer + 1 miner and on 2 relayers + 2 miners, the latter at lower
   load; the load tests and every capacity figure are from 1 relayer + 1 miner.
2. **Same version for relayer and miner**: image
   `ghcr.io/pokt-network/pocket-relay-miner:v0.1.2` for both, or the binary of
   the same tag. Mixed versions are not supported. Never use a moving tag.
3. **Redis 8.10 or newer, with `maxmemory` set and `maxmemory-policy
   noeviction`.** Both binaries refuse to start when `redis_version` is below
   8.10, when `maxmemory` is 0, or when the policy is anything other than
   `noeviction`: Redis holds every relay until it is claimed and proved, and an
   evicted key is a claim that cannot be proved. `validate` does not connect
   to Redis: these are checked when the process starts. Use
   [config.redis.example.conf](config.redis.example.conf).
4. **Memory and CPU limits**: set `GOMEMLIMIT` and `GOMAXPROCS`, or give each
   process a container (or systemd) memory and CPU limit. Without either, each
   process sizes itself from the whole host.
5. **The miner needs the chain at startup.** Set `block_time_seconds` and
   `pocket_node.chain_id` in the miner config; the miner exits if the node is
   unreachable or reports another network.
6. **The relayer's `/ready` answers 503 until the miner has published its
   service factor manifest.** Start the miner first. A relayer that is up with
   `/ready` at 503 is waiting for the miner, not broken.
7. **Validate before starting**: `pocket-relay-miner relayer validate --config <file>`
   and `pocket-relay-miner miner validate --config <file>` must exit 0
   (`pocket-relay-miner standalone validate --config <file>` in standalone
   mode). `validate` does not contact Redis or the chain, so exit 0 is
   necessary, not sufficient.

In **standalone mode**: 1 process per store and per set of supplier keys (a
second one on the same `storage.path` refuses to start; on other keys files
with the same keys, both would serve and claim the same suppliers); the store
on local disk with room (new work stops below 1 GiB free); its config has no
`redis` section; items 4, 5 and 7 hold as written, and the relayer side's
`/ready` waits for the miner side inside the process.

## Rules for an agent running a deployment

- **Size the limits for the load; the examples do not.** The limits in
  `examples/docker-compose/` (Redis and miner 4 GiB, relayer 2 GiB, 2 CPUs
  each) fit a few suppliers with ordinary traffic; they do not absorb a burst
  of large relays or dozens of suppliers. The `examples/host/` units have their
  own (miner `MemoryMax=8G`, relayer `MemoryMax=4G`, `CPUQuota=400%` each;
  Redis `maxmemory` is yours to set,
  [HOST.md, step 2](docs/deploy/HOST.md#step-2-redis-810)); neither set is
  the one the load run used, which gave the relayer 8 GiB, the miner 8 GiB and
  Redis 16 GiB with `maxmemory` 12.8 GiB. Before real traffic, scale them from: the
  comments above each limit in
  [examples/docker-compose/docker-compose.yaml](examples/docker-compose/docker-compose.yaml)
  (what the v0.1.0 load run used), the
  [capacity report](docs/benchmarks/v0.1.0/Relay-Miner-Capacity.pdf)
  (memory, CPU and Redis per component under load; how to read it against your
  load is in [docs/benchmarks/README.md](docs/benchmarks/README.md)),
  [config.redis.example.conf](config.redis.example.conf) (Redis settings of
  that run), and [TROUBLESHOOTING.md, Memory and CPU](docs/deploy/TROUBLESHOOTING.md#memory-and-cpu).
  Then tell the human to watch memory as traffic grows: it follows request
  sizes and live sessions, and no example can size it for every mix.
  Standalone mode's examples size the one process as the sum of the two
  (compose 6 GiB and 2 CPUs, host unit `MemoryMax=12G`); that sum is not
  measured for standalone mode, so the same watching applies, plus the free
  disk of its store.
- **Follow the runbook with the example's own files.** Edit the files in
  `examples/docker-compose/` (or install `examples/host/`) as the steps say;
  do not write a compose file or configs of your own, or the runbook's checks
  no longer apply to what you run.
- **Service ids come from the chain; never invent one.** Map what the human
  wants to serve to an id the network has registered:
  `curl -s 'https://sauron-api.beta.infra.pocket.network/pokt-network/poktroll/service/service?pagination.limit=1000'`
  (`sauron-api.infra.pocket.network` for mainnet) lists them. If none matches,
  say so; do not guess one.
- Pick 1 runbook, [docs/deploy/DOCKER_COMPOSE.md](docs/deploy/DOCKER_COMPOSE.md)
  or [docs/deploy/HOST.md](docs/deploy/HOST.md), and run its steps in order
  from Step 0. A step gives **Run** and **Expect**, and **If not** and
  **Stop if** where they apply. Compare the output with **Expect** before going on.
- **Give every HTTP backend an active health check, and check that each
  backend answers before starting**: `validate` never contacts a backend.
  Without a health check the relayer finds a dead backend only by failing the
  relays it forwards; see
  [docs/deploy/README.md, "Backend health checks"](docs/deploy/README.md#backend-health-checks-turn-them-on).
- **Prove the stack before any staking.** Run the runbook's steps with the
  public example key first: the stack up, `/ready` 200, the height growing. Only
  then help the human create, fund and stake a real key.
- **Never ask for, accept or handle a private key, a mnemonic or a
  passphrase.** Not in the chat, not in a file you write. Tell the human which
  file to put the key in and how (DOCKER_COMPOSE.md step 9, HOST.md step 3),
  let them do it, and check the result with `validate` and the miner's
  `staked_suppliers` count.
- **Stop and ask a human** before using real supplier keys, spending funds,
  staking, or pointing anything at mainnet. The key in
  `examples/docker-compose/config/supplier-keys.yaml` is public and unstaked,
  there only to start the stack: never fund or stake it.
