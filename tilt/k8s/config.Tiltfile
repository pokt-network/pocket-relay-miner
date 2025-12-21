# config.tilt - Configuration loading and validation

load("./defaults.Tiltfile", "get_defaults")
load("./utils.Tiltfile", "deep_merge", "get_redis_host", "apply_k8s_overrides_miner", "apply_k8s_overrides_relayer")
load("./ports.Tiltfile", "validate_port_conflicts")

CONFIG_FILE = "tilt_config.yaml"

def load_config():
    """Load and validate configuration with defaults"""
    defaults = get_defaults()

    # Check if user config exists
    if not os.path.exists(CONFIG_FILE):
        print("No {} found, generating from defaults...".format(CONFIG_FILE))
        generate_config_file(defaults)
        print("Generated {}. Edit this file to customize your environment.".format(CONFIG_FILE))

    # Load user config
    user_config = read_yaml(CONFIG_FILE, default={})

    # Deep merge with defaults
    merged = deep_merge(defaults, user_config)

    # Validate configuration
    validate_config(merged)

    # Validate port conflicts
    validate_port_conflicts(merged)

    return merged

def generate_config_file(defaults):
    """Generate tilt_config.yaml from defaults + example configs with k8s overrides applied"""
    header = """# Tilt Development Configuration for Pocket RelayMiner
# Edit this file to customize your local development environment.
# This includes the full configuration from example files with k8s service names.

"""
    # Read example configs from project root (paths are relative to working directory)
    miner_example = read_yaml("config.miner.example.yaml")
    relayer_example = read_yaml("config.relayer.example.yaml")

    # Get Redis host based on default mode
    redis_host = get_redis_host(defaults.get("redis", {}).get("mode", "standalone"))

    # Apply k8s overrides to example configs (redis URL, validator URL, keys path, metrics addr)
    miner_config = apply_k8s_overrides_miner(miner_example, redis_host)
    relayer_config = apply_k8s_overrides_relayer(relayer_example, redis_host)

    # Merge into defaults
    full_config = dict(defaults)
    full_config["miner"]["config"] = miner_config
    full_config["relayer"]["config"] = relayer_config

    # Write the config file
    config_yaml = encode_yaml(full_config)
    local("cat > {} << 'EOF'\n{}{}EOF".format(CONFIG_FILE, header, config_yaml))

def validate_config(config):
    """Validate configuration structure and values"""

    # Validate relayer count
    if config["relayer"]["count"] < 0:
        fail("relayer.count must be >= 0")

    # Validate miner count
    if config["miner"]["count"] < 0:
        fail("miner.count must be >= 0")

    # Validate Redis is enabled if any HA components exist
    total_instances = config["relayer"]["count"] + config["miner"]["count"]
    if total_instances > 0 and not config["redis"]["enabled"]:
        fail("Redis must be enabled when using relayers or miners")

    # Validate validator is enabled if relayers/miners are enabled
    if total_instances > 0 and not config["validator"]["enabled"]:
        fail("Validator must be enabled when using relayers or miners")

    # Validate Redis mode
    if config["redis"]["mode"] not in ["standalone", "cluster"]:
        fail("redis.mode must be 'standalone' or 'cluster'")

    return True
