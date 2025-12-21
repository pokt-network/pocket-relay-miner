# defaults.tilt - Default configuration values

def get_defaults():
    """Returns default configuration for Tilt environment"""
    return {
        "hot_reload": True,
        "global": {
            "image": "pocket-relay-miner",
            "namespace": "default",
            "debug": True,
        },
        "validator": {
            "enabled": True,
            "image": "ghcr.io/pokt-network/pocketd",
            "tag": "0.1.30",
            "chain_id": "pocket",
            "clean_on_restart": True,
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
            "base_port": 8180,  # Avoid conflict with redis_commander (8081)
            "metrics_base_port": 9190,  # Avoid conflict with validator_grpc (9090)
            "health_base_port": 8280,
            "pprof_port": 6060,
            "config": {
                "listen_addr": "0.0.0.0:8080",
                "validation_mode": "optimistic",
                "grace_period_extra_blocks": 2,
                "relay_meter": {
                    "enabled": True,
                },
            },
        },
        "miner": {
            "count": 2,
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
                "scrape_interval": "10s",
            },
            "grafana": {
                "port": 3000,
                "dashboards": ["redis", "relayer", "miner"],
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
