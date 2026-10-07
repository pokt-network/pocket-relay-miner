# miner.tilt - Miner deployment (HA mode with leader election)

load("./ports.Tiltfile", "get_miner_ports")
load("./utils.Tiltfile", "deep_merge", "read_miner_example_config", "get_redis_host", "apply_k8s_overrides_miner", "config_hash", "keyring_init_containers")

def deploy_miners(config):
    """Deploy miner as a Deployment with N replicas"""
    if config["miner"]["count"] == 0:
        print("Miners disabled (count: 0)")
        return

    print("Deploying miner Deployment with {} replica(s)...".format(config["miner"]["count"]))

    # Create miner ConfigMap first, and reuse its rendered YAML so the
    # Deployment can carry the config hash that rolls the pods on a change.
    miner_config_yaml = create_miner_configmap(config)

    # Deploy single miner Deployment with replicas
    deploy_miner_deployment(config, config_hash(miner_config_yaml))

def create_miner_configmap(config):
    """Create ConfigMap with miner configuration. Returns the rendered YAML."""
    miner_config_dict = generate_miner_config(config)

    # Add known_applications from parent config if available
    if "known_applications" in config.get("miner", {}):
        miner_config_dict["known_applications"] = config["miner"]["known_applications"]

    miner_config_yaml = str(encode_yaml(miner_config_dict))
    miner_config_indented = miner_config_yaml.replace("\n", "\n    ")

    miner_configmap = """
apiVersion: v1
kind: ConfigMap
metadata:
  name: miner-config
data:
  config.yaml: |
    {}
""".format(miner_config_indented)

    k8s_yaml(blob(miner_configmap))

    return miner_config_yaml

def deploy_miner_deployment(config, miner_config_hash):
    """Deploy miner as a single Deployment with N replicas"""

    # Miner Deployment with replicas + Service
    miner_yaml = """
apiVersion: apps/v1
kind: Deployment
metadata:
  name: miner
  labels:
    app: miner
spec:
  replicas: {replicas}
  selector:
    matchLabels:
      app: miner
  template:
    metadata:
      labels:
        app: miner
      annotations:
        # See config_hash in utils.Tiltfile: a mounted ConfigMap change does not
        # roll pods by itself, and the miner reads its config only at startup.
        pocket-relay-miner/config-hash: "{config_hash}"
    spec:
{keyring_init}
      containers:
      - name: miner
        image: {image}
        imagePullPolicy: Never
        command:
        - pocket-relay-miner
        - miner
        - --config=/config/config.yaml
        ports:
        - containerPort: 9092
          name: metrics
        - containerPort: 6060
          name: pprof
        env:
        - name: LOG_LEVEL
          value: "{log_level}"
        # Soft limit for the Go runtime below the container limit, so the GC
        # tightens before the kernel OOM-kills the pod.
        - name: GOMEMLIMIT
          value: "7GiB"
        - name: POD_NAME
          valueFrom:
            fieldRef:
              fieldPath: metadata.name
        volumeMounts:
        - name: config
          mountPath: /config
        - name: keys
          mountPath: /keys
        - name: keyring
          mountPath: /keyring
        - name: keyring-pass
          mountPath: /keyring-pass
        resources:
          requests:
            cpu: "500m"
            memory: "512Mi"
          limits:
            # From miner.cpu_cores. GOMAXPROCS is NOT set: automaxprocs
            # (main.go:7) derives it from THIS limit, and a present env would make
            # it return without touching anything (maxprocs.go:105-111). The miner
            # is the core component doing SMST, claims and proofs.
            cpu: "{cpu_limit}"
            memory: "8Gi"
        readinessProbe:
          httpGet:
            path: /health
            port: 9092
          initialDelaySeconds: 10
          periodSeconds: 5
      volumes:
      - name: config
        configMap:
          name: miner-config
      - name: keys
        secret:
          secretName: supplier-keys
          optional: true
      - name: keyring
        emptyDir: {{}}
      - name: keyring-pass
        secret:
          secretName: keyring-passphrase
---
apiVersion: v1
kind: Service
metadata:
  name: miner
  labels:
    app: miner
spec:
  selector:
    app: miner
  ports:
  - port: 9092
    targetPort: 9092
    name: metrics
  - port: 6060
    targetPort: 6060
    name: pprof
""".format(
        replicas=config["miner"]["count"],
        config_hash=miner_config_hash,
        image=config["global"]["image"],
        log_level="debug" if config["global"]["debug"] else "info",
        cpu_limit="{}000m".format(config["miner"]["cpu_cores"]),
        keyring_init=keyring_init_containers(),
    )

    k8s_yaml(blob(miner_yaml))

    k8s_resource(
        "miner",
        labels=["relay-miner"],
        resource_deps=["redis", "validator", "account-init"],
        objects=["miner-config:configmap", "supplier-keys:secret"],
        port_forwards=[
            "{}:9092".format(config["miner"]["metrics_base_port"]),
            "{}:{}".format(config["miner"]["pprof_port"], config["miner"]["pprof_port"]),
        ]
    )

def generate_miner_config(config):
    """Generate miner config using example file as base + user overrides + k8s overrides.

    Config layering:
    1. Base: config.miner.example.yaml (single source of truth for defaults)
    2. User overrides: tilt_config.yaml miner.config section
    3. K8s overrides: Redis URL, validator URL, keys path, metrics addr
    4. The localnet clock, which is not the miner's to hold on its own
    """
    # 1. Read example config as base
    base_config = read_miner_example_config()

    # 2. Merge with user overrides from tilt_config.yaml
    user_overrides = config.get("miner", {}).get("config", {})
    merged_config = deep_merge(base_config, user_overrides)

    # 3. Apply k8s-specific overrides (service names, paths)
    redis_host = get_redis_host(config.get("redis", {}).get("mode", "standalone"))
    final_config = apply_k8s_overrides_miner(merged_config, redis_host)

    # 4. THE clock, from localnet.block_time_seconds -- the same number that
    # becomes the validator's timeout_commit. It is forced rather than merged
    # because two places holding one clock is how they drift, and the miner
    # derives its claim and proof deadlines from this value: a divergence
    # miscomputes them with no error anywhere. Forcing an operator's value is
    # announced, the way the mode matrix in utils.Tiltfile announces its own.
    block_time = config["localnet"]["block_time_seconds"]
    stored = final_config.get("block_time_seconds")
    if stored != block_time:
        print("localnet clock: forcing miner block_time_seconds {!r} -> {!r} (set localnet.block_time_seconds instead)".format(
            stored, block_time))
    final_config["block_time_seconds"] = block_time

    return final_config

def format_port_forward(local_port, container_port):
    """Format port forward string"""
    return "{}:{}".format(local_port, container_port)

def link(url, text):
    """Create a Tilt UI link"""
    return url
