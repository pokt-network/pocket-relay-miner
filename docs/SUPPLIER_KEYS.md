# Supplier signing keys

Every relay a supplier serves is signed with that supplier's key, and every claim
and proof the miner submits is signed with it too. This is the one document for
how you give the relayer and the miner those keys — what the options are, which
one to pick, and why the others were left out.

**The relayer and the miner read keys the same way.** Same sources, same
validation, same reload behaviour, same error messages. Anything below applies to
both unless it says otherwise.

## The two sources

Configure **exactly one**. Naming both is a startup error, and so is naming
neither.

### `keys_file` — a YAML file of hex private keys

```yaml
keys:
  keys_file: /keys/supplier-keys.yaml
```

```yaml
# /keys/supplier-keys.yaml
keys:
  - c188c43496351a963762a5d9de78ff887ac66b4ba5de5967efd55a6d1e71ddda
  - fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210
```

The operator address of each supplier is **derived from the key**, so the file
lists key material only — there is nothing to keep in sync and no way for an
address and its key to disagree.

Mount it read-only into the container (the compose example bind-mounts
`supplier-keys.yaml`), or install it with mode 0600 on a host. This is the
simplest option and the one the local stack runs by default.

### `keyring` — a Cosmos SDK keyring

```yaml
keys:
  keyring:
    backend: file                                    # file | test
    dir: /keyring                                    # PARENT of the keyring
    app_name: pocket                                 # optional
    key_names: []                                    # optional; empty = all keys
    passphrase_file: /secrets/keyring-passphrase     # for backend: file
```

Use this when the keys already live in a keyring — the same one `pocketd` uses —
and you would rather not export them to a file.

#### `dir` is the PARENT of the keyring, not the keyring

cosmos-sdk appends the backend's own subdirectory to whatever you give it, so
`dir: /keyring` with `backend: file` reads `/keyring/keyring-file`. Point `dir` at
the keyring itself and you open `/keyring/keyring-file/keyring-file` — which
exists, is empty, and makes every lookup fail as **"key not found"**, a message
that blames the key name rather than the path. The process warns when your `dir`
looks like this mistake.

`dir` is **required**. An empty value is not a default: it resolves to a relative
`keyring-file` beside the process's working directory.

#### Which backends, and why only these two

| Backend | Supported | Why |
|---|---|---|
| `file` | **yes** | Passphrase-protected, works headless, covered by tests. What production should use. |
| `test` | **yes** | Fixed password, no prompt. For local stacks and CI. |
| `memory` | no | The keyring is created **empty** and nothing ever writes a key into it, so the process would hold no keys for its entire life. |
| `os` | no | Uses a system keychain where dbus and an unlocked desktop session exist — which a service account does not have — and silently falls back to an encrypted file where they do not, in a **different** directory than `file`. Which one a given host does is not something we can establish or test. |
| `kwallet`, `pass` | no | Never wired. |

If you want a passphrase-protected keyring on a bare VM, say `file`. You get the
behaviour you asked for on every host.

## Creating a supplier key, and staking it

The relay miner does not create keys or stake: `pocketd`, the Pocket Network
CLI, does. Run these on a machine you trust, and never paste a private key, a
mnemonic or a passphrase into a chat or an AI agent. Flags below are from
`pocketd` 0.1.35.

