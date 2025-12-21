# observability.tilt - Prometheus and Grafana deployment

load("./ports.Tiltfile", "get_port")

def deploy_observability(config):
    """Deploy Prometheus and Grafana"""
    if not config["observability"]["enabled"]:
        print("Observability disabled")
        return

    print("Deploying observability stack (Prometheus + Grafana)...")

    deploy_prometheus(config)
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

def deploy_grafana(config):
    """Deploy Grafana for metrics visualization"""
    grafana_config = config["observability"]["grafana"]

    # Load dashboard JSON files (reorganized by concern)
    dashboard_business_economics = str(read_file("tilt/grafana/dashboards/business-economics.json"))
    dashboard_service_performance = str(read_file("tilt/grafana/dashboards/service-performance.json"))
    dashboard_operational_health = str(read_file("tilt/grafana/dashboards/operational-health.json"))

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
  business-economics.json: |
    {}
  service-performance.json: |
    {}
  operational-health.json: |
    {}
""".format(
        dashboard_business_economics.replace("\n", "\n    "),
        dashboard_service_performance.replace("\n", "\n    "),
        dashboard_operational_health.replace("\n", "\n    ")
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
        resource_deps=["prometheus"],
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
