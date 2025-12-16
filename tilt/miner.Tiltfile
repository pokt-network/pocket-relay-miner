# miner.tilt - Miner deployment (HA mode with leader election)

load("./ports.Tiltfile", "get_miner_ports")

def deploy_miners(config):
    """Deploy miner as a Deployment with N replicas"""
    if config["miner"]["count"] == 0:
        print("Miners disabled (count: 0)")
        return

    print("Deploying miner Deployment with {} replica(s)...".format(config["miner"]["count"]))

    # Create miner ConfigMap first
    create_miner_configmap(config)

    # Deploy single miner Deployment with replicas
    deploy_miner_deployment(config)

def create_miner_configmap(config):
    """Create ConfigMap with miner configuration"""
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

def deploy_miner_deployment(config):
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
  replicas: {}
  selector:
    matchLabels:
      app: miner
  template:
    metadata:
      labels:
        app: miner
    spec:
      containers:
      - name: miner
        image: {}
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
          value: "{}"
        - name: POD_NAME
          valueFrom:
            fieldRef:
              fieldPath: metadata.name
        volumeMounts:
        - name: config
          mountPath: /config
        - name: keys
          mountPath: /keys
        resources:
          requests:
            cpu: "500m"
            memory: "512Mi"
          limits:
            cpu: "4000m"  # 4 cores - miner is the core component doing SMST, claims, proofs
            memory: "2Gi"
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
        config["miner"]["count"],
        config["global"]["image"],
        "debug" if config["global"]["debug"] else "info"
    )

    k8s_yaml(blob(miner_yaml))

    k8s_resource(
        "miner",
        labels=["relay-miner"],
        resource_deps=["redis", "validator"],
        objects=["miner-config:configmap", "supplier-keys:secret"],
        port_forwards=[
            "{}:9092".format(config["miner"]["metrics_base_port"]),
            "{}:{}".format(config["miner"]["pprof_port"], config["miner"]["pprof_port"]),
        ]
    )

def generate_miner_config(config):
    """Generate miner config from user config + defaults"""
    miner_cfg = config["miner"]["config"]

    # Determine Redis service name based on mode
    # Standalone: redis-standalone (created by Redis CR)
    # Cluster: redis-cluster-leader (created by RedisCluster CR)
    redis_mode = config["redis"]["mode"]
    if redis_mode == "standalone":
        redis_host = "redis-standalone"
    elif redis_mode == "cluster":
        redis_host = "redis-cluster-leader"
    else:
        redis_host = "redis"

    return {
        "redis": {
            "url": "redis://{}:6379".format(redis_host),
            "stream_prefix": "ha:relays",
            "consumer_group": "ha-miners",
            # consumer_name is auto-generated from os.Hostname() (pod name in k8s)
            # Format: "miner-<pod_name>-<pid>" (see cmd/cmd_miner.go:528-532)
            "block_timeout_ms": miner_cfg.get("redis", {}).get("block_timeout_ms", 5000),
            "claim_idle_timeout_ms": miner_cfg.get("redis", {}).get("claim_idle_timeout_ms", 60000),
        },
        "pocket_node": {
            "query_node_rpc_url": "http://validator:26657",
            "query_node_grpc_url": "validator:9090",
            "grpc_insecure": True,  # Disable TLS for local development
        },
        "keys": {
            "keys_file": "/keys/supplier-keys.yaml",
        },
        "session_tree": {
            "wal_enabled": miner_cfg.get("session_tree", {}).get("wal_enabled", True),
            "flush_interval_blocks": miner_cfg.get("session_tree", {}).get("flush_interval_blocks", 1),
        },
        "metrics": {
            "enabled": True,
            "addr": "0.0.0.0:9092",
            "pprof_enabled": True,
            "pprof_addr": "0.0.0.0:6065"
        },
        "logging": {
            "level": miner_cfg.get("logging", {}).get("level", "info"),
            "format": miner_cfg.get("logging", {}).get("format", "text"),
        },
        "deduplication_ttl_blocks": miner_cfg.get("deduplication_ttl_blocks", 10),
        "batch_size": miner_cfg.get("batch_size", 100),
        "ack_batch_size": miner_cfg.get("ack_batch_size", 50),
        "hot_reload_enabled": miner_cfg.get("hot_reload_enabled", True),
        "session_ttl": miner_cfg.get("session_ttl", "24h"),
        "wal_max_len": miner_cfg.get("wal_max_len", 100000),
        "cache_refresh_workers": miner_cfg.get("cache_refresh_workers", 6),
        # New features
        "block_time_seconds": miner_cfg.get("block_time_seconds", 10),
        "block_health_monitor": {
            "enabled": miner_cfg.get("block_health_monitor", {}).get("enabled", True),
            "slowness_threshold": miner_cfg.get("block_health_monitor", {}).get("slowness_threshold", 1.5),
        },
        "balance_monitor": {
            "enabled": miner_cfg.get("balance_monitor", {}).get("enabled", True),
            "check_interval_seconds": miner_cfg.get("balance_monitor", {}).get("check_interval_seconds", 300),
            "balance_threshold_upokt": miner_cfg.get("balance_monitor", {}).get("balance_threshold_upokt", 1000000),
            "stake_warning_proof_threshold": miner_cfg.get("balance_monitor", {}).get("stake_warning_proof_threshold", 1000),
            "stake_critical_proof_threshold": miner_cfg.get("balance_monitor", {}).get("stake_critical_proof_threshold", 100),
        },
        "session_lifecycle": {
            "window_start_buffer_blocks": miner_cfg.get("session_lifecycle", {}).get("window_start_buffer_blocks", 10),
            "submission_buffer_blocks": miner_cfg.get("session_lifecycle", {}).get("submission_buffer_blocks", 2),
            "stream_discovery_interval_seconds": miner_cfg.get("session_lifecycle", {}).get("stream_discovery_interval_seconds", 10),
        },
    }

def format_port_forward(local_port, container_port):
    """Format port forward string"""
    return "{}:{}".format(local_port, container_port)

def link(url, text):
    """Create a Tilt UI link"""
    return url
