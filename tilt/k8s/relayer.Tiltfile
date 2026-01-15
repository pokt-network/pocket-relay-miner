# relayer.tilt - Relayer deployment (stateless, waits for miners)

load("./ports.Tiltfile", "get_relayer_ports")
load("./utils.Tiltfile", "deep_merge", "read_relayer_example_config", "get_redis_host", "apply_k8s_overrides_relayer")

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
    """Generate relayer config using example file as base + user overrides + k8s overrides.

    Config layering:
    1. Base: config.relayer.example.yaml (single source of truth for defaults)
    2. User overrides: tilt_config.yaml relayer.config section
    3. K8s overrides: Redis URL, validator URL, keys path, metrics addr
    """
    # 1. Read example config as base
    base_config = read_relayer_example_config()

    # 2. Merge with user overrides from tilt_config.yaml
    user_overrides = config.get("relayer", {}).get("config", {})
    merged_config = deep_merge(base_config, user_overrides)

    # 3. Apply k8s-specific overrides (service names, paths)
    redis_host = get_redis_host(config.get("redis", {}).get("mode", "standalone"))
    final_config = apply_k8s_overrides_relayer(merged_config, redis_host)

    return final_config

def format_port_forward(local_port, container_port):
    """Format port forward string"""
    return "{}:{}".format(local_port, container_port)

def link(url, text):
    """Create a Tilt UI link"""
    return url
