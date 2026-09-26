#!/bin/bash
# Starts the single-validator localnet chain.
#
# The chain state lives in the validator-data volume. The first start
# initialises it; every start then copies the files mounted under /seed over
# the generated ones, so the files in this directory always win.
set -euo pipefail

HOME_DIR=/home/pocket/.pocket

if [ ! -f "$HOME_DIR/config/genesis.json" ]; then
  echo "first start: initialising $HOME_DIR"
  pocketd init validator --home="$HOME_DIR" --chain-id=pocket >/dev/null 2>&1
  # Only on the first start: overwriting the signing state of a chain that has
  # already advanced would make the validator sign heights it already signed.
  cp /seed/priv_validator_state.json "$HOME_DIR/data/priv_validator_state.json"
fi

for f in genesis.json priv_validator_key.json node_key.json app.toml config.toml client.toml; do
  cp "/seed/$f" "$HOME_DIR/config/$f"
done

exec pocketd start --home="$HOME_DIR" --log_level=info
