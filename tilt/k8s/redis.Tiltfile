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
    #
    # The line that used to sit here said issue #1542 (maxMemoryPercentOfLimit +
    # additionalRedisConfig) did not affect us "because we only use
    # additionalRedisConfig". That is FALSE, measured on the live pod 2026-09-18:
    # the ConfigMap carries maxmemory 9663676416 (9 GiB), the pod DOES read it at
    # /etc/redis/external.conf.d/redis-additional.conf:17, and the server runs
    # with 8589934592 -- exactly 80% of the 10Gi container limit, which is the
    # operator applying its own percentage on top. So the 1 GiB-per-step ladder
    # in tilt_config.yaml collapses: the ingestion brake closes at 8 GiB and
    # Redis refuses writes at 8 GiB, the same point, with no margin between them.
    # Queue item 363 carries the evidence and the options; do not "fix" it here
    # without reading it, because raising memory_limit and pinning the percentage
    # move the same number in opposite directions.
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
    # - RDB snapshots at 900s/1 change and 300s/10 changes
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
    # === PERSISTENCE: RDB snapshots ===
    # The operator's base redis.conf already declares save 900 1 / 300 10 /
    # 60 10000, and save lines ACCUMULATE across its include of this file
    # (config.c setConfigSaveOption), so "save 60 1" here ran four rules
    # and forked every ~76 s under load. "" clears the inherited list only;
    # the next line keeps snapshots on.
    save ""
    save 900 1 300 10
    appendonly no
    rdbcompression yes
    rdbchecksum no
    # === MEMORY MANAGEMENT ===
    # Comes from the config (gitignored) because a fixed cap with allkeys-lru
    # SILENTLY EVICTS: all state lives here (SMST nodes, meter, tracking), so
    # an eviction looks like lost claims, not like "Redis is full". Set 0 to
    # measure how much memory Redis ASKS FOR instead of how much we grant it.
    maxmemory {maxmemory}
    maxmemory-policy {maxmemory_policy}
    # === REDIS 8.x PERFORMANCE OPTIMIZATIONS ===
    # Counts the main thread: 4 = main + 3 I/O threads. io-threads-do-reads
    # is deprecated and ignored since 8.x, so it is not set.
    io-threads 4
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
    # -1 turns off the slowlog: `SLOWLOG GET` returns empty and reads as "no
    # slow commands happened" without ever having looked. Configurable so
    # Redis can be named as the bottleneck instead of inferred from its
    # caller's timeout.
    slowlog-log-slower-than {slowlog_us}
    # 128 entries were overwritten within seconds under load, which lost the
    # slow commands of the window being measured.
    slowlog-max-len 10000
    # 0 disables LATENCY LATEST/HISTORY; 10 ms records the stalls that
    # clients see as a slow GET without enabling it by hand after a restart.
    latency-monitor-threshold 10
""".format(
        maxmemory=redis_config.get("maxmemory", "1887436800"),
        maxmemory_policy=redis_config.get("maxmemory_policy", "noeviction"),
        slowlog_us=redis_config.get("slowlog_log_slower_than_us", "-1"),
    )

    # Redis CR for standalone
    redis_cr = """
apiVersion: redis.redis.opstreelabs.in/v1beta2
kind: Redis
metadata:
  name: redis-standalone
spec:
  kubernetesConfig:
    image: redis:8.10.1-alpine
    imagePullPolicy: IfNotPresent
    resources:
      requests:
        # Equal to the limit: under host contention a 100m request let
        # Redis wait for CPU while every client waited on Redis.
        cpu: {cpu_limit}
        memory: 256Mi
      limits:
        cpu: {cpu_limit}
        memory: {memory_limit}
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
""".format(
        cpu_limit=redis_config.get("cpu_limit", "2000m"),
        memory_limit=redis_config.get("memory_limit", "2Gi"),
    )

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
    # - RDB snapshots at 900s/1 change and 300s/10 changes
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
    # === PERSISTENCE: RDB snapshots ===
    # The operator's base redis.conf (read on the standalone pod) declares save 900 1 / 300 10 /
    # 60 10000, and save lines ACCUMULATE across its include of this file
    # (config.c setConfigSaveOption), so "save 60 1" here ran four rules
    # and forked every ~76 s under load. "" clears the inherited list only;
    # the next line keeps snapshots on.
    save ""
    save 900 1 300 10
    appendonly no
    rdbcompression yes
    rdbchecksum no
    # === MEMORY MANAGEMENT ===
    maxmemory 471859200
    # noeviction and NOT allkeys-lru: the SMST nodes are the proof preimage, so
    # evicting them looks like lost claims instead of "Redis is full".
    maxmemory-policy noeviction
    # === REDIS 8.x PERFORMANCE OPTIMIZATIONS ===
    # Counts the main thread: 4 = main + 3 I/O threads. io-threads-do-reads
    # is deprecated and ignored since 8.x, so it is not set.
    io-threads 4
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
    image: redis:8.10.1-alpine
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
