# redis.tilt - Redis deployment using Redis Operator

load("ext://helm_resource", "helm_resource", "helm_repo")
load("./ports.Tiltfile", "get_port")

def deploy_redis(config):
    """Deploy Redis using Redis Operator"""
    if not config["redis"]["enabled"]:
        print("Redis disabled")
        return

    redis_config = config["redis"]
    mode = redis_config["mode"]

    # Install Redis Operator (once)
    install_redis_operator()

    # TODO: we should wait for the operator here

    if mode == "standalone":
        deploy_redis_standalone(redis_config)
    elif mode == "cluster":
        deploy_redis_cluster(redis_config)
    else:
        fail("Invalid redis.mode: {}. Must be 'standalone' or 'cluster'".format(mode))

def install_redis_operator():
    """Install Redis Operator using Helm"""
    print("Installing Redis Operator v0.23.0 via Helm...")

    # Add Helm repo
    helm_repo(
        "ot-helm",
        "https://ot-container-kit.github.io/helm-charts/",
        labels=["znoop"]
    )

    # Install operator via Helm extension
    # v0.23.0 (Jan 2025) - supports Redis >=6 (including Redis 8.x)
    # See: https://github.com/OT-CONTAINER-KIT/redis-operator/releases
    # NOTE: GenerateConfigInInitContainer is needed to load additionalRedisConfig.
    # Issue #1542 affects maxMemoryPercentOfLimit + additionalRedisConfig combo,
    # but we only use additionalRedisConfig, so it should work.
    helm_resource(
        "redis-operator",
        "ot-helm/redis-operator",
        flags=[
            "--version", "0.23.0",  # Pin version for reproducibility
            "--set", "redisOperator.imagePullPolicy=IfNotPresent",
            "--set", "featureGates.GenerateConfigInInitContainer=true",
            "--timeout", "120s",
        ],
        labels=["redis"]
    )

def deploy_redis_standalone(redis_config):
    """Deploy standalone Redis using operator"""
    print("Deploying Redis 8.4 (standalone mode via operator) - SPEED OPTIMIZED...")

    # ConfigMap with Redis config
    # SPEED-OPTIMIZED configuration for pocket-relay-miner:
    # - RDB snapshots every 60s (acceptable 1-2 min data loss from 40 min sessions)
    # - AOF disabled (RDB is sufficient, avoids write amplification)
    # - IO threads enabled for parallel network I/O
    # - Lazyfree enabled for non-blocking deletions
    #
    # NOTE: The operator CRD redisConfig only supports:
    # - additionalRedisConfig (ConfigMap name)
    # - dynamicConfig (array of strings)
    # - maxMemoryPercentOfLimit (integer 1-100)
    # Inline key-value config is NOT supported per the CRD spec.
    # See: https://pkg.go.dev/github.com/OT-CONTAINER-KIT/redis-operator/api/common/v1beta2#RedisConfig
    redis_configmap = """
apiVersion: v1
kind: ConfigMap
metadata:
  name: redis-standalone-config
data:
  redis-additional.conf: |
    # === PERSISTENCE: RDB snapshots (1-2 min acceptable loss) ===
    save 60 1
    appendonly no
    rdbcompression yes
    rdbchecksum no
    # === MEMORY MANAGEMENT ===
    maxmemory 1887436800
    maxmemory-policy allkeys-lru
    # === REDIS 8.x PERFORMANCE OPTIMIZATIONS ===
    io-threads 4
    io-threads-do-reads yes
    lazyfree-lazy-eviction yes
    lazyfree-lazy-expire yes
    lazyfree-lazy-server-del yes
    lazyfree-lazy-user-del yes
    lazyfree-lazy-user-flush yes
    # === NETWORK TUNING ===
    tcp-backlog 511
    timeout 0
    tcp-keepalive 300
    activerehashing yes
    slowlog-log-slower-than -1
"""

    # Redis CR for standalone
    redis_cr = """
apiVersion: redis.redis.opstreelabs.in/v1beta2
kind: Redis
metadata:
  name: redis-standalone
spec:
  kubernetesConfig:
    image: redis:8.4-alpine
    imagePullPolicy: IfNotPresent
    resources:
      requests:
        cpu: 100m
        memory: 256Mi
      limits:
        cpu: 2000m
        memory: 2Gi
    serviceType: ClusterIP
  redisConfig:
    additionalRedisConfig: redis-standalone-config
  storage:
    volumeClaimTemplate:
      spec:
        accessModes: ["ReadWriteOnce"]
        resources:
          requests:
            storage: 1Gi
  redisExporter:
    enabled: true
    image: quay.io/opstree/redis-exporter:v1.44.0
"""

    k8s_yaml(blob(redis_configmap))
    k8s_yaml(blob(redis_cr))

    # Attach to Redis StatefulSet created by operator using k8s_resource with objects
    k8s_resource(
        objects=["redis-operator:namespace", "redis-standalone:redis:default"],
        new_name="redis",
        labels=["redis"],
        port_forwards=["{}:6379".format(get_port("redis"))],
        resource_deps=["redis-operator"]
    )

