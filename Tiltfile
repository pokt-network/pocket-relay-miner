# Tiltfile - Main orchestrator for Pocket RelayMiner local development

# Disable analytics
analytics_settings(enable=False)

# Allow Tilt to discover resources in all namespaces
allow_k8s_contexts('kind-kind')

# Load Tilt extensions
load("ext://restart_process", "docker_build_with_restart")
k8s_yaml(blob("""
apiVersion: v1
kind: Namespace
metadata:
  name: redis-operator
"""))

# Load Tilt extensions
load("ext://restart_process", "docker_build_with_restart")
load("ext://secret", "secret_create_generic")

# Load local modules
load("./tilt/k8s/config.Tiltfile", "load_config")
load("./tilt/k8s/redis.Tiltfile", "deploy_redis")
load("./tilt/k8s/validator.Tiltfile", "deploy_validator")
load("./tilt/k8s/account-init.Tiltfile", "deploy_account_init")
load("./tilt/k8s/miner.Tiltfile", "deploy_miners", "generate_miner_config")
load("./tilt/k8s/relayer.Tiltfile", "deploy_relayers", "generate_relayer_config")
load("./tilt/k8s/backend.Tiltfile", "deploy_backend")
load("./tilt/k8s/nginx-backend.Tiltfile", "provision_nginx_backend")
load("./tilt/k8s/observability.Tiltfile", "deploy_observability")

print("=" * 60)
print("Pocket RelayMiner - Local Development Environment")
print("=" * 60)

# Load configuration (SINGLE SOURCE OF TRUTH)
config = load_config()

print("\nConfiguration:")
print("  Chain ID: {}".format(config["validator"]["chain_id"]))
print("  Relayers: {}".format(config["relayer"]["count"]))
print("  Miners: {}".format(config["miner"]["count"]))
print("  Redis mode: {}".format(config["redis"]["mode"]))
print("  Hot reload: {}".format(config["hot_reload"]))
print("  Observability: {}".format(config["observability"]["enabled"]))
print()

# Build Docker image (always build inside container)
# Ignore non-code files to prevent unnecessary rebuilds
# NOTE: Patterns mirror .dockerignore for consistency
docker_build(
    config["global"]["image"],
    context=".",
    dockerfile="Dockerfile",
    ignore=[
        # Version Control
        ".git/",
        ".gitignore",
        ".github/",
        # IDE & Editors
        ".idea/",
        ".vscode/",
        ".claude/",
        # Build Artifacts
        "bin/",
        "pocket-relay-miner",
        "coverage.out",
        "coverage.html",
        "*.prof",
        "*.test",
        # Documentation
        "*.md",
        "*.txt",
        # Tilt & Development
        ".tilt-tmp/",
        "tilt_config.yaml",
        # Backend Server (separate build)
        "tilt/backend-server/",
    ],
)

# Load all-keys.yaml to extract supplier keys and application addresses
all_keys_path = "tilt/config/all-keys.yaml"
supplier_keys = []
known_applications = []
if os.path.exists(all_keys_path):
    all_keys = read_yaml(all_keys_path)
    for account in all_keys.get("accounts", []):
        name = account.get("name", "")
        # Extract supplier private keys (just the hex string)
        if name.startswith("supplier"):
            supplier_keys.append(account.get("private_key"))
        # Extract application addresses
        if name.startswith("app"):
            known_applications.append(account.get("address"))

# Add known_applications to config for miner to use
config["miner"]["known_applications"] = known_applications

# Create genesis ConfigMap from tilt/config/genesis.json
genesis_path = "tilt/config/genesis.json"
if os.path.exists(genesis_path):
    genesis_configmap = """
apiVersion: v1
kind: ConfigMap
metadata:
  name: genesis-config
data:
  genesis.json: |
    {}
""".format(str(read_file(genesis_path)).replace("\n", "\n    "))
    k8s_yaml(blob(genesis_configmap))
else:
    print("WARNING: Genesis file not found: {}".format(genesis_path))
    print("         Validator will use default genesis (will fail without validators)")

# Create all-keys ConfigMap for account initialization
if os.path.exists(all_keys_path):
    all_keys_content = str(read_file(all_keys_path)).replace("\n", "\n    ")
    all_keys_configmap = """
apiVersion: v1
kind: ConfigMap
metadata:
  name: all-keys-config
data:
  all-keys.yaml: |
    {}
""".format(all_keys_content)
    k8s_yaml(blob(all_keys_configmap))

# Create Secrets for validator config files
validator_config_files = [
    "tilt/config/priv_validator_key.json",
    "tilt/config/node_key.json",
    "tilt/config/app.toml",
    "tilt/config/config.toml",
    "tilt/config/client.toml",
]
from_files = []
for fpath in validator_config_files:
    if os.path.exists(fpath):
        fname = fpath.split("/")[-1]
        from_files.append("{}={}".format(fname, fpath))

if len(from_files) >= 2:  # At minimum need priv_validator_key.json and node_key.json
    secret_create_generic("validator-keys", from_file=from_files)
else:
    print("WARNING: Validator config files not found in tilt/config/")
    print("         Expected: priv_validator_key.json, node_key.json, app.toml, config.toml, client.toml")

# Create supplier-keys Secret from extracted supplier keys
# Format: keys: ["hex1", "hex2", ...] (simple array of private key hex strings)
if len(supplier_keys) > 0:
    supplier_keys_yaml = str(encode_yaml({"keys": supplier_keys}))
    supplier_keys_indented = supplier_keys_yaml.replace("\n", "\n    ")
    supplier_keys_secret = """
apiVersion: v1
kind: Secret
metadata:
  name: supplier-keys
type: Opaque
stringData:
  supplier-keys.yaml: |
    {}
""".format(supplier_keys_indented)
    k8s_yaml(blob(supplier_keys_secret))
else:
    print("WARNING: No supplier keys found in all-keys.yaml")
    print("         Relayers/miners will fail to start without supplier keys")

# Deploy infrastructure (order matters: Redis → Validator → Account Init → Miners → Relayers)
print("Deploying infrastructure...")
deploy_redis(config)
deploy_validator(config)
deploy_account_init(config)

# Deploy relay-miner components
# IMPORTANT: Miners MUST start before Relayers (cache population dependency)
print("Deploying relay-miner components...")
deploy_miners(config)
deploy_relayers(config)

# Deploy backends
print("Deploying backend services...")
deploy_backend(config)
provision_nginx_backend()

# Deploy observability (optional)
if config["observability"]["enabled"]:
    print("Deploying observability stack...")
    deploy_observability(config)

print()
print("=" * 60)
print("✓ Tilt setup complete!")
print("=" * 60)
print()
print("Quick Links:")
print("  - Tilt UI: http://localhost:10350")
print("  - Grafana: http://localhost:{}".format(config["observability"]["grafana"]["port"]))
print("  - Validator Rest: http://localhost:{}".format(config["validator"]["ports"]["rest"]))
print("  - Validator RPC: http://localhost:{}".format(config["validator"]["ports"]["rpc"]))
print("  - Validator gRpc: http://localhost:{}".format(config["validator"]["ports"]["grpc"]))
print()
print("Commands:")
print("  tilt up       - Start all services")
print("  tilt down     - Stop all services")
print("  tilt logs <resource> - View logs for a specific resource")
print()
