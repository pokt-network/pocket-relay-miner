#!/usr/bin/env python3
import re

# Read the file
with open('/home/overlordyorch/Development/pocket-relay-miner/tx/tx_client_test.go', 'r') as f:
    content = f.read()

# Pattern to find TxClientConfig without GasLimit
pattern = r'(config := TxClientConfig\{[^}]*?)'
pattern += r'(?!.*?GasLimit:)'  # Negative lookahead - no GasLimit in the block
pattern += r'(ChainID:\s*"test-chain",)\s*\n'
pattern += r'(\s*\})'

# Replacement adds GasLimit before the closing brace
def add_gas_limit(match):
    before = match.group(1)
    chain_id = match.group(2)
    indent = match.group(3)

    # Check if it already has GasLimit (extra safety)
    if 'GasLimit:' in match.group(0):
        return match.group(0)

    return f"{before}{chain_id}\n\t\tGasLimit:     100000,\n{indent}"

# Apply the replacement
new_content = re.sub(pattern, add_gas_limit, content)

# Write back
with open('/home/overlordyorch/Development/pocket-relay-miner/tx/tx_client_test.go', 'w') as f:
    f.write(new_content)

print("Fixed gas limits in test file")