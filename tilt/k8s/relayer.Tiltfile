# relayer.tilt - Relayer deployment (stateless, waits for miners)

load("./ports.Tiltfile", "get_relayer_ports")
load("./utils.Tiltfile", "deep_merge", "read_relayer_example_config", "get_redis_host", "apply_k8s_overrides_relayer", "config_hash")

def deploy_relayers(config):
    """Deploy relayer as a Deployment with N replicas"""
    if config["relayer"]["count"] == 0:
        print("Relayers disabled (count: 0)")
        return

    print("Deploying relayer Deployment with {} replica(s)...".format(config["relayer"]["count"]))

    # Render the config once: the ConfigMap carries it, and the Deployment
    # carries its hash so a config edit actually rolls the pods.
    relayer_config_yaml = str(encode_yaml(generate_relayer_config(config)))

    # Create relayer ConfigMap first
    create_relayer_configmap(relayer_config_yaml)

    # Deploy single relayer Deployment with replicas
    deploy_relayer_deployment(config, config_hash(relayer_config_yaml))

def create_relayer_configmap(relayer_config_yaml):
    """Create ConfigMap with relayer configuration"""
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

def deploy_relayer_deployment(config, relayer_config_hash):
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
      annotations:
        # Hash of the rendered config. A mounted ConfigMap change does not roll
        # pods on its own, and the relayer reads its config only at startup, so
        # without this a config edit would update the ConfigMap and leave the
        # running relayers on the old one.
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
          rm -rf /keyring
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
          # The app container runs as uid 1000; this one runs as root, so the
          # files it just wrote would be unreadable there. Measured: without
          # this the keyring loads ZERO keys, which the miner reports as a
          # hard startup failure and the relayer used to survive silently.
          chown -R 1000:1000 /keyring
          chmod -R go-rwx /keyring
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
        - name: keyring
          mountPath: /keyring
        - name: keyring-pass
          mountPath: /keyring-pass
        resources:
          requests:
            cpu: "2000m"
            memory: "1Gi"
          limits:
            cpu: "8000m"  # 8 cores - handles relay validation and signing at high RPS
            memory: "4Gi"
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
      - name: keyring
        emptyDir: {{}}
      - name: keyring-pass
        secret:
          secretName: keyring-passphrase
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
        relayer_config_hash,
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
