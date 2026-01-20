# observability.tilt - Prometheus, Grafana, Loki, and Promtail deployment

load("./ports.Tiltfile", "get_port")

def deploy_observability(config):
    """Deploy Prometheus, Grafana, Loki, and Promtail"""
    if not config["observability"]["enabled"]:
        print("Observability disabled")
        return

    print("Deploying observability stack (Prometheus + Grafana + Loki + Promtail)...")

    deploy_prometheus(config)
    deploy_loki(config)
    deploy_promtail(config)
    deploy_grafana(config)

def deploy_prometheus(config):
    """Deploy Prometheus for metrics collection"""
    prom_config = config["observability"]["prometheus"]

    # Prometheus RBAC (ServiceAccount, ClusterRole, ClusterRoleBinding)
    # Required for kubernetes service discovery to work
    prometheus_rbac_yaml = """
apiVersion: v1
kind: ServiceAccount
metadata:
  name: prometheus
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: prometheus
rules:
  - apiGroups: [""]
    resources:
      - nodes
      - nodes/proxy
      - services
      - endpoints
      - pods
    verbs: ["get", "list", "watch"]
  - apiGroups:
      - extensions
    resources:
      - ingresses
    verbs: ["get", "list", "watch"]
  - nonResourceURLs: ["/metrics"]
    verbs: ["get"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: prometheus
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: prometheus
subjects:
  - kind: ServiceAccount
    name: prometheus
    namespace: default
"""

    k8s_yaml(blob(prometheus_rbac_yaml))

    # Prometheus ConfigMap
    prometheus_config_yaml = """
apiVersion: v1
kind: ConfigMap
metadata:
  name: prometheus-config
data:
  prometheus.yml: |
    global:
      scrape_interval: {}
      evaluation_interval: {}

    scrape_configs:
      # Redis metrics (redis-exporter on port 9121)
      - job_name: 'redis'
        static_configs:
          - targets: ['redis-standalone:9121']

      # Relayer metrics (port 9090)
      - job_name: 'relayers'
        kubernetes_sd_configs:
          - role: pod
        relabel_configs:
          - source_labels: [__meta_kubernetes_pod_label_app]
            regex: relayer
            action: keep
          - source_labels: [__meta_kubernetes_pod_name]
            target_label: exported_instance
          - source_labels: [__meta_kubernetes_pod_label_app]
            target_label: instance
          # Target metrics port explicitly
          - source_labels: [__address__]
            regex: ([^:]+)(?::\\d+)?
            target_label: __address__
            replacement: ${{1}}:9090

      # Miner metrics (port 9092)
      - job_name: 'miners'
        kubernetes_sd_configs:
          - role: pod
        relabel_configs:
          - source_labels: [__meta_kubernetes_pod_label_app]
            regex: miner
            action: keep
          - source_labels: [__meta_kubernetes_pod_name]
            target_label: exported_instance
          - source_labels: [__meta_kubernetes_pod_label_app]
            target_label: instance
          # Target metrics port explicitly
          - source_labels: [__address__]
            regex: ([^:]+)(?::\\d+)?
            target_label: __address__
            replacement: ${{1}}:9092

      # Backend metrics
      - job_name: 'backend'
        static_configs:
          - targets: ['backend:9095']
""".format(prom_config["scrape_interval"], prom_config["scrape_interval"])

    k8s_yaml(blob(prometheus_config_yaml))

    # Prometheus Deployment
    prometheus_yaml = """
apiVersion: v1
kind: Service
metadata:
  name: prometheus
  labels:
    app: prometheus
spec:
  selector:
    app: prometheus
  ports:
  - port: 9091
    targetPort: 9090
    name: http
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: prometheus
  labels:
    app: prometheus
spec:
  replicas: 1
  selector:
    matchLabels:
      app: prometheus
  template:
    metadata:
      labels:
        app: prometheus
    spec:
      serviceAccountName: prometheus
      containers:
      - name: prometheus
        image: prom/prometheus:latest
        args:
          - '--config.file=/etc/prometheus/prometheus.yml'
          - '--storage.tsdb.path=/prometheus'
          - '--web.console.libraries=/usr/share/prometheus/console_libraries'
          - '--web.console.templates=/usr/share/prometheus/consoles'
        ports:
        - containerPort: 9090
          name: http
        volumeMounts:
        - name: config
          mountPath: /etc/prometheus
        - name: storage
          mountPath: /prometheus
        resources:
          requests:
            cpu: "100m"
            memory: "256Mi"
          limits:
            cpu: "500m"
            memory: "1Gi"
      volumes:
      - name: config
        configMap:
          name: prometheus-config
      - name: storage
        emptyDir: {}
"""

    k8s_yaml(blob(prometheus_yaml))

    k8s_resource(
        "prometheus",
        labels=["observability"],
        objects=[
            "prometheus-config:configmap",
            "prometheus:serviceaccount",
            "prometheus:clusterrole",
            "prometheus:clusterrolebinding"
        ],
        port_forwards=[format_port_forward(prom_config["port"], 9090)]
    )

