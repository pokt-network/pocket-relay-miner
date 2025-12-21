#!/bin/sh
# Account initialization script for Docker Compose
# Registers app public keys on-chain by sending a transaction from each app account.
# This is required because PATH needs to create ring signatures using app pubkeys.
#
# PARALLELIZED: Registers all keys concurrently for faster bootstrap.

NODE="http://validator:26657"
CHAIN_ID="pocket"
PARALLEL_BATCH_SIZE=5  # Number of concurrent transactions

echo "=============================================="
echo "Account Initialization (Parallelized)"
echo "=============================================="

echo "Waiting for validator to be ready..."
until pocketd status --node "$NODE" 2>/dev/null | grep -q "latest_block_height"; do
  echo "  Validator not ready yet, waiting..."
  sleep 3
done

# Get current height
CURRENT_HEIGHT=$(pocketd status --node "$NODE" 2>/dev/null | grep -o '"latest_block_height":"[0-9]*"' | grep -o '[0-9]*' || echo "0")
echo "Validator is ready at height: $CURRENT_HEIGHT"

# Wait until we're past height 2 to ensure chain is stable (reduced from 5)
while [ "$CURRENT_HEIGHT" -lt 2 ]; do
  echo "Waiting for chain to stabilize (height $CURRENT_HEIGHT < 2)..."
  sleep 2
  CURRENT_HEIGHT=$(pocketd status --node "$NODE" 2>/dev/null | grep -o '"latest_block_height":"[0-9]*"' | grep -o '[0-9]*' || echo "0")
done
echo "Chain is stable at height $CURRENT_HEIGHT"

# Function to register a single key
# Args: $1=key_hex $2=unique_name
register_key() {
  local key="$1"
  local name="$2"

  # Import key to keyring
  echo "$key" | pocketd keys import-hex "$name" --keyring-backend test 2>/dev/null || return 0

  # Get address
  local addr=$(pocketd keys show "$name" -a --keyring-backend test 2>/dev/null || echo "")
  if [ -n "$addr" ]; then
    # Send minimal tx to register pubkey (send to self)
    pocketd tx bank send "$addr" "$addr" 1upokt \
      --node "$NODE" \
      --chain-id "$CHAIN_ID" \
      --from "$name" \
      --keyring-backend test \
      --fees 1upokt \
      --yes \
      --broadcast-mode sync >/dev/null 2>&1 && echo "  ✓ Registered: $name ($addr)" || echo "  ✗ Failed: $name"
  fi

  # Remove temp key
  pocketd keys delete "$name" --keyring-backend test --yes 2>/dev/null || true
}

# Gateway private key from path-config.yaml
# This is the gateway address: pokt15vzxjqklzjtlz7lahe8z2dfe9nm5vxwwmscne4
GATEWAY_KEY="cf09805c952fa999e9a63a9f434147b0a5abfd10f268879694c6b5a70e1ae177"

# App private keys from all-keys.yaml (app1-app4)
APP_KEYS="2d00ef074d9b51e46886dc9a1df11e7b986611d0f336bdcf1f0adce3e037ec0a
7e7571a8c61b0887ff8a9017bb4ad83c016b193234f9dc8b6a8ce10c7c483600
7cbbaa043b9b63baa7d6bb087483b0a6a9f82596c19dce4c5028eb43e5b63674
84e4f2257f24d9e1517d414b834bbbfa317e0d53fef21c1528a07a5fa8c70d57"

# Supplier private keys from all-keys.yaml (supplier1-supplier15)
SUPPLIER_KEYS="cd41a98ac7ac3b190c345b88e2885422d7bb553c59bca03c6e2618d49f8b1c06
7b2d353bf74a0aade09bf21928a9e195b54fd1309d19b11e63e16f92efdb610c
3f132ad18b97a7adcbbc45d1741997406c5a2625829f9bdac51736f748857710
1e84c17eec3ae7455a46ec08d0254d9fe0cf7952c82a2ca026e31d67298d2920
f0bdaa5b487138560fe43b187018b0fc8b5ba07a09357612ee47b115e10225db
75533ccabda3431dba1198ef761f157632f13f9752fd32e9265d1b88ce8d4f22
7ea7aa76c8bcbbb1cab93faa92ccec31b2b52a5dce6d038b678dd68c98183006
f39be26fbb70dd43301377e703494f8c10166bd6c088c3074b35495fbd668e8d
00a146ce362b2d193a091d24eb31491d88ed94468b73f18c7529b38a6fcdd429
92daafb0fc7a061cb2cad6d2eeea65700965eab5eb1b8f9ccb19d5594127f6b3
dc4bcddf623d4ccabf49b80a647d5f6a6eb82c64deaa4c6bdfc8d42d81555561
39efcaf10ad3aae093ed64f3145b8bcad48a7fc71c1298f1bf0c6198d0ceb6b4
d2cd3196d580e34ecce3ebec5446cea0013a9cdf50bc91af75066970a142f13c
f5cdf38536b68d23d13da5e241b91841eb29d5eee31e7983b90b0f039264a9cf
9314b707725f5eb82eac29a2fdb819ca8e8fafef5ed8198ae5cac7b1697d159c"

# Count total keys
TOTAL_KEYS=20  # 1 gateway + 4 apps + 15 suppliers
echo ""
echo "Registering $TOTAL_KEYS public keys in parallel (batch size: $PARALLEL_BATCH_SIZE)..."
echo ""

# Register gateway first (critical for PATH)
echo "[1/3] Gateway:"
register_key "$GATEWAY_KEY" "gw-0"

# Register apps in parallel
echo ""
echo "[2/3] Applications (4 keys):"
i=0
for key in $APP_KEYS; do
  register_key "$key" "app-$i" &
  i=$((i + 1))

  # Batch control - wait every PARALLEL_BATCH_SIZE jobs
  if [ $((i % PARALLEL_BATCH_SIZE)) -eq 0 ]; then
    wait
  fi
done
wait  # Wait for remaining

# Register suppliers in parallel
echo ""
echo "[3/3] Suppliers (15 keys):"
i=0
for key in $SUPPLIER_KEYS; do
  register_key "$key" "sup-$i" &
  i=$((i + 1))

  # Batch control - wait every PARALLEL_BATCH_SIZE jobs
  if [ $((i % PARALLEL_BATCH_SIZE)) -eq 0 ]; then
    wait
  fi
done
wait  # Wait for remaining

# Wait for transactions to be committed (at least 2 blocks)
echo ""
echo "Waiting for transactions to be committed..."
TX_HEIGHT=$(pocketd status --node "$NODE" 2>/dev/null | grep -o '"latest_block_height":"[0-9]*"' | grep -o '[0-9]*' || echo "0")
TARGET_HEIGHT=$((TX_HEIGHT + 2))
echo "Current height: $TX_HEIGHT, waiting for height: $TARGET_HEIGHT"

while true; do
  CURRENT_HEIGHT=$(pocketd status --node "$NODE" 2>/dev/null | grep -o '"latest_block_height":"[0-9]*"' | grep -o '[0-9]*' || echo "0")
  if [ "$CURRENT_HEIGHT" -ge "$TARGET_HEIGHT" ]; then
    echo "Reached height $CURRENT_HEIGHT - transactions committed"
    break
  fi
  sleep 1
done

echo ""
echo "=============================================="
echo "Account initialization complete!"
echo "=============================================="
exit 0
