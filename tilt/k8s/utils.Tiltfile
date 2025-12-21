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

    # Override keys path for k8s volume mount
    if "keys" not in config:
        config["keys"] = {}
    config["keys"]["keys_file"] = "/keys/supplier-keys.yaml"

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

    return config

def apply_k8s_overrides_relayer(config, redis_host):
    """Apply k8s-specific overrides to relayer config (service names, etc.)"""
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

    # Override keys path for k8s volume mount
    if "keys" not in config:
        config["keys"] = {}
    config["keys"]["keys_file"] = "/keys/supplier-keys.yaml"

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
    if "services" in config:
        for service_id, service_config in config["services"].items():
            if "backends" in service_config:
                backends = service_config["backends"]
                # The example file already has "backend:xxx" which is the k8s service name
                # No changes needed here

    return config

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

# ============================================================================
# ConfigMap and Secret Creation
# ============================================================================

def create_genesis_configmap():
    """Create genesis ConfigMap from config/genesis.json"""
    configmap = """
apiVersion: v1
kind: ConfigMap
metadata:
  name: genesis-config
data:
  genesis.json: |
    {}
""".format(str(read_file("../config/genesis.json")).replace("\n", "\n    "))
    k8s_yaml(blob(configmap))

def create_all_keys_configmap():
    """Create all-keys ConfigMap from config/all-keys.yaml"""
    configmap = """
apiVersion: v1
kind: ConfigMap
metadata:
  name: all-keys-config
data:
  all-keys.yaml: |
    {}
""".format(str(read_file("../config/all-keys.yaml")).replace("\n", "\n    "))
    k8s_yaml(blob(configmap))

def create_validator_secrets():
    """Create validator secrets from config files"""
    # Create secret YAML (using stringData for convenience)
    secret = """
apiVersion: v1
kind: Secret
metadata:
  name: validator-keys
type: Opaque
stringData:
  priv_validator_key.json: |
    {}
  node_key.json: |
    {}
  app.toml: |
    {}
  config.toml: |
    {}
  client.toml: |
    {}
""".format(
        str(read_file("../config/priv_validator_key.json")).replace("\n", "\n    "),
        str(read_file("../config/node_key.json")).replace("\n", "\n    "),
        str(read_file("../config/app.toml")).replace("\n", "\n    "),
        str(read_file("../config/config.toml")).replace("\n", "\n    "),
        str(read_file("../config/client.toml")).replace("\n", "\n    ")
    )
    k8s_yaml(blob(secret))

def create_supplier_keys_secret():
    """Create supplier keys secret from config/supplier-keys.yaml"""
    secret = """
apiVersion: v1
kind: Secret
metadata:
  name: supplier-keys
type: Opaque
stringData:
  supplier-keys.yaml: |
    {}
""".format(str(read_file("../config/supplier-keys.yaml")).replace("\n", "\n    "))
    k8s_yaml(blob(secret))
