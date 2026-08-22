# miner.tilt - Miner deployment (HA mode with leader election)

load("./ports.Tiltfile", "get_miner_ports")
load("./utils.Tiltfile", "deep_merge", "read_miner_example_config", "get_redis_host", "apply_k8s_overrides_miner", "config_hash")

def deploy_miners(config):
    """Deploy miner as a Deployment with N replicas"""
    if config["miner"]["count"] == 0:
        print("Miners disabled (count: 0)")
        return

    print("Deploying miner Deployment with {} replica(s)...".format(config["miner"]["count"]))

    # Create miner ConfigMap first, and reuse its rendered YAML so the
    # Deployment can carry the config hash that rolls the pods on a change.
    miner_config_yaml = create_miner_configmap(config)

    # Deploy single miner Deployment with replicas
    deploy_miner_deployment(config, config_hash(miner_config_yaml))

def create_miner_configmap(config):
    """Create ConfigMap with miner configuration. Returns the rendered YAML."""
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

    return miner_config_yaml

def deploy_miner_deployment(config, miner_config_hash):
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
      annotations:
        # See config_hash in utils.Tiltfile: a mounted ConfigMap change does not
        # roll pods by itself, and the miner reads its config only at startup.
        pocket-relay-miner/config-hash: "{}"
    spec:
      initContainers:
      # Build a cosmos keyring from the SAME hex keys the keys_file holds, so the
      # stack can be run against either source without changing what suppliers
      # exist. The "test" backend needs no passphrase and stores to disk; pocketd
      # writes /keyring/keyring-test/<name>.info, which is exactly what a
      # KeyringProvider with backend "test" and dir /keyring reads.
      #
      # No curly braces below: this YAML is rendered through Starlark's
      # .format(), which reads them as placeholders and fails the Tiltfile.
      #
      # An emptyDir, rebuilt on every pod start, on purpose: the keyring is
      # DERIVED from the secret, so there is one source of truth for which keys
      # exist and no second place to update.
      - name: build-keyring
        image: ghcr.io/pokt-network/pocketd:0.1.34
        # As the SAME user the app container runs as, so the keyring files are
        # born owned by it. The first attempt chowned them afterwards instead and
        # failed with "Operation not permitted": this image does not run as root,
        # so it could not hand ownership to anyone. Before that the files were
        # simply unreadable to the app, which loaded ZERO keys.
        securityContext:
          runAsUser: 1000
          runAsGroup: 1000
        env:
        # pocketd writes a client config under $HOME on startup, and as a
        # non-root user in this image $HOME is not writable: it tried /.pocket
        # and failed with "permission denied", which surfaced as a failed key
        # import until the error stopped being swallowed.
        - name: HOME
          value: /tmp
        command:
        - sh
        - -c
        - |
          set -eu
          # Derived data, rebuilt from scratch. An emptyDir survives a container
          # RESTART, and import-hex refuses a name that already exists, so a
          # retry after any failure would die on supplier1 "already exists" --
          # reporting the retry instead of the cause. Measured: that is exactly
          # what a CrashLoopBackOff here looked like.
          # The CONTENTS, not the directory: /keyring is the mount point and
          # removing it fails with "Permission denied". Named explicitly rather
          # than globbed, because these are the only two subdirectories cosmos-sdk
          # creates and a glob would need a shell brace this YAML cannot carry.
          rm -rf /keyring/keyring-file /keyring/keyring-test
          mkdir -p /keyring
          # Read the backend from the RENDERED config rather than taking it as a
          # parameter: the keyring must be built in the same format the process
          # will open, and reading the one file that decides that makes them
          # agree by construction. No keyring block (keys_file mode) means the
          # keyring is still built, in the default format, so switching source
          # later is a config change and nothing else.
          # No braces and no backslashes anywhere in this script. This YAML goes
          # through Starlark's .format() (which reads a brace as a placeholder)
          # inside a triple-quoted Starlark string (where a backslash-n becomes
          # a REAL newline -- that one produced a YAML "unknown directive"
          # because the fragment after it started with a percent sign). Two
          # greps instead of awk or sed for exactly that reason.
          BACKEND=$(grep -oE 'backend: *"?(file|test)' /config/config.yaml | head -1 | grep -oE 'file|test')
          if [ -z "$BACKEND" ]; then BACKEND=test; fi
          # The passphrase, twice, in a file: cosmos-sdk asks once and then a
          # second time to confirm when it is CREATING the keyring (no keyhash
          # file yet), so feeding two lines covers both cases. A file rather
          # than a printf because printf would need a backslash-n.
          PASS=$(cat /keyring-pass/passphrase)
          echo "$PASS" > /tmp/pp
          echo "$PASS" >> /tmp/pp
          i=0
          # The hex keys are one per line in the mounted secret. Selecting them by
          # LENGTH instead of a regex quantifier keeps this free of a yq
          # dependency in the pocketd image and free of curly braces, which the
          # Starlark .format() that renders this YAML reads as placeholders.
          for hex in $(grep -oiE '[0-9a-f]+' /keys/supplier-keys.yaml | awk 'length == 64'); do
            i=$((i+1))
            # The spare second line dies with the process; the test backend
            # ignores stdin entirely. pocketd's own error is PRINTED, not
            # swallowed: discarding it once already hid the real cause behind a
            # generic failure line.
            if ! pocketd keys import-hex "supplier$i" "$hex" \
              --keyring-backend "$BACKEND" --keyring-dir /keyring < /tmp/pp >/dev/null; then
              echo "failed to import supplier$i into the $BACKEND keyring (error above)" >&2
              exit 1
            fi
          done
          # No chown and no chmod: runAsUser above makes these files the app
          # user's, and cosmos-sdk writes them 0600 already. Touching the mount
          # point itself is not permitted to a non-root user anyway.
          echo "imported $i keys into the $BACKEND keyring at /keyring, owned by uid 1000"
        volumeMounts:
        - name: config
          mountPath: /config
        - name: keys
          mountPath: /keys
        - name: keyring
          mountPath: /keyring
        - name: keyring-pass
          mountPath: /keyring-pass
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
        - name: keyring
          mountPath: /keyring
        - name: keyring-pass
          mountPath: /keyring-pass
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
      - name: keyring
        emptyDir: {{}}
      - name: keyring-pass
        secret:
          secretName: keyring-passphrase
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
        miner_config_hash,
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