def deploy_loki(config):
    """Deploy Loki for log aggregation"""
    loki_config = config["observability"].get("loki", {"port": 3100})

    # Loki ConfigMap
    loki_config_yaml = """
apiVersion: v1
kind: ConfigMap
metadata:
  name: loki-config
data:
  loki-config.yaml: |
    auth_enabled: false
    server:
      http_listen_port: 3100
      grpc_listen_port: 9096
    common:
      instance_addr: 127.0.0.1
      path_prefix: /loki
      storage:
        filesystem:
          chunks_directory: /loki/chunks
          rules_directory: /loki/rules
      replication_factor: 1
      ring:
        kvstore:
          store: inmemory
    query_range:
      results_cache:
        cache:
          embedded_cache:
            enabled: true
            max_size_mb: 100
    schema_config:
      configs:
        - from: 2020-10-24
          store: tsdb
          object_store: filesystem
          schema: v13
          index:
            prefix: index_
            period: 24h
    ruler:
      alertmanager_url: http://localhost:9093
    limits_config:
      reject_old_samples: true
      reject_old_samples_max_age: 168h
      ingestion_rate_mb: 16
      ingestion_burst_size_mb: 24
      max_entries_limit_per_query: 50000
      volume_enabled: true
      discover_log_levels: true
      discover_service_name:
        - service
        - app
        - component
"""

    k8s_yaml(blob(loki_config_yaml))

    # Loki Deployment
    loki_yaml = """
apiVersion: v1
kind: Service
metadata:
  name: loki
  labels:
    app: loki
spec:
  selector:
    app: loki
  ports:
  - port: 3100
    targetPort: 3100
    name: http
  - port: 9096
    targetPort: 9096
    name: grpc
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: loki
  labels:
    app: loki
spec:
  replicas: 1
  selector:
    matchLabels:
      app: loki
  template:
    metadata:
      labels:
        app: loki
    spec:
      containers:
      - name: loki
        image: grafana/loki:3.3.0
        args:
          - '-config.file=/etc/loki/loki-config.yaml'
        ports:
        - containerPort: 3100
          name: http
        - containerPort: 9096
          name: grpc
        volumeMounts:
        - name: config
          mountPath: /etc/loki
        - name: storage
          mountPath: /loki
        resources:
          requests:
            cpu: "100m"
            memory: "256Mi"
          limits:
            cpu: "500m"
            memory: "1Gi"
      volumes:
      - name: config
        configMap:
          name: loki-config
      - name: storage
        emptyDir: {}
"""

    k8s_yaml(blob(loki_yaml))

    k8s_resource(
        "loki",
        labels=["observability"],
        objects=["loki-config:configmap"],
        port_forwards=[format_port_forward(loki_config.get("port", 3100), 3100)]
    )

