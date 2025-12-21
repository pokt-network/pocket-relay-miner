# validator.Tiltfile - Pocket Shannon validator deployment

load("./ports.Tiltfile", "get_port")

def deploy_validator(config):
    """Deploy Pocket Shannon validator"""
    if not config["validator"]["enabled"]:
        print("Validator disabled")
        return

    validator_config = config["validator"]

    # Validator Deployment (based on pnf-ops helm chart pattern)
    validator_yaml = """
apiVersion: v1
kind: Service
metadata:
  name: validator
  labels:
    app: validator
spec:
  ports:
  - port: 26657
    targetPort: 26657
    name: rpc
  - port: 9090
    targetPort: 9090
    name: grpc
  - port: 1317
    targetPort: 1317
    name: rest
  selector:
    app: validator
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: validator
  labels:
    app: validator
spec:
  replicas: 1
  strategy:
    type: Recreate
  selector:
    matchLabels:
      app: validator
  template:
    metadata:
      labels:
        app: validator
    spec:
      securityContext:
        runAsUser: 0
        runAsGroup: 0
        fsGroup: 0
      initContainers:
      # Initialize validator home directory
      - name: init-validator
        image: {}:{}
        command:
        - "/bin/sh"
        - "-c"
        - |
          pocketd init validator --home=/home/pocket/.pocket --chain-id={} > /dev/null 2>&1 && \\
          echo "Validator initialized" || \\
          echo "Validator already initialized"
        volumeMounts:
        - name: validator-data
          mountPath: /home/pocket/.pocket
      # Mount genesis
      - name: copy-genesis
        image: busybox
        command:
        - "/bin/sh"
        - "-c"
        - |
          if [ -f /genesis/genesis.json ]; then
            cp /genesis/genesis.json /home/pocket/.pocket/config/genesis.json
            echo "Genesis loaded from ConfigMap"
          else
            echo "No genesis ConfigMap found, using default"
          fi
        volumeMounts:
        - name: validator-data
          mountPath: /home/pocket/.pocket
        - name: genesis-config
          mountPath: /genesis
      containers:
      - name: validator
        image: {}:{}
        command:
        - pocketd
        - start
        - --home=/home/pocket/.pocket
        - --log_level={}
        ports:
        - containerPort: 26657
          name: rpc
        - containerPort: 9090
          name: grpc
        - containerPort: 1317
          name: rest
        env:
        - name: LOG_LEVEL
          value: "{}"
        volumeMounts:
        - name: validator-data
          mountPath: /home/pocket/.pocket
        - name: validator-keys
          mountPath: /home/pocket/.pocket/config/priv_validator_key.json
          subPath: priv_validator_key.json
          readOnly: true
        - name: validator-keys
          mountPath: /home/pocket/.pocket/config/node_key.json
          subPath: node_key.json
          readOnly: true
        - name: validator-keys
          mountPath: /home/pocket/.pocket/config/app.toml
          subPath: app.toml
          readOnly: true
        - name: validator-keys
          mountPath: /home/pocket/.pocket/config/config.toml
          subPath: config.toml
          readOnly: true
        - name: validator-keys
          mountPath: /home/pocket/.pocket/config/client.toml
          subPath: client.toml
          readOnly: true
        resources:
          requests:
            cpu: "500m"
            memory: "1Gi"
          limits:
            cpu: "2000m"
            memory: "4Gi"
        livenessProbe:
          httpGet:
            path: /health
            port: 26657
          initialDelaySeconds: 30
          periodSeconds: 10
        readinessProbe:
          httpGet:
            path: /health
            port: 26657
          initialDelaySeconds: 10
          periodSeconds: 5
      volumes:
      - name: validator-data
        emptyDir: {{}}
      - name: genesis-config
        configMap:
          name: genesis-config
          optional: true
      - name: validator-keys
        secret:
          secretName: validator-keys
          optional: true
          defaultMode: 0644
""".format(
        validator_config["image"],
        validator_config["tag"],
        validator_config["chain_id"],
        validator_config["image"],
        validator_config["tag"],
        "debug" if config["global"]["debug"] else "info",
        "debug" if config["global"]["debug"] else "info"
    )

    k8s_yaml(blob(validator_yaml))

    k8s_resource(
        "validator",
        labels=["validator"],
        objects=[
            "genesis-config:configmap",
            "all-keys-config:configmap",
            "validator-keys:secret"
        ],
        port_forwards=[
            "{}:26657".format(get_port("validator_rpc")),
            "{}:9090".format(get_port("validator_grpc")),
            "{}:1317".format(get_port("validator_rest")),
        ]
    )
