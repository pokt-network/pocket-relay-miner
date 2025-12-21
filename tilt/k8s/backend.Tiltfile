# backend.tilt - Demo backend server deployment

load("./ports.Tiltfile", "get_port")

def deploy_backend(config):
    """Deploy demo backend server"""
    if not config["backend"]["enabled"]:
        print("Backend disabled")
        return

    print("Deploying demo backend server...")

    backend_config = config["backend"]

    # Get resource limits from config with defaults
    resources = backend_config.get("resources", {})
    requests = resources.get("requests", {})
    limits = resources.get("limits", {})

    cpu_request = requests.get("cpu", "200m")
    memory_request = requests.get("memory", "256Mi")
    cpu_limit = limits.get("cpu", "2000m")
    memory_limit = limits.get("memory", "1Gi")

    # Build backend server Docker image
    # Only watch specific files to prevent unnecessary rebuilds
    docker_build(
        "demo-backend",
        context="tilt/backend-server",
        dockerfile="tilt/backend-server/Dockerfile",
        only=[
            "main.go",
            "config.yaml",
            "go.mod",
            "go.sum",
            "pb/demo.proto",
            "pb/demo.pb.go",
            "pb/demo_grpc.pb.go",
        ],
        live_update=[
            # Sync Go source changes
            sync("tilt/backend-server/main.go", "/app/main.go"),
        ],
    )

    # Backend Deployment
    backend_yaml = """
apiVersion: v1
kind: Service
metadata:
  name: backend
  labels:
    app: backend
spec:
  selector:
    app: backend
  ports:
  - port: 8545
    targetPort: 8545
    name: http
  - port: 50051
    targetPort: 50051
    name: grpc
  - port: 9095
    targetPort: 9095
    name: metrics
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: backend
  labels:
    app: backend
spec:
  replicas: 1
  selector:
    matchLabels:
      app: backend
  template:
    metadata:
      labels:
        app: backend
    spec:
      containers:
      - name: backend
        image: demo-backend
        imagePullPolicy: IfNotPresent
        ports:
        - containerPort: 8545
          name: http
        - containerPort: 50051
          name: grpc
        - containerPort: 9095
          name: metrics
        resources:
          requests:
            cpu: "{}"
            memory: "{}"
          limits:
            cpu: "{}"
            memory: "{}"
        readinessProbe:
          httpGet:
            path: /health
            port: 8545
          initialDelaySeconds: 5
          periodSeconds: 5
        livenessProbe:
          httpGet:
            path: /health
            port: 8545
          initialDelaySeconds: 10
          periodSeconds: 10
""".format(
        cpu_request,
        memory_request,
        cpu_limit,
        memory_limit
    )

    k8s_yaml(blob(backend_yaml))

    k8s_resource(
        "backend",
        labels=["backends"],
        port_forwards=[
            format_port_forward(backend_config["port"], 8545),
            format_port_forward(backend_config["grpc_port"], 50051),
            format_port_forward(backend_config["metrics_port"], 9095),
        ]
    )

def format_port_forward(local_port, container_port):
    """Format port forward string"""
    return "{}:{}".format(local_port, container_port)

def link(url, text):
    """Create a Tilt UI link"""
    return url
