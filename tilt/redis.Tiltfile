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
    print("Installing Redis Operator via Helm...")

    # Add Helm repo
    helm_repo(
        "ot-helm",
        "https://ot-container-kit.github.io/helm-charts/",
        labels=["znoop"]
    )

    # Install operator via Helm extension
    helm_resource(
        "redis-operator",
        "ot-helm/redis-operator",
        flags=["--set", "redisOperator.imagePullPolicy=IfNotPresent"],
        labels=["redis"]
    )

def deploy_redis_standalone(redis_config):
    """Deploy standalone Redis using operator"""
    print("Deploying Redis (standalone mode via operator)...")

    # Redis CR for standalone
    # Performance-tuned persistence settings - see REDIS.md for details
    redis_cr = """
apiVersion: redis.redis.opstreelabs.in/v1beta2
kind: Redis
metadata:
  name: redis-standalone
spec:
  kubernetesConfig:
    image: quay.io/opstree/redis:v7.0.12
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
    # AOF (Append Only File) - Critical for SMST/Streams/Sessions persistence
    appendonly: "yes"
    appendfsync: "everysec"              # Sync every 1s (NOT every write) - balances durability vs performance
    no-appendfsync-on-rewrite: "yes"     # Don't block writes during AOF rewrites
    auto-aof-rewrite-percentage: "100"   # Rewrite when AOF is 2x base size
    auto-aof-rewrite-min-size: "64mb"    # Minimum AOF size to trigger rewrite
    # Disable RDB snapshots (redundant with AOF, adds write amplification)
    save: ""
    # Memory management (88% of 2Gi container limit = ~1.76GB for local dev)
    # Calculated as: 2 * 1024^3 * 0.88 = 1887436800 bytes
    maxmemory: "1887436800"
    maxmemory-policy: "allkeys-lru"      # LRU eviction for local dev (avoid write failures)
    # Performance tuning
    tcp-backlog: "511"
    timeout: "0"
    tcp-keepalive: "300"
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
""".format(redis_config["max_memory"])

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

    print("Deploying Redis cluster (via operator with {} leader + {} follower = {} total nodes)...".format(
        leader_count, follower_count, total_nodes))

    # RedisCluster CR
    # Performance-tuned persistence settings - see REDIS.md for details
    #
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
    image: quay.io/opstree/redis:v7.0.12
    imagePullPolicy: IfNotPresent
    resources:
      requests:
        cpu: 100m
        memory: 256Mi
      limits:
        cpu: 1000m
        memory: {max_memory}
  redisConfig:
    # AOF (Append Only File) - Critical for SMST/Streams/Sessions persistence
    appendonly: "yes"
    appendfsync: "everysec"              # Sync every 1s (NOT every write) - balances durability vs performance
    no-appendfsync-on-rewrite: "yes"     # Don't block writes during AOF rewrites
    auto-aof-rewrite-percentage: "100"   # Rewrite when AOF is 2x base size
    auto-aof-rewrite-min-size: "64mb"    # Minimum AOF size to trigger rewrite
    # Disable RDB snapshots (redundant with AOF, adds write amplification)
    save: ""
    # Memory management (88% of default 512Mi = ~450MB for local dev)
    # Adjust if using different max_memory in tilt_config.yaml
    # Calculated as: 512 * 1024^2 * 0.88 = 471859200 bytes
    maxmemory: "471859200"
    maxmemory-policy: "allkeys-lru"      # LRU eviction for local dev (avoid write failures)
    # Performance tuning
    tcp-backlog: "511"
    timeout: "0"
    tcp-keepalive: "300"
    # Cluster-specific settings
    cluster-enabled: "yes"
    cluster-node-timeout: "5000"
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
  redisLeader:
    replicas: {leader_replicas}
  redisFollower:
    replicas: {follower_replicas}
""".format(
        cluster_size=total_nodes,
        max_memory=redis_config["max_memory"],
        leader_replicas=leader_count,
        follower_replicas=follower_count
    )

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