def deploy_redis_cluster(redis_config):
    """Deploy Redis cluster using operator"""
    cluster_config = redis_config["cluster"]
    leader_count = cluster_config["redisLeader"]
    follower_count = cluster_config["redisFollower"]
    total_nodes = leader_count + follower_count

    print("Deploying Redis 8.4 cluster (via operator with {} leader + {} follower = {} total nodes) - SPEED OPTIMIZED...".format(
        leader_count, follower_count, total_nodes))

    # ConfigMap with Redis cluster config
    # SPEED-OPTIMIZED configuration for pocket-relay-miner:
    # - RDB snapshots every 60s (acceptable 1-2 min data loss from 40 min sessions)
    # - AOF disabled (RDB is sufficient, avoids write amplification)
    # - IO threads enabled for parallel network I/O
    # - Lazyfree enabled for non-blocking deletions
    #
    # NOTE: The operator CRD redisConfig only supports:
    # - additionalRedisConfig (ConfigMap name)
    # - dynamicConfig (array of strings)
    # - maxMemoryPercentOfLimit (integer 1-100)
    # Inline key-value config is NOT supported per the CRD spec.
    # See: https://pkg.go.dev/github.com/OT-CONTAINER-KIT/redis-operator/api/common/v1beta2#RedisConfig
    redis_cluster_configmap = """
apiVersion: v1
kind: ConfigMap
metadata:
  name: redis-cluster-config
data:
  redis-additional.conf: |
    # === PERSISTENCE: RDB snapshots (1-2 min acceptable loss) ===
    save 60 1
    appendonly no
    rdbcompression yes
    rdbchecksum no
    # === MEMORY MANAGEMENT ===
    maxmemory 471859200
    maxmemory-policy allkeys-lru
    # === REDIS 8.x PERFORMANCE OPTIMIZATIONS ===
    io-threads 4
    io-threads-do-reads yes
    lazyfree-lazy-eviction yes
    lazyfree-lazy-expire yes
    lazyfree-lazy-server-del yes
    lazyfree-lazy-user-del yes
    lazyfree-lazy-user-flush yes
    # === NETWORK TUNING ===
    tcp-backlog 511
    timeout 0
    tcp-keepalive 300
    activerehashing yes
    slowlog-log-slower-than -1
    # === CLUSTER SETTINGS ===
    cluster-node-timeout 5000
"""

    # RedisCluster CR
    # IMPORTANT: clusterSize = total nodes (leaders + followers)
    # The operator automatically distributes nodes based on clusterSize
    # For 3 leaders + 3 followers: clusterSize = 6
    redis_cluster_cr = """
apiVersion: redis.redis.opstreelabs.in/v1beta2
kind: RedisCluster
metadata:
  name: redis-cluster
spec:
  clusterSize: {cluster_size}
  clusterVersion: v7
  persistenceEnabled: true
  kubernetesConfig:
    image: redis:8.4-alpine
    imagePullPolicy: IfNotPresent
    resources:
      requests:
        cpu: 100m
        memory: 256Mi
      limits:
        cpu: 1000m
        memory: {max_memory}
  redisLeader:
    redisConfig:
      additionalRedisConfig: redis-cluster-config
  redisFollower:
    redisConfig:
      additionalRedisConfig: redis-cluster-config
  storage:
    volumeClaimTemplate:
      spec:
        accessModes: ["ReadWriteOnce"]
        resources:
          requests:
            storage: 1Gi
  redisExporter:
    enabled: true
    image: quay.io/opstree/redis-exporter:v1.44.0
""".format(
        cluster_size=total_nodes,
        max_memory=redis_config["max_memory"]
    )

    k8s_yaml(blob(redis_cluster_configmap))
    k8s_yaml(blob(redis_cluster_cr))

    # Attach to Redis Cluster created by operator using k8s_resource with objects
    # Forward to one of the leader nodes for cluster-wide access
    k8s_resource(
        objects=["redis-operator:namespace", "redis-cluster:rediscluster:default"],
        new_name="redis-cluster",
        labels=["redis"],
        port_forwards=["{}:6379".format(get_port("redis"))],
        resource_deps=["redis-operator"],
    )
