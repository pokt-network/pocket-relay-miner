#!/bin/bash
# Test script to verify compression handling in relay miner
# This will help diagnose the Content-Encoding header issue

set -e

echo "=== Testing Compression Handling ==="
echo ""

# Test 1: Request with Accept-Encoding, check if Content-Encoding is returned
echo "Test 1: Request with Accept-Encoding: gzip"
echo "---"
curl -v -H "Accept-Encoding: gzip" \
  -H "Content-Type: application/json" \
  -d '{"jsonrpc":"2.0","method":"eth_blockNumber","params":[],"id":1}' \
  http://localhost:8080 2>&1 | tee /tmp/compression-test.log

echo ""
echo "=== Checking for Content-Encoding header ==="
if grep -i "content-encoding" /tmp/compression-test.log; then
  echo "✓ Content-Encoding header FOUND"
else
  echo "✗ Content-Encoding header MISSING"
fi

echo ""
echo "=== Checking if response is gzipped ==="
# Check for gzip magic bytes in the response
if grep -ao $'\x1f\x8b' /tmp/compression-test.log; then
  echo "✗ Response appears to be gzipped (found magic bytes)"
else
  echo "✓ Response is plain text"
fi

echo ""
echo "=== Test Complete ==="
echo "Check /tmp/compression-test.log for full output"
