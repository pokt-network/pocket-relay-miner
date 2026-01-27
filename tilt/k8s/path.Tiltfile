# path.Tiltfile - Deploy PATH gateway for integrated testing
#
# PATH is the official Pocket Network gateway that routes client requests to suppliers.
# When enabled, this deploys PATH with localnet configuration to test the full relay flow:
#   - Apps delegate to gateway1
#   - PATH signs relays with gateway1's key on behalf of apps
#   - RelayMiner validates ring signatures and processes relays
#
# PATH repository: https://github.com/pokt-network/path

def deploy_path(config):
    """Deploy PATH gateway with localnet configuration"""

    path_config = config.get("path", {})
    if not path_config.get("enabled", False):
        print("PATH gateway disabled - skipping deployment")
        return

    image = path_config.get("image", "ghcr.io/pokt-network/path")
    tag = path_config.get("tag", "feat-unified-qos")
    full_image = "{}:{}".format(image, tag)

    port = path_config.get("port", 3069)
    metrics_port = path_config.get("metrics_port", 9096)

    # Determine redis resource name based on mode
    redis_mode = config.get("redis", {}).get("mode", "standalone")
    redis_resource = "redis" if redis_mode == "standalone" else "redis-cluster"

    print("Deploying PATH gateway: {}".format(full_image))

    # PATH configuration for localnet
    # Gateway1 is configured in genesis and all apps delegate to it
    # Services: develop-http, develop-websocket, develop-stream, develop-grpc
    # Config format based on: path/e2e/config/config.yaml

    # Determine redis address based on mode
    redis_host = "redis-standalone" if redis_mode == "standalone" else "redis-cluster"

    path_config_yaml = """
# PATH Gateway Configuration for Pocket RelayMiner Localnet
# Gateway: pokt15vzxjqklzjtlz7lahe8z2dfe9nm5vxwwmscne4

# Redis Configuration (for shared state with relayers)
redis_config:
  address: "{redis_host}:6379"
  password: ""
  db: 0
  pool_size: 10
  dial_timeout: 5s
  read_timeout: 3s
  write_timeout: 3s

# Router Configuration
router_config:
  port: {port}
  read_timeout: 30s
  write_timeout: 30s
  idle_timeout: 120s
  websocket_message_buffer_size: 8192

# Logger Configuration
logger_config:
  level: "debug"

# Metrics Configuration
metrics_config:
  prometheus_addr: ":{metrics_port}"
  pprof_addr: ":6060"

# Full Node Configuration
full_node_config:
  rpc_url: http://validator:26657
  grpc_config:
    host_port: validator:9090
    insecure: true
  lazy_mode: false
  session_rollover_blocks: 4

# Gateway Configuration
gateway_config:
  # Centralized mode - gateway owns and signs with app keys directly
  gateway_mode: "centralized"
  gateway_address: "pokt15vzxjqklzjtlz7lahe8z2dfe9nm5vxwwmscne4"
  gateway_private_key_hex: "cf09805c952fa999e9a63a9f434147b0a5abfd10f268879694c6b5a70e1ae177"
  # App private keys from all-keys.yaml (app1-app4)
  owned_apps_private_keys_hex:
    - "2d00ef074d9b51e46886dc9a1df11e7b986611d0f336bdcf1f0adce3e037ec0a"  # app1 - develop-http
    - "7e7571a8c61b0887ff8a9017bb4ad83c016b193234f9dc8b6a8ce10c7c483600"  # app2 - develop-websocket
    - "7cbbaa043b9b63baa7d6bb087483b0a6a9f82596c19dce4c5028eb43e5b63674"  # app3 - develop-stream
    - "84e4f2257f24d9e1517d414b834bbbfa317e0d53fef21c1528a07a5fa8c70d57"  # app4 - develop-grpc

  # Disable reputation for localnet testing (simple endpoint selection)
  reputation_config:
    enabled: false

  # Disable retries for localnet (simplifies debugging)
  retry_config:
    enabled: false

  # Disable health checks for localnet
  active_health_checks:
    enabled: false

  # Service configurations - 4 localnet services
  services:
    - id: develop-http
      type: "generic"
      rpc_types: ["json_rpc"]
      latency_profile: "standard"
    - id: develop-websocket
      type: "generic"
      rpc_types: ["websocket"]
      latency_profile: "standard"
    - id: develop-stream
      type: "generic"
      rpc_types: ["rest"]
      latency_profile: "standard"
    - id: develop-grpc
      type: "generic"
      rpc_types: ["grpc"]
      latency_profile: "standard"
""".format(port=port, metrics_port=metrics_port, redis_host=redis_host)

    # Create PATH ConfigMap
    path_configmap = """
apiVersion: v1
kind: ConfigMap
metadata:
  name: path-config
data:
  config.yaml: |
    {}
""".format(path_config_yaml.replace("\n", "\n    "))

    k8s_yaml(blob(path_configmap))

    # Deploy PATH
    path_deployment = """
apiVersion: apps/v1
kind: Deployment
metadata:
  name: path
  labels:
    app: path
spec:
  replicas: 1
  selector:
    matchLabels:
      app: path
  template:
    metadata:
      labels:
        app: path
    spec:
      containers:
      - name: path
        image: {image}
        imagePullPolicy: IfNotPresent
        command: ["./path"]
        args:
        - --config
        - /config/config.yaml
        ports:
        - name: http
          containerPort: {port}
        - name: metrics
          containerPort: {metrics_port}
        volumeMounts:
        - name: config
          mountPath: /config
        resources:
          requests:
            memory: "1Gi"
            cpu: "1000m"
          limits:
            memory: "2Gi"
            cpu: "4000m"
        readinessProbe:
          httpGet:
            path: /healthz
            port: {port}
          initialDelaySeconds: 5
          periodSeconds: 10
        livenessProbe:
          httpGet:
            path: /healthz
            port: {port}
          initialDelaySeconds: 10
          periodSeconds: 30
      volumes:
      - name: config
        configMap:
          name: path-config
---
apiVersion: v1
kind: Service
metadata:
  name: path
  labels:
    app: path
spec:
  selector:
    app: path
  ports:
  - name: http
    port: {port}
    targetPort: http
  - name: metrics
    port: {metrics_port}
    targetPort: metrics
""".format(image=full_image, port=port, metrics_port=metrics_port)

    k8s_yaml(blob(path_deployment))

    # Resource definition for Tilt UI
    # PATH depends on validator (for chain queries) and redis (for shared state with relayers)
    k8s_resource(
        "path",
        port_forwards=[
            "{}:{}".format(port, port),  # HTTP
            "{}:{}".format(metrics_port, metrics_port),  # Metrics
        ],
        labels=["gateway"],
        objects=["path-config:configmap"],
        resource_deps=["validator", redis_resource],
    )

    print("  PATH gateway will be available at http://localhost:{}".format(port))
    print("  PATH metrics at http://localhost:{}".format(metrics_port))
