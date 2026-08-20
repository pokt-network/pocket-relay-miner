# Shared helpers for scripts that drive the relayer through the repo's own
# relay CLI. Load and smoke tests go through `relay jsonrpc` (ring-signed
# requests, supplier-signature-verified responses) straight to the relayer —
# never through the PATH gateway, which masks relayer 503s as empty 200s.
#
# Source this file; it defines functions only and runs nothing.

# build_relay_cli — builds the CLI under test into a fresh mktemp dir and
# exports CLI_BIN (binary path) and CLI_BIN_DIR (its directory).
#
# Installs NO trap: bash keeps a single EXIT trap, so a trap set here would
# either be clobbered by the caller's own `trap cleanup EXIT` (leaking the
# ~120MB binary) or clobber it. The caller's cleanup must rm -rf "$CLI_BIN_DIR".
build_relay_cli() {
    local repo_root
    repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
    CLI_BIN_DIR="$(mktemp -d)"
    CLI_BIN="$CLI_BIN_DIR/pocket-relay-miner"
    ( cd "$repo_root" && go build -o "$CLI_BIN" . ) || return 1
    export CLI_BIN CLI_BIN_DIR
}

# wait_relay_ready SERVICE URL [ATTEMPTS] — retries one signed relay (judged
# by CLI exit code, which already implies a verified supplier signature)
# until it succeeds, ATTEMPTS times (default 30) 2s apart. Returns 1 and
# prints the failure if the relayer never serves one.
wait_relay_ready() {
    local service="$1" url="$2" attempts="${3:-30}" i
    for i in $(seq 1 "$attempts"); do
        if "$CLI_BIN" relay jsonrpc --localnet --service "$service" \
            --relayer-url "$url" >/dev/null 2>&1; then
            return 0
        fi
        echo "  waiting for relayer ($i/$attempts)..."
        sleep 2
    done
    echo "ERROR: relayer not serving signed relays after $((attempts * 2))s" >&2
    return 1
}