def deploy_promtail(config):
    """Deploy Promtail for log collection from all pods"""

    # Promtail RBAC
    promtail_rbac_yaml = """
apiVersion: v1
kind: ServiceAccount
metadata:
  name: promtail
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: promtail
rules:
  - apiGroups: [""]
    resources:
      - nodes
      - nodes/proxy
      - services
      - endpoints
      - pods
    verbs: ["get", "list", "watch"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: promtail
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: promtail
subjects:
  - kind: ServiceAccount
    name: promtail
    namespace: default
"""

    k8s_yaml(blob(promtail_rbac_yaml))

    # Promtail ConfigMap
    promtail_config_yaml = """
apiVersion: v1
kind: ConfigMap
metadata:
  name: promtail-config
data:
  promtail-config.yaml: |
    server:
      http_listen_port: 9080
      grpc_listen_port: 0
    positions:
      filename: /tmp/positions.yaml
    clients:
      - url: http://loki:3100/loki/api/v1/push
    scrape_configs:
      - job_name: kubernetes-pods
        kubernetes_sd_configs:
          - role: pod
        pipeline_stages:
          - cri: {}
          # Parse JSON logs and extract level field
          - json:
              expressions:
                level: level
                component: component
                module: module
          # Add level as a label (critical for Loki filtering)
          - labels:
              level:
              component:
        relabel_configs:
          # Filter to only collect logs from our apps FIRST
          - source_labels: [__meta_kubernetes_pod_label_app]
            regex: (miner|relayer|validator|path|backend|redis.*)
            action: keep
          # Drop init containers
          - source_labels: [__meta_kubernetes_pod_container_init]
            regex: "true"
            action: drop
          # Set the __path__ label to find log files
          # CRI path format: /var/log/pods/<namespace>_<pod_name>_<pod_uid>/<container_name>/*.log
          - source_labels: [__meta_kubernetes_namespace, __meta_kubernetes_pod_name, __meta_kubernetes_pod_uid, __meta_kubernetes_pod_container_name]
            target_label: __path__
            separator: ;
            regex: (.*);(.*);(.*);(.*)
            replacement: /var/log/pods/${1}_${2}_${3}/${4}/*.log
          # Set labels
          - source_labels: [__meta_kubernetes_pod_label_app]
            target_label: app
          - source_labels: [__meta_kubernetes_pod_name]
            target_label: pod
          - source_labels: [__meta_kubernetes_namespace]
            target_label: namespace
          - source_labels: [__meta_kubernetes_pod_container_name]
            target_label: container
"""

    k8s_yaml(blob(promtail_config_yaml))

    # Promtail DaemonSet (runs on every node to collect logs)
    promtail_yaml = """
apiVersion: apps/v1
kind: DaemonSet
metadata:
  name: promtail
  labels:
    app: promtail
spec:
  selector:
    matchLabels:
      app: promtail
  template:
    metadata:
      labels:
        app: promtail
    spec:
      serviceAccountName: promtail
      containers:
      - name: promtail
        image: grafana/promtail:3.3.0
        args:
          - '-config.file=/etc/promtail/promtail-config.yaml'
        ports:
        - containerPort: 9080
          name: http
        volumeMounts:
        - name: config
          mountPath: /etc/promtail
        - name: varlog
          mountPath: /var/log
          readOnly: true
        - name: varlibdockercontainers
          mountPath: /var/lib/docker/containers
          readOnly: true
        - name: pods
          mountPath: /var/log/pods
          readOnly: true
        resources:
          requests:
            cpu: "50m"
            memory: "64Mi"
          limits:
            cpu: "200m"
            memory: "256Mi"
        env:
        - name: HOSTNAME
          valueFrom:
            fieldRef:
              fieldPath: spec.nodeName
      volumes:
      - name: config
        configMap:
          name: promtail-config
      - name: varlog
        hostPath:
          path: /var/log
      - name: varlibdockercontainers
        hostPath:
          path: /var/lib/docker/containers
      - name: pods
        hostPath:
          path: /var/log/pods
"""

    k8s_yaml(blob(promtail_yaml))

    k8s_resource(
        "promtail",
        labels=["observability"],
        resource_deps=["loki"],
        objects=[
            "promtail-config:configmap",
            "promtail:serviceaccount",
            "promtail:clusterrole",
            "promtail:clusterrolebinding"
        ]
    )

