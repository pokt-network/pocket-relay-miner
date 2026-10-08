# standalone.tilt - Standalone mode: the relayer and the miner in one process,
# their state in an embedded store on a volume, and no Redis.

load("./utils.Tiltfile", "config_hash", "config_check", "keyring_init_containers", "deep_merge")
load("./ports.Tiltfile", "get_port")
load("./miner.Tiltfile", "generate_miner_config")
load("./relayer.Tiltfile", "generate_relayer_config")

# The sections a standalone config holds once, at the top level, for both sides
# (standalone/config.go, commonSections).
COMMON_SECTIONS = ["pocket_node", "keys", "logging", "metrics", "pprof"]

# The process's one metrics and pprof server is the miner side's (the binary
# takes the miner's view of these sections), so those two take the miner's
# values. pocket_node and keys decide which chain and which suppliers: a value
# the two sides disagree on fails here instead of being dropped.
MINER_WINS = ["logging", "metrics", "pprof"]

def deploy_standalone(config):
    """Deploy the standalone process: 1 replica, its store on a volume."""
    print("Deploying standalone (relayer + miner in one process, no Redis)...")
    standalone_config_yaml = str(encode_yaml(generate_standalone_config(config)))
    k8s_yaml(blob("""
apiVersion: v1
kind: ConfigMap
metadata:
  name: standalone-config
data:
  config.yaml: |
    {}
""".format(standalone_config_yaml.replace("\n", "\n    "))))
    check = config_check("standalone-config", "standalone", standalone_config_yaml)
    deploy_standalone_deployment(config, config_hash(standalone_config_yaml), check)

def generate_standalone_config(config):
    """One standalone config from the relayer and miner configs the HA mode
    renders, so a tilt_config.yaml override reaches both modes the same way."""
    relayer_side = generate_relayer_config(config)
    miner_side = generate_miner_config(config)
    if "known_applications" in config.get("miner", {}):
        miner_side["known_applications"] = config["miner"]["known_applications"]

    out = {}
    for section in COMMON_SECTIONS:
        r = relayer_side.pop(section, None)
        m = miner_side.pop(section, None)
        if r == None or m == None:
            value = m if m != None else r
        elif section in MINER_WINS:
            value = deep_merge(r, m)
        else:
            check_no_conflict(section, r, m)
            value = deep_merge(r, m)
        if value != None:
            out[section] = value

    for side in [relayer_side, miner_side]:
        redis = side.get("redis")
        if redis == None:
            continue
        # A standalone config names no Redis server or keyspace, and a pool
        # size sizes nothing; the side's other redis keys still apply.
        for key in ["url", "namespace", "pool_size"]:
            redis.pop(key, None)
        if len(redis) == 0:
            side.pop("redis")

    # On the volume mounted at /data: a pod restart keeps the relays not yet
    # claimed and the trees of sessions not yet proved.
    out["storage"] = {"path": "/data/store"}
    # The read-only inspect server: the live gate and `standalone inspect`
    # read the store through it, as they read Redis in high-availability mode.
    # Loopback in the pod; reached through the port forward below.
    out["inspect"] = {"enabled": True, "addr": "127.0.0.1:{}".format(get_port("standalone_inspect"))}
    out["relayer"] = relayer_side
    out["miner"] = miner_side
    return out

def check_no_conflict(section, r, m, path=""):
    """fail() when the relayer and miner configs set one leaf to two values."""
    for key, rv in r.items():
        if key not in m:
            continue
        mv = m[key]
        where = "{}{}.{}".format(section, path, key)
        if type(rv) == "dict" and type(mv) == "dict":
            check_no_conflict(section, rv, mv, path + "." + key)
        elif rv != mv:
            fail("standalone: {} is {!r} in the relayer config and {!r} in the miner config; ".format(where, rv, mv) +
                 "a standalone config holds it once: make them equal in tilt_config.yaml")

