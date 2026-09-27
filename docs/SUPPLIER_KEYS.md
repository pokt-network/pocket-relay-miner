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
  - 2d00ef074d9b51e46886dc9a1df11e7b986611d0f336bdcf1f0adce3e037ec0a
  - fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210
```

The operator address of each supplier is **derived from the key**, so the file
lists key material only — there is nothing to keep in sync and no way for an
address and its key to disagree.

Mount it as a Kubernetes Secret or a compose secret. This is the simplest option
and the one the local stack runs by default.

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

**An emptied key file is refused.** A file with no keys is far more often a
truncated write or a bad template than a request to stop serving every supplier
at once, so it is not applied. To stop serving, unstake or stop the process.

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
