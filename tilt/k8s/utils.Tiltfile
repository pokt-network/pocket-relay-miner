# utils.tilt - Utility functions

def deep_merge(base, override):
    """Deep merge two dictionaries, with override taking precedence"""
    result = dict(base)

    for key, value in override.items():
        if key in result and type(result[key]) == "dict" and type(value) == "dict":
            result[key] = deep_merge(result[key], value)
        else:
            result[key] = value

    return result

def read_miner_example_config():
    """Read miner example config as base - the example file is the single source of truth for defaults"""
    return read_yaml("config.miner.example.yaml")

def read_relayer_example_config():
    """Read relayer example config as base - the example file is the single source of truth for defaults"""
    return read_yaml("config.relayer.example.yaml")

def get_redis_host(redis_mode):
    """Get Redis host based on mode (standalone/cluster)"""
    if redis_mode == "standalone":
        return "redis-standalone"
    elif redis_mode == "cluster":
        return "redis-cluster-leader"
    else:
        return "redis"

def apply_k8s_overrides_miner(config, redis_host):
    """Apply k8s-specific overrides to miner config (service names, etc.)"""
    config = dict(config)

    # Override Redis URL with k8s service name
    if "redis" not in config:
        config["redis"] = {}
    config["redis"]["url"] = "redis://{}:6379".format(redis_host)

    # Override pocket_node with k8s service names
    if "pocket_node" not in config:
        config["pocket_node"] = {}
    config["pocket_node"]["query_node_rpc_url"] = "http://validator:26657"
    config["pocket_node"]["query_node_grpc_url"] = "validator:9090"
    config["pocket_node"]["grpc_insecure"] = True

    # Point the binary at the key SOURCE this run is exercising.
    #
    # BOTH sources are mounted in every pod -- the keys_file secret and a keyring
    # built from it at pod start -- but a config may name only ONE: setting both
    # is a startup error (keys.ValidateKeySources). Mounting both anyway is what
    # makes switching a one-line config change instead of a redeploy, and the
    # config-hash annotation rolls the pods, which is required because the
    # binaries read their config only at startup.
    #
    #   key_source: keys_file   the WATCHED source; a change lands at once
    #   key_source: keyring     UNWATCHABLE; found by the reload timer (30s)
    if "keys" not in config:
        config["keys"] = {}
    key_source = config.get("key_source", "keys_file")
    config.pop("key_source", None)
    if key_source not in ("keys_file", "keyring"):
        fail("key_source must be keys_file or keyring (they are mutually exclusive); got: " + str(key_source))

    # Which keyring BACKEND, when the source is a keyring. Production uses
    # "file", which is passphrase-protected, so exercising only "test" would
    # leave the backend an operator actually runs untested -- and "file" is the
    # one that panicked until 0b3b929 for want of a password reader.
    keyring_backend = config.get("keyring_backend", "test")
    config.pop("keyring_backend", None)
    if keyring_backend not in ("test", "file"):
        fail("keyring_backend must be test or file -- the only backends a service accepts " +
             "(keys.ValidateKeyringBackend); got: " + str(keyring_backend))

    config["keys"].pop("keys_file", None)
    config["keys"].pop("keyring", None)
    if key_source == "keys_file":
        config["keys"]["keys_file"] = "/keys/supplier-keys.yaml"
    else:
        keyring = {
            "backend": keyring_backend,
            "dir": "/keyring",
            "app_name": "pocket",
        }
        if keyring_backend == "file":
            # A REFERENCE to the mounted secret, not the passphrase: the config
            # stays non-sensitive, exactly like keys_file pointing at a file of
            # private keys. This is also why the container command needs no shell
            # wrapper to redirect stdin.
            keyring["passphrase_file"] = "/keyring-pass/passphrase"
        config["keys"]["keyring"] = keyring

    # Override metrics addr for container
    if "metrics" not in config:
        config["metrics"] = {}
    config["metrics"]["enabled"] = True
    config["metrics"]["addr"] = "0.0.0.0:9092"

    # Override pprof addr for container
    if "pprof" not in config:
        config["pprof"] = {}
    config["pprof"]["enabled"] = True
    config["pprof"]["addr"] = "0.0.0.0:6065"

    # Localnet supplier-lease override. The example default (90s) is sized for
    # production fleets; on the localnet, whose claim window is 100s (10 blocks
    # of 10s, mainnet-proportional), a crashed miner's lease must expire and be
    # taken over well inside that window or the dead pod's relays miss their
    # claim. 30s keeps crash takeover at ~30-90s. Rollouts are unaffected
    # (graceful shutdown releases leases immediately).
    if "supplier_claiming" not in config:
        config["supplier_claiming"] = {}
    config["supplier_claiming"]["claim_ttl_seconds"] = 30

    return config

