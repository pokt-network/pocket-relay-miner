#!/bin/bash
# Add GasLimit: 100000 to all TxClientConfig that don't have it

# Find all TxClientConfig declarations without Gas Limit and add it
sed -i '/config := TxClientConfig{/,/}/ {
  /GasLimit:/b
  /}/ {
    /ChainID.*test-chain/{
      s/}/\tGasLimit:      100000,\n\t}/
    }
  }
}' /home/overlordyorch/Development/pocket-relay-miner/tx/tx_client_test.go