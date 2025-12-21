# account-init.Tiltfile - Initialize account public keys on-chain

load("./ports.Tiltfile", "get_port")

def deploy_account_init(config):
    """Deploy job to initialize account public keys"""
    if not config["validator"]["enabled"]:
        return

    # Job to initialize all accounts by sending a tx
    # This registers their public keys on-chain
    init_job = """
apiVersion: batch/v1
kind: Job
metadata:
  name: account-init
  labels:
    app: account-init
spec:
  ttlSecondsAfterFinished: 300
  backoffLimit: 10
  template:
    spec:
      restartPolicy: OnFailure
      securityContext:
        runAsUser: 0
        runAsGroup: 0
        fsGroup: 0
      initContainers:
      # Wait for validator to be ready
      - name: wait-for-validator
        image: curlimages/curl:latest
        command:
        - sh
        - -c
        - |
          echo "Waiting for validator to be ready..."
          until curl -s http://validator:26657/status | grep -q '"latest_block_height":"[1-9]'; do
            echo "Validator not ready yet, waiting..."
            sleep 5
          done
          echo "Validator is ready!"
      containers:
      - name: init-accounts
        image: {}:{}
        command:
        - sh
        - -c
        - |
          set -e
          VALIDATOR_RPC="http://validator:26657"
          PNF_ADDRESS="pokt1eeeksh2tvkh7wzmfrljnhw4wrhs55lcuvmekkw"

          # Create necessary directories (config already exists from volume mount)
          mkdir -p /tmp/pocket/data
          chmod 777 /tmp/pocket/data

          # Import all accounts from all-keys.yaml
          echo "Importing accounts..."
          cd /keys

          # Parse YAML and send tx from each account to initialize pubkey
          # For now, we'll hardcode the accounts that need initialization
          ACCOUNTS="app1 app2 app3 app4 supplier1 supplier2 supplier3 supplier3 supplier4 supplier5 supplier6 supplier7 supplier8 supplier9 supplier10 supplier11 supplier12 supplier13 supplier14 supplier15 gateway1"

          for ACC_NAME in $ACCOUNTS; do
            echo "Initializing $ACC_NAME..."

            # Extract address and mnemonic from all-keys.yaml
            ADDR=$(grep -A 4 "name: $ACC_NAME" all-keys.yaml | grep "address:" | awk '{{print $2}}')
            MNEMONIC=$(grep -A 4 "name: $ACC_NAME" all-keys.yaml | grep "mnemonic:" | cut -d'"' -f2)

            if [ -z "$ADDR" ] || [ -z "$MNEMONIC" ]; then
              echo "Warning: Could not find address or mnemonic for $ACC_NAME"
              continue
            fi

            echo "Address: $ADDR"

            # Import account using mnemonic recovery
            echo "$MNEMONIC" | pocketd keys add "$ACC_NAME" --recover --keyring-backend test --home /tmp/pocket 2>/dev/null || true

            # Verify the key was imported correctly
            IMPORTED_ADDR=$(pocketd keys show "$ACC_NAME" -a --keyring-backend test --home /tmp/pocket 2>/dev/null || echo "")

            if [ "$IMPORTED_ADDR" != "$ADDR" ]; then
              echo "✗ ERROR: Imported address mismatch for $ACC_NAME (got: $IMPORTED_ADDR, expected: $ADDR)"
              continue
            fi

            echo "Checking if $ACC_NAME ($ADDR) already has pubkey..."

            # Check if account already has pubkey (with timeout)
            ACCOUNT_INFO=$(timeout 10 pocketd query auth account "$ADDR" --node "$VALIDATOR_RPC" --chain-id pocket --output json 2>&1 || echo "{{}}")

            # Check for public_key field in the JSON response
            if echo "$ACCOUNT_INFO" | grep -q '"public_key"'; then
              echo "✓ $ACC_NAME already has pubkey registered, skipping..."
              continue
            fi

            echo "Initializing $ACC_NAME pubkey on-chain..."

            # Send transaction (simple pattern that works)
            pocketd tx bank send \
              "$ADDR" \
              "$PNF_ADDRESS" \
              1upokt \
              --from="$ACC_NAME" \
              --gas=1000000 \
              --fees=1upokt \
              --yes \
              --broadcast-mode=sync \
              --home=/tmp/pocket \
              --keyring-backend=test \
              --node="$VALIDATOR_RPC" \
              --chain-id=pocket \
              --output=json 2>&1 | tee /tmp/tx_$ACC_NAME.log

            # Check if tx was successful (JSON format: "code":0)
            if grep -q '"code":0' /tmp/tx_$ACC_NAME.log 2>/dev/null; then
              echo "✓ $ACC_NAME pubkey initialized successfully"

              # Wait for next block and verify pubkey was registered
              sleep 2
              VERIFY_INFO=$(timeout 10 pocketd query auth account "$ADDR" --node "$VALIDATOR_RPC" --chain-id pocket --output json 2>&1 || echo "{{}}")
              if echo "$VERIFY_INFO" | grep -q '"public_key"'; then
                echo "✓ Verified: $ACC_NAME pubkey is now on-chain"
              else
                echo "⚠ Warning: Transaction succeeded but pubkey not yet visible on-chain"
                echo "Account info: $VERIFY_INFO"
              fi
            else
              echo "✗ Failed to initialize $ACC_NAME pubkey:"
              cat /tmp/tx_$ACC_NAME.log
            fi

            sleep 3
          done

          echo "Account initialization complete!"
        volumeMounts:
        - name: all-keys
          mountPath: /keys
          readOnly: true
        - name: pocket-home
          mountPath: /tmp/pocket
        - name: client-config
          mountPath: /tmp/pocket/config/client.toml
          subPath: client.toml
          readOnly: true
      volumes:
      - name: all-keys
        configMap:
          name: all-keys-config
      - name: pocket-home
        emptyDir: {{}}
      - name: client-config
        secret:
          secretName: validator-keys
          items:
          - key: client.toml
            path: client.toml
""".format(
        config["validator"]["image"],
        config["validator"]["tag"]
    )

    k8s_yaml(blob(init_job))

    k8s_resource(
        "account-init",
        labels=["validator"],
        resource_deps=["validator"],
    )