def deploy_standalone_deployment(config, standalone_config_hash, config_check_resource):
    cores = config["relayer"]["cpu_cores"] + config["miner"]["cpu_cores"]
    k8s_yaml(blob("""
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: standalone-store
spec:
  accessModes: ["ReadWriteOnce"]
  resources:
    requests:
      storage: 10Gi
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: standalone
  labels:
    app: standalone
spec:
  # 1, always: the store takes an exclusive lock, and a second process on its
  # own store would serve and claim the same suppliers.
  replicas: 1
  # Recreate, not RollingUpdate: the new pod must not start while the old one
  # still holds the store's lock on the volume.
  strategy:
    type: Recreate
  selector:
    matchLabels:
      app: standalone
  template:
    metadata:
      labels:
        app: standalone
      annotations:
        # See config_hash in utils.Tiltfile: a ConfigMap change does not roll
        # pods by itself, and the process reads its config only at startup.
        pocket-relay-miner/config-hash: "{config_hash}"
    spec:
{keyring_init}
      containers:
      - name: standalone
        image: {image}
        imagePullPolicy: Never
        command:
        - pocket-relay-miner
        - standalone
        - --config=/config/config.yaml
        # A key the binary does not understand stops the pod instead of being
        # warned about and ignored.
        - --strict-config
        ports:
        - containerPort: 8080
          name: relay
        - containerPort: 8081
          name: health
        - containerPort: 9092
          name: metrics
        - containerPort: 6065
          name: pprof
        env:
        - name: LOG_LEVEL
          value: "{log_level}"
        # The relayer's and the miner's GOMEMLIMIT together (7GiB each), under
        # the sum of their limits.
        - name: GOMEMLIMIT
          value: "14GiB"
        - name: POD_NAME
          valueFrom:
            fieldRef:
              fieldPath: metadata.name
        volumeMounts:
        - name: config
          mountPath: /config
        - name: keys
          mountPath: /keys
        - name: keyring
          mountPath: /keyring
        - name: keyring-pass
          mountPath: /keyring-pass
        - name: store
          mountPath: /data
        resources:
          requests:
            cpu: "2500m"
            memory: "1536Mi"
          limits:
            # relayer.cpu_cores + miner.cpu_cores. GOMAXPROCS is NOT set:
            # automaxprocs derives it from this limit.
            cpu: "{cpu_limit}"
            memory: "16Gi"
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
          name: standalone-config
      - name: keys
        secret:
          secretName: supplier-keys
          optional: true
      - name: keyring
        emptyDir: {{}}
      - name: keyring-pass
        secret:
          secretName: keyring-passphrase
      - name: store
        persistentVolumeClaim:
          claimName: standalone-store
---
# The suppliers are staked at http://relayer.default.svc.cluster.local:8080
# (tilt/config/genesis.json), so the relay port keeps the relayer's name.
apiVersion: v1
kind: Service
metadata:
  name: relayer
  labels:
    app: standalone
spec:
  selector:
    app: standalone
  ports:
  - port: 8080
    targetPort: 8080
    name: relay
  - port: 8081
    targetPort: 8081
    name: health
---
apiVersion: v1
kind: Service
metadata:
  name: standalone
  labels:
    app: standalone
spec:
  selector:
    app: standalone
  ports:
  - port: 9092
    targetPort: 9092
    name: metrics
  - port: 6065
    targetPort: 6065
    name: pprof
""".format(
        config_hash=standalone_config_hash,
        image=config["global"]["image"],
        log_level="debug" if config["global"]["debug"] else "info",
        cpu_limit="{}000m".format(cores),
        keyring_init=keyring_init_containers(),
    )))

    k8s_resource(
        "standalone",
        labels=["relay-miner"],
        objects=["standalone-config:configmap", "standalone-store:persistentvolumeclaim", "supplier-keys:secret"],
        resource_deps=["validator", "account-init", config_check_resource],
        port_forwards=[
            # The same local ports high-availability mode uses, so every
            # client, load test and the live gate reach it unchanged.
            "{}:8080".format(config["relayer"]["base_port"]),
            "{}:8081".format(config["relayer"]["health_base_port"]),
            "{}:9092".format(config["miner"]["metrics_base_port"]),
            "{}:6065".format(config["miner"]["pprof_port"]),
            "{0}:{0}".format(get_port("standalone_inspect")),
        ],
    )