def apply_k8s_overrides_relayer(config, redis_host):
    """Apply k8s-specific overrides to relayer config (service names, etc.)"""
    config = dict(config)

    # Mode-matrix services: every develop-X service gets a sibling with the
    # OPPOSITE validation_mode, staked in genesis with its own application, so
    # one gate run measures each (rpc_type, validation_mode) cell. The BASE
    # mode is forced explicitly too, so the matrix cannot drift with the
    # example config (it shipped every service as eager, which silently made
    # earlier "optimistic" coverage a lie). A fresh tilt_config.yaml
    # regenerated from a clean checkout yields exactly this truth table.
    # NOTE: gRPC and WebSocket currently ignore validation_mode (issue #24);
    # their -optimistic cells behave eager until that lands, and exist so the
    # matrix is already wired to measure it when it does.
    mode_matrix = {
        "develop-http": ("optimistic", "develop-http-eager", "eager"),
        "develop-websocket": ("eager", "develop-websocket-optimistic", "optimistic"),
        "develop-grpc": ("eager", "develop-grpc-optimistic", "optimistic"),
        "develop-stream": ("eager", "develop-stream-optimistic", "optimistic"),
        "develop-cometbft": ("eager", "develop-cometbft-optimistic", "optimistic"),
    }
    services = config.get("services", {})
    for base, triple in mode_matrix.items():
        base_mode = triple[0]
        clone_id = triple[1]
        clone_mode = triple[2]
        if base in services:
            services[base] = dict(services[base])
            services[base]["validation_mode"] = base_mode
            if clone_id in services:
                # Force the CLONE's mode too. A clone entry that survived in an
                # operator's generated tilt_config.yaml (edited by hand, or
                # stale after the matrix changed) would otherwise run whatever
                # mode it stored -- while the live gate trusts the service
                # NAME for its per-mode verdict, turning a green "eager"
                # column into unexercised coverage. Bases and clones get the
                # same treatment: the mode-matrix table above is the only
                # source of truth.
                stored = services[clone_id].get("validation_mode")
                if stored != clone_mode:
                    print("mode-matrix: forcing {}.validation_mode {!r} -> {!r} (stored value ignored)".format(
                        clone_id, stored, clone_mode))
                services[clone_id] = dict(services[clone_id])
                services[clone_id]["validation_mode"] = clone_mode
            else:
                clone = dict(services[base])
                clone["validation_mode"] = clone_mode
                services[clone_id] = clone
    config["services"] = services

    # Override Redis URL with k8s service name
    if "redis" not in config:
        config["redis"] = {}
    config["redis"]["url"] = "redis://{}:6379".format(redis_host)

    # Override pocket_node with k8s service names
    if "pocket_node" not in config:
        config["pocket_node"] = {}
    config["pocket_node"]["query_node_rpc_url"] = "http://validator:26657"
    config["pocket_node"]["query_node_grpc_url"] = "validator:9090"
    config["pocket_node"]["grpc_insecure"] = True

    # Point the binary at the key SOURCE this run is exercising.
    #
    # BOTH sources are mounted in every pod -- the keys_file secret and a keyring
    # built from it at pod start -- but a config may name only ONE: setting both
    # is a startup error (keys.ValidateKeySources). Mounting both anyway is what
    # makes switching a one-line config change instead of a redeploy, and the
    # config-hash annotation rolls the pods, which is required because the
    # binaries read their config only at startup.
    #
    #   key_source: keys_file   the WATCHED source; a change lands at once
    #   key_source: keyring     UNWATCHABLE; found by the reload timer (30s)
    if "keys" not in config:
        config["keys"] = {}
    key_source = config.get("key_source", "keys_file")
    config.pop("key_source", None)
    if key_source not in ("keys_file", "keyring"):
        fail("key_source must be keys_file or keyring (they are mutually exclusive); got: " + str(key_source))

    # Which keyring BACKEND, when the source is a keyring. Production uses
    # "file", which is passphrase-protected, so exercising only "test" would
    # leave the backend an operator actually runs untested -- and "file" is the
    # one that panicked until 0b3b929 for want of a password reader.
    keyring_backend = config.get("keyring_backend", "test")
    config.pop("keyring_backend", None)
    if keyring_backend not in ("test", "file"):
        fail("keyring_backend must be test or file -- the only backends a service accepts " +
             "(keys.ValidateKeyringBackend); got: " + str(keyring_backend))

    config["keys"].pop("keys_file", None)
    config["keys"].pop("keyring", None)
    if key_source == "keys_file":
        config["keys"]["keys_file"] = "/keys/supplier-keys.yaml"
    else:
        keyring = {
            "backend": keyring_backend,
            "dir": "/keyring",
            "app_name": "pocket",
        }
        if keyring_backend == "file":
            # A REFERENCE to the mounted secret, not the passphrase: the config
            # stays non-sensitive, exactly like keys_file pointing at a file of
            # private keys. This is also why the container command needs no shell
            # wrapper to redirect stdin.
            keyring["passphrase_file"] = "/keyring-pass/passphrase"
        config["keys"]["keyring"] = keyring

    # Override metrics addr for container
    if "metrics" not in config:
        config["metrics"] = {}
    config["metrics"]["enabled"] = True
    config["metrics"]["addr"] = "0.0.0.0:9090"

    # Override pprof addr for container (sibling of metrics, like miner)
    if "pprof" not in config:
        config["pprof"] = {}
    config["pprof"]["enabled"] = True
    config["pprof"]["addr"] = "0.0.0.0:6060"

    # Override health_check addr for container
    if "health_check" not in config:
        config["health_check"] = {}
    config["health_check"]["enabled"] = True
    config["health_check"]["addr"] = "0.0.0.0:8081"

    # Override backend URLs to use k8s service names
    # For develop-http: use multi-backend urls array (backend + backend-2)
    # For other services: keep single url format
    if "services" in config:
        for service_id, service_config in config["services"].items():
            if "backends" in service_config:
                backends = service_config["backends"]
                if service_id == "develop-http" and "jsonrpc" in backends:
                    # Multi-backend: use urls array with both backend pods
                    backends["jsonrpc"]["urls"] = [
                        "http://backend:8545",
                        {"name": "backup", "url": "http://backend-2:8545"},
                    ]
                    # Remove single url if present (mutually exclusive with urls)
                    if "url" in backends["jsonrpc"]:
                        backends["jsonrpc"].pop("url")

    # Localnet-only: enable simulated relays with the well-known dev app+gateway
    # keypairs pinned (one identity per service, since each service uses its own
    # app key). These are PUBLIC localnet dev keys — NEVER enable simulation with
    # them on a real deployment.
    #
    # Injected ONLY when the config does not mention identities at all. This
    # function runs on the deploy path, AFTER tilt_config.yaml has been merged in,
    # so an unconditional assignment here silently discarded whatever the operator
    # wrote: editing simulation identities in tilt_config.yaml had no effect, and
    # the file documented a knob it did not control.
    #
    # The test is key PRESENCE, not truthiness, so the two intents stay
    # distinguishable: an absent `identities` means "I did not configure this,
    # give me the localnet defaults" (what config.relayer.example.yaml ships),
    # while an explicit `identities: []` means "I want none" and must reach the
    # relayer as-is — enabling simulation with nothing pinned is a config error
    # the relayer is supposed to reject, and injecting defaults would hide it.
    _sim = config.get("simulation") or {}
    if "identities" in _sim:
        return config

    _gw1 = "025821a2ac597a034250ac14b772efccf9297aa7c4bea5444564059a7cfb152063"
    config["simulation"] = {
        "enabled": True,
        "max_concurrent": 32,
        "freshness_window_seconds": 60,
        "identities": [
            {"key_id": "sim-http", "enabled": True, "max_rps": 100,
             "app_pubkey_hex": "0397896e9b106df70124a856861cc9be52fac9980e2c7a118a36c19d0198692cc5",
             "gateway_pubkeys_hex": [_gw1], "allowed_services": ["develop-http"]},
            {"key_id": "sim-ws", "enabled": True, "max_rps": 100,
             "app_pubkey_hex": "02ff92de294bea65988bf929d7c159be03f69c4d74dc75682c78751102febf2d8e",
             "gateway_pubkeys_hex": [_gw1], "allowed_services": ["develop-websocket"]},
            {"key_id": "sim-stream", "enabled": True, "max_rps": 100,
             "app_pubkey_hex": "0393251466c074a111fd2f6d19ca7fc956ecca512a6d4ae7a0ad6080fd1560926c",
             "gateway_pubkeys_hex": [_gw1], "allowed_services": ["develop-stream"]},
            {"key_id": "sim-grpc", "enabled": True, "max_rps": 100,
             "app_pubkey_hex": "020499e5ebe4945576ee20a9f0524ddcd6ca7c1bcea726a6cbb2d68cbc25d369a6",
             "gateway_pubkeys_hex": [_gw1], "allowed_services": ["develop-grpc"]},
            {"key_id": "sim-cometbft", "enabled": True, "max_rps": 100,
             "app_pubkey_hex": "0204e2c883e67ff768b9b4261ecb2ab08130ab09c41ca0f7b176f750d90092aa6a",
             "gateway_pubkeys_hex": [_gw1], "allowed_services": ["develop-cometbft"]},
        ],
    }

    return config

