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
            "image": "ghcr.io/pokt-network/poktroll",
            "tag": "v0.1.31-rc1",
            "chain_id": "pocket-localnet",
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
                    "over_servicing_enabled": False,
                },
            },
        },
        "miner": {
            "count": 2,
            "metrics_base_port": 9092,
            "pprof_port": 6061,
            "config": {
                "hot_reload_enabled": True,
                "session_ttl": "24h",
                "wal_max_len": 100000,
                "batch_size": 100,
                "cache_refresh_workers": 6,
            },
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
    }
