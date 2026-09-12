# defaults.tilt - Default configuration values

def get_defaults():
    """Returns default configuration for Tilt environment"""
    return {

        "global": {
            "image": "pocket-relay-miner",

            "debug": True,
        },
        "localnet": {
            # Which genesis and keys the stack starts from: "default" is
            # tilt/config, "scale" is what scripts/localnet/gen-genesis.go writes
            # to tilt/config/scale. Switching it on a running chain needs a
            # fresh validator.
            "profile": "default",

            # THE localnet clock. One number feeds both the validator's
            # timeout_commit and the miner's block_time_seconds, so they cannot
            # drift apart -- and the miner derives its claim and proof deadlines
            # from its value, so a divergence would miscompute them silently.
            #
            # 30s is beta's clock, and that is the reason it is the default
            # (Jorge): beta is the fastest clock this software has to cope with,
            # so the localnet runs there rather than somewhere no deployment
            # lives. NOT VERIFIED, and offered only as a second reason: at 10s a
            # load is believed never to have crossed target_num_relays (100k per
            # service and session, the same number here and on mainnet), so the
            # difficulty would never have been exercised -- nobody measured that.
            #
            # Loads that measure against mainnet raise it to 60 in
            # tilt_config.yaml, which is gitignored -- that is the point of the
            # knob: changing the clock for a run leaves the tree clean, and
            # scripts/localonly/_state/loadgen/run-base111.sh refuses to run on
            # a dirty tree.
            "block_time_seconds": 30,
        },
        "validator": {
            "enabled": True,
            "image": "ghcr.io/pokt-network/pocketd",
            "tag": "0.1.34",
            "chain_id": "pocket",

            "ports": {
                "grpc": 9090,
                "rpc": 26657,
                "rest": 1317,
            }
        },
        "redis": {
            "enabled": True,
            "mode": "standalone",  # or "cluster"
            "cluster": {
                "redisLeader": 3,
                "redisFollower": 3,
            },
            "max_memory": "512Mi",
        },
        "relayer": {
            "count": 2,

            # THE relayer's CPU. One integer renders both the container's CPU
            # limit and GOMAXPROCS, so they cannot drift apart -- which is
            # exactly what had happened: the limit was raised to 8 cores and the
            # GOMAXPROCS literal stayed at 4, under a comment that claimed they
            # matched. The pod was allowed 8 cores and the Go runtime scheduled
            # on 4, so a load ramp hitting that ceiling would have reported the
            # runtime's limit as this software's.
            #
            # Cores, not a k8s quantity: GOMAXPROCS is an integer >= 1, so
            # "500m" has no honest rendering, while an integer renders "8" and
            # "8000m" with no arithmetic. requests.cpu stays a literal in the
            # template -- it serves the scheduler, not the runtime.
            "cpu_cores": 8,
            "base_port": 8180,  # Avoid conflict with redis_commander (8081)
            "metrics_base_port": 9190,  # Avoid conflict with validator_grpc (9090)
            "health_base_port": 8280,
            "pprof_port": 6060,
            "config": {
                "listen_addr": "0.0.0.0:8080",
                "validation_mode": "optimistic",
                "relay_meter": {
                    "enabled": True,
                },
            },
        },
        "miner": {
            "count": 2,

            # THE miner's CPU -- see relayer.cpu_cores above for why this is one
            # number. 4 is what the miner already had in BOTH places, so this
            # changes nothing about how it runs: it only removes the second
            # place that could have gone stale.
            "cpu_cores": 4,
            "metrics_base_port": 9092,
            "pprof_port": 6065,  # Start at 6065 to avoid conflict with relayer pprof (6060-6064)
            "config": {},  # Config loaded from example file, overridden by tilt_config.yaml
        },
        "backend": {
            "enabled": True,
            "port": 8545,
            "grpc_port": 50051,
            "metrics_port": 9095,
        },
        "observability": {
            "enabled": True,
            "prometheus": {
                "port": 9091,
                "scrape_interval": "2s",
            },
            "grafana": {
                "port": 3000,

            },
        },
        # PATH gateway (optional - for testing full relay flow with gateway signing)
        # When enabled, deploys PATH with localnet gateway and apps configured.
        # This allows testing the complete relay signing flow where:
        #   - Apps delegate to gateway1
        #   - PATH signs relays with gateway1's key on behalf of apps
        #   - RelayMiner verifies ring signatures
        "path": {
            "enabled": True,  # Disabled by default
            "image": "ghcr.io/pokt-network/path",
            "tag": "feat-unified-qos",  # Configurable for speed
            "port": 3069,  # PATH HTTP port
            "metrics_port": 9096,
        },
    }