**0. Install `pocketd`.** From the [poktroll v0.1.35 release](https://github.com/pokt-network/poktroll/releases/tag/v0.1.35)
(`pocket_linux_arm64.tar.gz` on ARM, the `darwin` archives on macOS; check
them against `release_checksum` on the same page):

```bash
curl -sLO https://github.com/pokt-network/poktroll/releases/download/v0.1.35/pocket_linux_amd64.tar.gz
tar -xzf pocket_linux_amd64.tar.gz      # extracts a single file: pocketd
sudo install pocketd /usr/local/bin/
pocketd version                         # prints 0.1.35
```

**1. Create the key in a passphrase-protected keyring.**

```bash
pocketd keys add supplier1 --keyring-backend file --keyring-dir ~/.pocket
```

It asks for a passphrase twice, then prints the address (`pokt1...`) and a
mnemonic. Write the mnemonic down offline: it is the only way to recover the key.

**2. Give the key to the relay miner, one of two ways.**

- **The keyring itself.** Copy `~/.pocket/keyring-file/` to the server and point
  `keys.keyring.dir` at its PARENT, with `backend: file` and the passphrase in a
  file ([the `keyring` source](#keyring--a-cosmos-sdk-keyring),
  [the passphrase](#the-passphrase-for-backend-file)).
- **A hex key in the keys file.** Export the private key and put its 64 hex
  characters under `keys:` in `supplier-keys.yaml` (mode 0600;
  [the `keys_file` source](#keys_file--a-yaml-file-of-hex-private-keys)):

  ```bash
  pocketd keys export supplier1 --unarmored-hex --unsafe --keyring-backend file --keyring-dir ~/.pocket
  ```

  The supplier address is derived from the key: nothing else to write down.

**3. Fund and stake the supplier.** The account needs POKT for the stake and for
the fee of every claim and proof. On beta, request test POKT for the address
from the faucet, <https://faucet.beta.pocket.network>. On mainnet there is no
faucet: POKT is bought, and sent to the address from an account you control.

Staking is a chain transaction. Its config file (owner and operator addresses,
stake amount, and each service with its endpoint URL) is described in the
Pocket Network docs, [Supplier staking](https://docs.pocket.network/node-operators/supplier-staking/);
the minimum stake is a governance parameter of the network. Each `service_id`
must be a service registered on that network, spelled exactly:
`pocketd query service all-services --network beta` (or `--network main`)
lists them; so does
`https://sauron-api.beta.infra.pocket.network/pokt-network/poktroll/service/service?pagination.limit=1000`
(`sauron-api.infra.pocket.network` for mainnet).

A minimal `stake_config.yaml`, in the format of that page (the addresses are
yours; 1 endpoint per transport you serve for a service, `rpc_type` one of
`JSON_RPC`, `REST`, `WEBSOCKET`):

```yaml
owner_address: pokt1...
operator_address: pokt1...
stake_amount: 59500000000upokt
services:
  - service_id: <an id registered on this network>
    endpoints:
      - publicly_exposed_url: https://relayer.example.com
        rpc_type: JSON_RPC
```

`stake_amount` is in `upokt` (1 POKT = 1,000,000 upokt) and at least the
network's `min_stake`, which governance can change: read it before staking
with `curl -s https://sauron-api.beta.infra.pocket.network/pokt-network/poktroll/supplier/params`
(`sauron-api.infra.pocket.network` for mainnet; it read `59500000000` on both
on 2026-09-26). The account also needs POKT left over for fees. Beta registers
its own services, not mainnet's: if the one you want to serve is not on beta,
the test on beta stops at the running stack, and staking it needs mainnet.

**The endpoint URL is where gateways on the internet reach your relayer**, not
your backend node. It is a public DNS name or IP, normally `https://` through a
TLS proxy in front of the relayer's relay port (8080 in the relayer's config;
the compose example publishes it on the host as 8180, bound to loopback until
you change it). Never `localhost` or a private address (`10.x`, `172.16-31.x`,
`192.168.x`): gateways cannot reach it, and the supplier gets no relays. A
server at home needs a public address, or a port forward on the router to the
TLS proxy.

```bash
pocketd tx supplier stake-supplier --config stake_config.yaml --from supplier1 \
  --keyring-backend file --keyring-dir ~/.pocket --network beta
pocketd query supplier show-supplier <pokt1-address> --network beta
```

Use `--network main` for mainnet. Once staked,
`pocket-relay-miner relayer validate --config <file> --check-stake` confirms every
staked service has a backend in your relayer config.

## The passphrase, for `backend: file`

The passphrase never goes in the config. The config carries a **reference** to
it, exactly as `keys_file` carries a path to a file of private keys. Three ways,
pick one:

```yaml
# 1. A mounted secret. Preferred.
keys:
  keyring:
    backend: file
    dir: /keyring
    passphrase_file: /secrets/keyring-passphrase
```

```yaml
# 2. The NAME of an environment variable -- never its value.
keys:
  keyring:
    backend: file
    dir: /keyring
    passphrase_env: KEYRING_PASSPHRASE
```

```bash
# 3. stdin, for a host with a terminal or a secret manager you can pipe from.
#    Configure neither of the above.
echo "$SECRET" | pocket-relay-miner relayer --config config.yaml
```

A file is preferred over an environment variable because an environment variable
is readable from `/proc/<pid>/environ` by anything in the same namespace, and
tends to reach crash dumps and process listings.

`passphrase_file` and `passphrase_env` are mutually exclusive. Setting either for
a backend that has no passphrase (`test`) is an error rather than a no-op.

**One unlock covers the whole keyring.** The passphrase is read once and cached
for the life of the process, so a keyring holding 50 supplier keys prompts once,
not 50 times. A file saved without a trailing newline works.

**stdin does not work in a container.** A container's stdin is `/dev/null`, so
cosmos-sdk reads EOF, retries three times and the process exits at startup. That
is a clear failure rather than a hang, and the process warns at startup when no
passphrase source is configured — but if you are deploying, use
`passphrase_file`. Do **not** wrap the command in a shell to pipe a file into it:
a pipe leaves a shell as the parent, and that shell does not forward `SIGTERM`,
which the miner needs in order to release its supplier leases on shutdown.

## Hot reload — a key added or pulled takes effect without a restart

On by default, in **both** binaries, and configured in the same place in both:

```yaml
keys:
  hot_reload_enabled: true   # default
```

Turning it off is legitimate — some operators want key changes to happen only on
a deliberate restart — so the setting stays. What the process will not do is let
you turn it off quietly: with `hot_reload_enabled: false` the key manager logs a
**warning** at startup saying a key added or removed now takes effect only on
restart.

`hot_reload_enabled` at the TOP level of a miner config is a startup error, not
an ignored line. It used to live there, and until 2026-08-22 it was read by
nothing: a miner whose config said `true` ran with key hot reload off. The error
names the new location so the migration is one line.

Two mechanisms feed the same reload, so there is one piece of code deciding what
changed:

- **A watch**, on `keys_file` only. Its provider watches the containing
  *directory* for writes and creates, which is what makes a Kubernetes secret's
  `..data` symlink swap register. A change lands almost at once.
- **A timer**, every 30 seconds, over **every** source. A keyring cannot be
  watched at all, so this is the only thing that finds a change there. It also
  covers a watch that died — fsnotify drops its watch when the inode it holds is
  renamed away, and without a timer a process would then stop reloading for the
  rest of its life without ever saying so.

So the promise is: **a key change takes effect within 30 seconds, from any
source**, and much sooner for `keys_file`. The process logs which of your sources
are watched and which rely on the timer when it starts.

A reload that finds nothing changed is silent — no log, no metric, no work. Only
real changes are reported.

### Removing a key is per key STORE, not per fleet

A reload sees the store the process opened, so where that store lives decides
how many places a removal has to happen:

- **`keys_file` on a shared Secret or file** — one write reaches every pod. The
  directory watch fires as soon as the volume is synced.
- **A keyring built per instance** — each process has its own copy, so a key
  deleted in one pod is still loaded in the others. Delete it in every instance,
  or the fleet is left in a split state where some replicas serve a supplier and
  some refuse it.

Measured on 2026-08-22 with a per-pod keyring: deleting `<name>.info` from all
four pods dropped the key from every process within one reload interval, relays
for that supplier were refused at the gate with **zero** backend calls, other
suppliers were unaffected, and nothing restarted. Deleting only the `.info` is
enough — cosmos-sdk lists a keyring by its `.info` files and ignores the
leftover `.address` entry.

### What a reload can and cannot do

A reload adds keys and removes keys. There is no third case: because the operator
address is derived from the key material, the same address cannot come back with
different key material. A rotation is one address leaving and another arriving.

**A source it cannot read is not a removal.** If the key file is mid-rewrite, the
secret is mid-swap, or the keyring is briefly locked, the reload is abandoned and
the previous keys are kept — with an error naming the source. Treating an
unreadable source as "the operator removed these keys" would stop the relayer
serving those suppliers and make the miner drain their pipelines, on a transient
read error, every 30 seconds.

**An emptied key FILE is refused, and an emptied KEYRING is not.** The two
sources answer this differently, on purpose. A `supplier-keys.yaml` with no keys is
far more often a truncated write or a bad template than a request to stop
serving, so it is refused and the previous keys are kept; to stop serving,
unstake or stop the process.

A keyring that yields no signing keys IS applied — every supplier is released.
Refusing there was tried and measured on 2026-08-31: the condition is stable, so
it repeated on every reload, the previous key set was kept for the life of the
process, and a key withdrawn afterwards went on signing. Removing a record's
`.info` is the documented withdrawal, so this is also what makes that withdrawal
work. The release logs at `Error` and moves `ha_keys_load_errors_total` — once.
The load after it finds the directory unchanged and returns from cache, so the
counter goes flat while every supplier stays released: **alert on
`ha_keys_supplier_keys_active` falling to zero, not on a rate over the error
counter**, which fires and self-resolves. Measured 2026-08-31.

A keyring whose records are present but **undecodable** is handled by one rule,
and the rule is whether the keyring REPORTED the problem or hid it:

- **The keyring hid it.** Without `key_names`, cosmos-sdk lists what it can
  decode and silently skips the rest — no error reaches us. There is nothing to
  report and nothing to wait for, so the broken record is applied: that supplier
  loses service, because nothing can sign with a key it cannot decode.
  `ha_keys_undecodable_records` counts how many are broken right now. That series
  is only published in this mode; with `key_names` set nothing counts the
  records, so it is **absent rather than zero**.
- **The keyring reported it.** With `key_names`, a broken record among the
  selected names comes back as an error. The reload is then **abandoned and the
  previous keys are kept — deliberately, and for as long as the record stays
  broken.** Nothing is guessed and nothing is released on a source that told us
  it could not be read. A key withdrawn while that condition lasts does NOT take
  effect, so repair or remove the broken `.info` first.

  This is safe only because it never goes quiet, and that is asserted by a test:
  every reload attempt logs at `Error` and moves `ha_keys_load_errors_total`
  again, and `ha_keys_last_successful_reload_timestamp_seconds` stops advancing.
  **The age of that last series is what tells you a process is stuck**, and it is
  the one to alert on — a rate over the error counter tells you it is failing, a
  stale timestamp tells you it has been failing since a specific moment.

Of the ways a keyring can come back short, the one still refused outright is
records that exist and **not one** decoding, which is what a wrong or rotated
passphrase looks like: applying that would release every supplier at once over a
credential the operator can fix. The refusals in the paragraph above — a
directory this process cannot read, a source that errored — are unchanged and are
a different thing: there, nothing was measured at all.

## What fails at startup, and what it tells you

Every one of these is a startup error naming the specific thing to change:

| Situation | Because |
|---|---|
| Both `keys_file` and `keyring` set | Nothing should have to decide at runtime which source wins a duplicate, or what one unreadable source means while the other is fine. |
| Neither set | The process would sign nothing and reject every relay while looking healthy. |
| A source configured that yields **zero** keys | Same reason. The error names the source and the **uid** the process runs as, because "unreadable by this user" is the common cause — a keyring written by a root init container is not readable by a process running as uid 1000. |
| `keyring.dir` empty | It is not a default; it resolves to a relative path. |
| An unsupported backend | See the table above. |
| Both `passphrase_file` and `passphrase_env` | Ambiguous. |
| A passphrase set for `backend: test` | It has none; the setting would be silently ignored. |

## The CLI

`pocket-relay-miner relay` resolves keys by name from a keyring with
`--keyring-backend` and `--keyring-dir`. It accepts the **same** backends and
enforces the same `dir` rule as the services, deliberately: a backend the CLI
accepted and a service refused would only teach you that a key you had just
resolved was usable.

```bash
echo "$SECRET" | pocket-relay-miner relay jsonrpc --service <svc> \
  --node <host:port> --chain-id <id> \
  --keyring-backend file --keyring-dir ~/.pocket \
  --app-key <name> --gateway-key <name>
```

Raw `--app-priv-key` hex is visible to `ps` and in shell history — localnet only.

## Local development

The Tilt stack mounts **both** sources in every relayer and miner pod: the
`supplier-keys` Secret, and a keyring built at pod start from those same keys.
Which one each binary uses is one line in `tilt_config.yaml`, since a config may
name only one:

```yaml
relayer:
  config:
    key_source: keys_file      # keys_file | keyring
    keyring_backend: test      # test | file, when key_source is keyring
miner:
  config:
    key_source: keyring
    keyring_backend: file
```

Changing it rewrites the ConfigMap; the config-hash annotation rolls the pods,
which is required because both binaries read their config only at startup.
