# account-init.Tiltfile - Initialize account public keys on-chain
#
# This job waits for block 2, then sends ALL account initialization
# transactions in PARALLEL for fast startup (~5 seconds vs ~60 seconds).

load("./ports.Tiltfile", "get_port")
load("./accounts.star", "staked_account_names")

def deploy_account_init(config, all_keys_path = "tilt/config/all-keys.yaml", genesis_path = "tilt/config/genesis.json"):
    """Deploy job to initialize account public keys"""
    if not config["validator"]["enabled"]:
        return

    accounts = " ".join(staked_account_names(read_yaml(all_keys_path), read_json(genesis_path)))

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
      # Wait for validator to reach block 2
      - name: wait-for-block-2
        image: curlimages/curl:latest
        command:
        - sh
        - -c
        - |
          echo "Waiting for validator to reach block 2..."
          while true; do
            HEIGHT=$(curl -s http://validator:26657/status 2>/dev/null | grep -o '"latest_block_height":"[0-9]*"' | grep -o '[0-9]*' || echo "0")
            if [ "$HEIGHT" -ge 2 ] 2>/dev/null; then
              echo "Validator at block $HEIGHT, proceeding with initialization..."
              break
            fi
            echo "Current block: $HEIGHT, waiting for block 2..."
            sleep 1
          done
      containers:
      - name: init-accounts
        image: {}:{}
        command:
        - sh
        - -c
        - |
          set -e
          VALIDATOR_RPC="http://validator:26657"
          PNF_ADDRESS="pokt12vxfedye7hmamrxq0yvkmsxpxhfmtraaahy90g"

          # Create necessary directories
          mkdir -p /tmp/pocket/data
          chmod 777 /tmp/pocket/data

          cd /keys

          # All accounts that need pubkey initialization
          ACCOUNTS="{}"
          BLOCK_TIME_SECONDS="{}"

          echo "================================================"
          echo "PHASE 1: Importing all accounts to keyring"
          echo "================================================"

          for ACC_NAME in $ACCOUNTS; do
            ADDR=$(grep -A 4 "name: $ACC_NAME$" all-keys.yaml | grep "address:" | awk '{{print $2}}')
            MNEMONIC=$(grep -A 4 "name: $ACC_NAME$" all-keys.yaml | grep "mnemonic:" | cut -d'"' -f2)

            if [ -z "$ADDR" ] || [ -z "$MNEMONIC" ]; then
              echo "⚠ Warning: Could not find address or mnemonic for $ACC_NAME"
              continue
            fi

            # Import account using mnemonic recovery (silent)
            echo "$MNEMONIC" | pocketd keys add "$ACC_NAME" --recover --keyring-backend test --home /tmp/pocket 2>/dev/null || true
            echo "✓ Imported $ACC_NAME ($ADDR)"
          done

          echo ""
          echo "================================================"
          echo "PHASE 2: Sending ALL transactions in PARALLEL"
          echo "================================================"

          # Export variables for subshells
          export VALIDATOR_RPC PNF_ADDRESS

          # Launch ALL transactions in parallel using background jobs
          PIDS=""
          for ACC_NAME in $ACCOUNTS; do
            (
              ADDR=$(grep -A 4 "name: $ACC_NAME$" /keys/all-keys.yaml | grep "address:" | awk '{{print $2}}')

              if [ -z "$ADDR" ]; then
                echo "[$ACC_NAME] ✗ No address found" > /tmp/result_$ACC_NAME.log
                exit 1
              fi

              # Check if already has pubkey
              ACCOUNT_INFO=$(timeout 5 pocketd query auth account "$ADDR" --node "$VALIDATOR_RPC" --chain-id pocket --output json 2>&1 || echo "{{}}")
              if echo "$ACCOUNT_INFO" | grep -q '"public_key"'; then
                echo "[$ACC_NAME] ✓ Already has pubkey" > /tmp/result_$ACC_NAME.log
                exit 0
              fi

              # Send transaction
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
                --output=json 2>&1 > /tmp/tx_$ACC_NAME.log

              if grep -q '"code":0' /tmp/tx_$ACC_NAME.log 2>/dev/null; then
                echo "[$ACC_NAME] ✓ TX submitted" > /tmp/result_$ACC_NAME.log
              else
                echo "[$ACC_NAME] ✗ TX failed" > /tmp/result_$ACC_NAME.log
              fi
            ) &
            PIDS="$PIDS $!"
          done

          # Wait for all background jobs to complete
          echo "Waiting for all transactions to complete..."
          for PID in $PIDS; do
            wait $PID 2>/dev/null || true
          done

          echo ""
          echo "================================================"
          echo "PHASE 3: Transaction Results"
          echo "================================================"
          for ACC_NAME in $ACCOUNTS; do
            if [ -f /tmp/result_$ACC_NAME.log ]; then
              cat /tmp/result_$ACC_NAME.log
            else
              echo "[$ACC_NAME] ? No result"
            fi
          done

          echo ""
          echo "================================================"
          echo "PHASE 4: Verifying pubkeys on-chain"
          echo "================================================"
          # A transaction is included in a later block, and a block takes the
          # validator's block time (localnet.block_time_seconds, which sets its
          # timeout_commit). So the wait is counted in blocks of that clock: check
          # until every account has its pubkey, or until the chain has advanced 5
          # blocks, and give up after 8 block times if the chain stops advancing.
          height() {{
            curl -s http://validator:26657/status 2>/dev/null | grep -o '"latest_block_height":"[0-9]*"' | grep -o '[0-9]*' || echo "0"
          }}
          START_HEIGHT=$(height)
          DEADLINE_HEIGHT=$((START_HEIGHT + 5))
          DEADLINE_SECONDS=$((BLOCK_TIME_SECONDS * 8))
          ELAPSED=0
          while true; do
            SUCCESS=0
            FAILED=0
            MISSING=""
            for ACC_NAME in $ACCOUNTS; do
              ADDR=$(grep -A 4 "name: $ACC_NAME$" all-keys.yaml | grep "address:" | awk '{{print $2}}')
              if [ -z "$ADDR" ]; then
                continue
              fi
              VERIFY_INFO=$(timeout 5 pocketd query auth account "$ADDR" --node "$VALIDATOR_RPC" --chain-id pocket --output json 2>&1 || echo "{{}}")
              if echo "$VERIFY_INFO" | grep -q '"public_key"'; then
                SUCCESS=$((SUCCESS + 1))
              else
                FAILED=$((FAILED + 1))
                MISSING="$MISSING $ACC_NAME"
              fi
            done
            CURRENT_HEIGHT=$(height)
            if [ "$FAILED" -eq 0 ] || [ "$CURRENT_HEIGHT" -ge "$DEADLINE_HEIGHT" ] 2>/dev/null || [ "$ELAPSED" -ge "$DEADLINE_SECONDS" ]; then
              break
            fi
            echo "block $CURRENT_HEIGHT: $FAILED account(s) without a pubkey yet; checking again until block $DEADLINE_HEIGHT (at most $DEADLINE_SECONDS s)"
            sleep 5
            ELAPSED=$((ELAPSED + 5))
          done

          echo ""
          echo "================================================"
          echo "SUMMARY: $SUCCESS successful, $FAILED failed"
          echo "================================================"

          if [ "$FAILED" -gt 0 ]; then
            echo "✗ No pubkey on-chain by block $CURRENT_HEIGHT (started at $START_HEIGHT) for:$MISSING"
            exit 1
          fi

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
        config["validator"]["tag"],
        accounts,
        config["localnet"]["block_time_seconds"],
    )

    k8s_yaml(blob(init_job))

    k8s_resource(
        "account-init",
        labels=["validator"],
        resource_deps=["validator"],
    )
