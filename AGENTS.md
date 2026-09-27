# AGENTS.md

Read this first. It routes you to the right document and lists what stops a
deployment from starting.

## What this is

Pocket RelayMiner serves relays for Pocket Network suppliers and gets them paid.
It is 2 processes from 1 binary, sharing 1 Redis: the **relayer** verifies each
relay, forwards it to your backend and signs the response; the **miner** builds
the claim and the proof from the served relays and submits them to the chain.

## Where to go

- **Deploying**: [docs/deploy/README.md](docs/deploy/README.md) to choose a path,
  then its runbook: [DOCKER_COMPOSE.md](docs/deploy/DOCKER_COMPOSE.md) or
  [HOST.md](docs/deploy/HOST.md). When it does not start or does not serve:
  [TROUBLESHOOTING.md](docs/deploy/TROUBLESHOOTING.md). Words such as
  supplier, stake, session or backend are explained at the top of
  [docs/deploy/README.md](docs/deploy/README.md#words-you-will-meet).
- **The human has nothing yet** (no key, no POKT, no stake): the usual case,
  and not a blocker. In this order:
  1. Start now: the Docker Compose runbook on beta with the public example
     key, steps 0 to 8, with the dashboards if they want graphs.
  2. Configure the backends they gave you (step 10), under the on-chain id of
     the service each one serves, and check each answers. If that service is
     not registered on the network you are testing on (beta has its own
     list), leave the example's service as it is for this test and configure
     theirs when they stake on the network that has it. Never put a backend
     under another service's id, and never invent a URL or a port.
  3. For the key, the POKT and the stake, copy into your reply the commands
     of [docs/SUPPLIER_KEYS.md, "Creating a supplier key, and staking it"](docs/SUPPLIER_KEYS.md#creating-a-supplier-key-and-staking-it)
     (installing `pocketd`, the beta faucet, the export, the stake file), as
     they are written there: a link alone is not enough for a human who will
     not read it, and commands, URLs or flags from memory are wrong.
     Two facts they will ask about: beta POKT is free from the faucet
     (mainnet POKT is bought), and the URL in the stake is the relayer's
     public `https://` address behind a TLS proxy, never an IP and port of
     this machine or a private address.
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
the relayer, the miner and Redis on a local kind cluster for development: it is
a starting point for your own manifests, not a production config. The
invariants below hold on any platform.

## Invariants

Some of these are checked at startup and stop a binary when broken; the rest
are not checked, and breaking them is unsupported.

1. **Topology: 1 Redis shared by the relayers and miners.** v0.1.0 was tested
   on 1 relayer + 1 miner and on 2 relayers + 2 miners, the latter at lower
   load; the load tests and every capacity figure are from 1 relayer + 1 miner.
2. **Same version for relayer and miner**: image
   `ghcr.io/pokt-network/pocket-relay-miner:v0.1.0` for both, or the binary of
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
   and `pocket-relay-miner miner validate --config <file>` must exit 0.
   `validate` does not contact Redis or the chain, so exit 0 is necessary, not
   sufficient.

## Rules for an agent running a deployment

**Hard rules**, broken in real first-time runs:

1. Run the runbook's commands exactly as written, with its `$C` (project
   `prm-example`). Do not swap in another tool: `netstat` is missing on many
   hosts and prints nothing, which reads as "all ports free".
2. Never install software or pipe a downloaded script into a shell on the
   human's machine. `pocketd` is installed by the human, with the commands in
   [docs/SUPPLIER_KEYS.md](docs/SUPPLIER_KEYS.md#creating-a-supplier-key-and-staking-it).
3. Never ask for, accept or handle a private key, a mnemonic or a passphrase.
4. Never invent a service id, a URL, a port, a flag or an amount; take each
   from the docs or the chain, or say you do not know.
5. Stake and configure only services the human's backends serve. A service
   with another name is another service, even if it sounds related.
6. Stop and ask before anything on mainnet, and before spending or staking.

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
- **Follow the runbook with the example's own files.** Edit the files in
  `examples/docker-compose/` (or install `examples/host/`) as the steps say;
  do not write a compose file or configs of your own, or the runbook's checks
  no longer apply to what you run.
- **Service ids come from the chain; never invent one.** Map what the human
  wants to serve to an id the network has registered:
  `curl -s 'https://sauron-api.beta.infra.pocket.network/pokt-network/poktroll/service/service?pagination.limit=1000'`
  (`sauron-api.infra.pocket.network` for mainnet) lists them. If none matches,
  say so; do not guess one.
- **Check that the ports are free before starting, and that every check
  reaches your own stack.** Another program on 8180, 9091 or 3000 answers a
  health or dashboard check in place of yours: the runbook's step 0 checks the
  ports and says how to move them; after `up`, `docker compose ps` must show
  each port published by your containers.
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
