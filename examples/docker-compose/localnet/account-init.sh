#!/bin/bash
# One-shot: puts the public key of every staked localnet account on chain.
#
# The genesis stakes the applications, suppliers and gateway, but an account
# that never signed a transaction has no public key on chain, and the relayer
# needs the application's and gateway's public keys to verify a relay's ring
# signature. So each account sends 1upokt to the PNF account once.
#
# Idempotent: an account that already has a public key is skipped. Exits 1 if
# any account still has no public key at the end, so the services that depend
# on this one do not start against a chain that cannot verify relays.
set -uo pipefail

NODE=tcp://validator:26657
CHAIN_ID=pocket
PNF_ADDRESS=pokt1eeeksh2tvkh7wzmfrljnhw4wrhs55lcuvmekkw
ACCOUNTS_FILE=/localnet/accounts.yaml
KEYRING_HOME=/tmp/pocket
export HOME=/tmp

height() {
  pocketd status --node "$NODE" 2>/dev/null |
    grep -o '"latest_block_height":"[0-9]*"' | grep -o '[0-9]*' || echo 0
}

has_pubkey() {
  pocketd query auth account "$1" --node "$NODE" --output json 2>/dev/null |
    grep -q 'PubKey'
}

echo "waiting for block 2..."
for _ in $(seq 1 120); do
  [ "$(height)" -ge 2 ] && break
  sleep 1
done
if [ "$(height)" -lt 2 ]; then
  echo "validator did not reach block 2 in 120s" >&2
  exit 1
fi

# name address private_key, one account per line.
mapfile -t ROWS < <(awk '
  /- name:/      { name = $3 }
  /address:/     { addr = $2 }
  /private_key:/ { print name, addr, $2 }
' "$ACCOUNTS_FILE")
echo "${#ROWS[@]} accounts in $ACCOUNTS_FILE"
if [ "${#ROWS[@]}" -eq 0 ]; then
  echo "no accounts parsed from $ACCOUNTS_FILE" >&2
  exit 1
fi

rm -rf "$KEYRING_HOME"
for row in "${ROWS[@]}"; do
  read -r name addr key <<<"$row"
  if has_pubkey "$addr"; then
    echo "$name: already has a public key"
    continue
  fi
  if ! pocketd keys import-hex "$name" "$key" --keyring-backend test --home "$KEYRING_HOME" >/dev/null; then
    echo "$name: key import failed" >&2
    continue
  fi
  out=$(pocketd tx bank send "$name" "$PNF_ADDRESS" 1upokt \
    --fees 1upokt --gas 1000000 --yes --broadcast-mode sync \
    --keyring-backend test --home "$KEYRING_HOME" \
    --node "$NODE" --chain-id "$CHAIN_ID" --output json 2>&1)
  if echo "$out" | grep -q '"code":0'; then
    echo "$name: transaction sent"
  else
    echo "$name: transaction failed: $out" >&2
  fi
done

# Verify, allowing a few blocks for the transactions to be included.
missing=()
for _ in $(seq 1 12); do
  missing=()
  for row in "${ROWS[@]}"; do
    read -r name addr _ <<<"$row"
    has_pubkey "$addr" || missing+=("$name")
  done
  [ "${#missing[@]}" -eq 0 ] && break
  sleep 5
done

if [ "${#missing[@]}" -ne 0 ]; then
  echo "accounts without a public key: ${missing[*]}" >&2
  exit 1
fi
echo "${#ROWS[@]} accounts verified with a public key on chain"
