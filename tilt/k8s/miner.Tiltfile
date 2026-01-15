# miner.tilt - Miner deployment (HA mode with leader election)

load("./ports.Tiltfile", "get_miner_ports")
load("./utils.Tiltfile", "deep_merge", "read_miner_example_config", "get_redis_host", "apply_k8s_overrides_miner")

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
        - name: GOMAXPROCS
          value: "4"  # Match CPU limit - makes runtime.NumCPU() return 4
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
    """
    # 1. Read example config as base
    base_config = read_miner_example_config()

    # 2. Merge with user overrides from tilt_config.yaml
    user_overrides = config.get("miner", {}).get("config", {})
    merged_config = deep_merge(base_config, user_overrides)

    # 3. Apply k8s-specific overrides (service names, paths)
    redis_host = get_redis_host(config.get("redis", {}).get("mode", "standalone"))
    final_config = apply_k8s_overrides_miner(merged_config, redis_host)

    return final_config

def format_port_forward(local_port, container_port):
    """Format port forward string"""
    return "{}:{}".format(local_port, container_port)

def link(url, text):
    """Create a Tilt UI link"""
    return url
