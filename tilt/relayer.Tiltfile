# relayer.tilt - Relayer deployment (stateless, waits for miners)

load("./ports.Tiltfile", "get_relayer_ports")

def deploy_relayers(config):
    """Deploy relayer as a Deployment with N replicas"""
    if config["relayer"]["count"] == 0:
        print("Relayers disabled (count: 0)")
        return

    print("Deploying relayer Deployment with {} replica(s)...".format(config["relayer"]["count"]))

    # Create relayer ConfigMap first
    create_relayer_configmap(config)

    # Deploy single relayer Deployment with replicas
    deploy_relayer_deployment(config)

def create_relayer_configmap(config):
    """Create ConfigMap with relayer configuration"""
    relayer_config_dict = generate_relayer_config(config)
    relayer_config_yaml = str(encode_yaml(relayer_config_dict))
    relayer_config_indented = relayer_config_yaml.replace("\n", "\n    ")

    relayer_configmap = """
apiVersion: v1
kind: ConfigMap
metadata:
  name: relayer-config
data:
  config.yaml: |
    {}
""".format(relayer_config_indented)

    k8s_yaml(blob(relayer_configmap))

def deploy_relayer_deployment(config):
    """Deploy relayer as a single Deployment with N replicas"""

    # Relayer Deployment with replicas + Service
    relayer_yaml = """
apiVersion: apps/v1
kind: Deployment
metadata:
  name: relayer
  labels:
    app: relayer
spec:
  replicas: {}
  selector:
    matchLabels:
      app: relayer
  template:
    metadata:
      labels:
        app: relayer
    spec:
      containers:
      - name: relayer
        image: {}
        imagePullPolicy: Never
        command:
        - pocket-relay-miner
        - relayer
        - --config=/config/config.yaml
        ports:
        - containerPort: 8080
          name: relay
        - containerPort: 9090
          name: metrics
        - containerPort: 8081
          name: health
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
            cpu: "1000m"
            memory: "512Mi"
          limits:
            cpu: "4000m"  # 2 cores - handles relay validation and signing at high RPS
            memory: "2Gi"
        readinessProbe:
          httpGet:
            path: /ready
            port: 8081
          initialDelaySeconds: 10
          periodSeconds: 5
        livenessProbe:
          httpGet:
            path: /health
            port: 8081
          initialDelaySeconds: 30
          periodSeconds: 10
      volumes:
      - name: config
        configMap:
          name: relayer-config
      - name: keys
        secret:
          secretName: supplier-keys
          optional: true
---
apiVersion: v1
kind: Service
metadata:
  name: relayer
  labels:
    app: relayer
spec:
  selector:
    app: relayer
  ports:
  - port: 8080
    targetPort: 8080
    name: relay
  - port: 9090
    targetPort: 9090
    name: metrics
  - port: 8081
    targetPort: 8081
    name: health
  - port: 6060
    targetPort: 6060
    name: pprof
""".format(
        config["relayer"]["count"],
        config["global"]["image"],
        "debug" if config["global"]["debug"] else "info"
    )

    k8s_yaml(blob(relayer_yaml))

    k8s_resource(
        "relayer",
        labels=["relay-miner"],
        objects=["relayer-config:configmap"],
        resource_deps=["redis", "validator", "miner"],
        port_forwards=[
            "{}:8080".format(config["relayer"]["base_port"]),
            "{}:9090".format(config["relayer"]["metrics_base_port"]),
            "{}:8081".format(config["relayer"]["health_base_port"]),
            "{}:{}".format(config["relayer"]["pprof_port"], config["relayer"]["pprof_port"]),
        ]
    )

def generate_relayer_config(config):
    """Generate relayer config from user config + defaults"""
    relayer_cfg = config["relayer"]["config"]

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
        "listen_addr": relayer_cfg.get("listen_addr", "0.0.0.0:8080"),
        "redis": {
            "url": "redis://{}:6379".format(redis_host),
            "stream_prefix": "ha:relays",
            "max_stream_len": relayer_cfg.get("redis", {}).get("max_stream_len", 100000),
        },
        "pocket_node": {
            "query_node_rpc_url": "http://validator:26657",
            "query_node_grpc_url": "validator:9090",
            "grpc_insecure": True,  # Disable TLS for local development
        },
        "keys": {
            "keys_file": "/keys/supplier-keys.yaml",
        },
        "services": relayer_cfg.get("services", {
            "develop": {
                "validation_mode": relayer_cfg.get("validation_mode", "optimistic"),
                "backends": {
                    "jsonrpc": {
                        "url": "http://backend:8545",
                    },
                    "websocket": {
                        "url": "ws://backend:8545/ws",
                    },
                    "grpc": {
                        "url": "backend:50051",
                    },
                },
            },
        }),
        "default_validation_mode": relayer_cfg.get("default_validation_mode", "optimistic"),
        "default_request_timeout_seconds": relayer_cfg.get("default_request_timeout_seconds", 30),
        "default_max_body_size_bytes": relayer_cfg.get("default_max_body_size_bytes", 10485760),  # 10MB default
        "grace_period_extra_blocks": relayer_cfg.get("grace_period_extra_blocks", 2),
        "metrics": {
            "enabled": True,
            "addr": "0.0.0.0:9090",
            "pprof_enabled": True,
            "pprof_addr": "0.0.0.0:6060"
        },
        "health_check": {
            "enabled": True,
            "addr": "0.0.0.0:8081",
        },
        "logging": {
            "level": relayer_cfg.get("logging", {}).get("level", "info"),
            "format": relayer_cfg.get("logging", {}).get("format", "text"),
        },
        "relay_meter": relayer_cfg.get("relay_meter", {"enabled": True, "over_servicing_enabled": False}),
        "http_transport": relayer_cfg.get("http_transport", {}),
        "timeout_profiles": relayer_cfg.get("timeout_profiles", {
            "fast": {
                "response_header_timeout_seconds": 30,
                "dial_timeout_seconds": 5,
                "tls_handshake_timeout_seconds": 10,
            },
            "streaming": {
                "response_header_timeout_seconds": 0,
                "dial_timeout_seconds": 10,
                "tls_handshake_timeout_seconds": 15,
            },
        }),
        "cache_warmup": relayer_cfg.get("cache_warmup", {"enabled": False}),
    }

def format_port_forward(local_port, container_port):
    """Format port forward string"""
    return "{}:{}".format(local_port, container_port)

def link(url, text):
    """Create a Tilt UI link"""
    return url