def config_hash(config_yaml):
    """Return a short sha256 of a rendered config, for a pod-template annotation.

    Kubernetes does NOT restart pods when a mounted ConfigMap changes: the
    Deployment's pod template is untouched, so there is nothing to roll. Both the
    relayer and the miner read their config once at startup — simulation
    identities in particular are not hot-reloaded — so editing the config used to
    update the ConfigMap and change nothing that was running, with no error and
    no signal. Stamping this hash into the pod template turns a config change
    into a template change, which is what makes Kubernetes roll the pods.

    Truncated to 16 hex chars: this is a change detector, not a security control.
    """
    return str(local(
        "cat << 'PRM_CFG_EOF' | sha256sum | cut -c1-16\n{}\nPRM_CFG_EOF".format(config_yaml),
        quiet=True,
        echo_off=True,
    )).strip()

def dict_get(d, key, default=None):
    """Safe dictionary get with default value"""
    return d.get(key, default) if d else default

def ensure_list(value):
    """Ensure value is a list"""
    if type(value) == "list":
        return value
    elif value == None:
        return []
    else:
        return [value]

def format_port_forward(local_port, container_port):
    """Format port forward string"""
    return "{}:{}".format(local_port, container_port)

def format_env_var(name, value):
    """Format environment variable dict"""
    return {"name": name, "value": str(value)}