def deploy_grafana(config):
    """Deploy Grafana for metrics visualization"""
    grafana_config = config["observability"]["grafana"]

    # Load dashboard JSON files (reorganized by concern)
    dashboard_unified_overview = str(read_file("tilt/grafana/dashboards/unified-overview.json"))

    # Dashboard provisioning configuration
    dashboards_provisioning_yaml = """
apiVersion: v1
kind: ConfigMap
metadata:
  name: grafana-dashboards-provisioning
data:
  dashboards.yaml: |
    apiVersion: 1
    providers:
      - name: 'default'
        orgId: 1
        folder: ''
        type: file
        disableDeletion: false
        updateIntervalSeconds: 10
        allowUiUpdates: true
        options:
          path: /var/lib/grafana/dashboards
"""

    k8s_yaml(blob(dashboards_provisioning_yaml))

    # Dashboard ConfigMap with all concern-based dashboards
    dashboards_yaml = """
apiVersion: v1
kind: ConfigMap
metadata:
  name: grafana-dashboards
data:
  unified-overview.json: |
    {}
""".format(
        dashboard_unified_overview.replace("\n", "\n    ")
    )

    k8s_yaml(blob(dashboards_yaml))

    # Grafana Deployment
    grafana_yaml = """
apiVersion: v1
kind: ConfigMap
metadata:
  name: grafana-datasources
data:
  datasources.yaml: |
    apiVersion: 1
    datasources:
      - name: Prometheus
        type: prometheus
        access: proxy
        url: http://prometheus:9091
        isDefault: true
      - name: Loki
        type: loki
        access: proxy
        url: http://loki:3100
        jsonData:
          maxLines: 1000
---
apiVersion: v1
kind: Service
metadata:
  name: grafana
  labels:
    app: grafana
spec:
  selector:
    app: grafana
  ports:
  - port: 3000
    targetPort: 3000
    name: http
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: grafana
  labels:
    app: grafana
spec:
  replicas: 1
  selector:
    matchLabels:
      app: grafana
  template:
    metadata:
      labels:
        app: grafana
    spec:
      containers:
      - name: grafana
        image: grafana/grafana:latest
        ports:
        - containerPort: 3000
          name: http
        env:
        - name: GF_AUTH_ANONYMOUS_ENABLED
          value: "true"
        - name: GF_AUTH_ANONYMOUS_ORG_ROLE
          value: "Admin"
        - name: GF_SECURITY_ADMIN_PASSWORD
          value: "admin"
        volumeMounts:
        - name: datasources
          mountPath: /etc/grafana/provisioning/datasources
        - name: dashboards-provisioning
          mountPath: /etc/grafana/provisioning/dashboards
        - name: dashboards
          mountPath: /var/lib/grafana/dashboards
        resources:
          requests:
            cpu: "100m"
            memory: "128Mi"
          limits:
            cpu: "500m"
            memory: "512Mi"
      volumes:
      - name: datasources
        configMap:
          name: grafana-datasources
      - name: dashboards-provisioning
        configMap:
          name: grafana-dashboards-provisioning
      - name: dashboards
        configMap:
          name: grafana-dashboards
"""

    k8s_yaml(blob(grafana_yaml))

    k8s_resource(
        "grafana",
        labels=["observability"],
        resource_deps=["prometheus", "loki"],
        objects=[
            "grafana-datasources:configmap",
            "grafana-dashboards-provisioning:configmap",
            "grafana-dashboards:configmap"
        ],
        port_forwards=[format_port_forward(grafana_config["port"], 3000)]
    )

def format_port_forward(local_port, container_port):
    """Format port forward string"""
    return "{}:{}".format(local_port, container_port)

def link(url, text):
    """Create a Tilt UI link"""
    return url
