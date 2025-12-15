#!/bin/bash
#
# Watch for claim submissions in real-time
#

SUPPLIER_ADDR="pokt19a3t4yunp0dlpfjrp7qwnzwlrzd5fzs2gjaaaj"
RELAYER_BIN="./bin/pocket-relay-miner"

GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m'

clear
echo -e "${BLUE}========================================${NC}"
echo -e "${BLUE}  Watching for Claim Submissions${NC}"
echo -e "${BLUE}========================================${NC}"
echo ""

while true; do
    # Clear screen and reposition cursor
    tput cup 5 0

    echo -e "${YELLOW}$(date '+%Y-%m-%d %H:%M:%S')${NC}"
    echo ""

    # Show sessions by state
    echo -e "${BLUE}=== Sessions by State ===${NC}"
    for state in active claiming claimed proving settled; do
        count=$($RELAYER_BIN redis-debug sessions --supplier $SUPPLIER_ADDR --state $state 2>/dev/null | grep -c "^[a-f0-9]\{64\}" || echo "0")
        echo -e "${GREEN}$state:${NC} $count"
    done
    echo ""

    # Show recent claimed sessions
    echo -e "${BLUE}=== Recently Claimed (last 5) ===${NC}"
    $RELAYER_BIN redis-debug sessions --supplier $SUPPLIER_ADDR --state claimed 2>/dev/null | head -7 || echo "No claimed sessions yet"
    echo ""

    # Show Redis Streams stats
    echo -e "${BLUE}=== Redis Streams ===${NC}"
    $RELAYER_BIN redis-debug streams --supplier $SUPPLIER_ADDR 2>/dev/null | head -5 || echo "No stream data"
    echo ""

    # Show all sessions summary
    echo -e "${BLUE}=== All Sessions (top 10) ===${NC}"
    $RELAYER_BIN redis-debug sessions --supplier $SUPPLIER_ADDR 2>/dev/null | head -12

    # Wait before refresh
    sleep 3
done
