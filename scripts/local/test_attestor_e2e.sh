#!/usr/bin/env bash

# Runs the non-destructive live attestor contract suite after a local sidecar
# is up. The suite validates the daemon against its L2 RPC, including the
# Ed25519 verdict a relayer accepts, rather than repeating mocked unit cases.
#
# Usage:
#   scripts/local/test_attestor_e2e.sh op
#   scripts/local/test_attestor_e2e.sh base
#   scripts/local/test_attestor_e2e.sh arbitrum
#
# Optional overrides are deliberately the test's ATTESTOR_E2E_* variables, so
# this also works against a remote loopback tunnel or a nonstandard local port.

set -euo pipefail

cd "$(dirname "$0")/../.."
REPO_ROOT=$PWD

usage() {
    cat <<'EOF'
Usage: scripts/local/test_attestor_e2e.sh <op|base|arbitrum>

Start the chosen stack and its attestor first. The script sources that stack's
attestor.env and runs attestor/grpc's opt-in TestLiveAttestorE2E. It only reads
the attestor, L2 RPC, and metrics endpoint; it neither restarts nor mutates a
daemon, state file, replica, or relayer.
EOF
}

case "${1:-}" in
    op)
        HANDOFF=$REPO_ROOT/.op-devnet-run/attestor.env
        DEFAULT_GRPC_PORT=3001
        DEFAULT_METRICS_PORT=3000
        DEFAULT_METRIC_PREFIX=spectre_op_attestor_
        DEFAULT_WATCH=supported
        ;;
    base)
        HANDOFF=$REPO_ROOT/.base-devnet-run/attestor.env
        DEFAULT_GRPC_PORT=3003
        DEFAULT_METRICS_PORT=3000
        DEFAULT_METRIC_PREFIX=spectre_op_attestor_
        DEFAULT_WATCH=supported
        ;;
    arbitrum)
        HANDOFF=$REPO_ROOT/.arbitrum-devnet-run/attestor.env
        DEFAULT_GRPC_PORT=3002
        DEFAULT_METRICS_PORT=9090
        DEFAULT_METRIC_PREFIX=spectre_arbitrum_attestor_
        DEFAULT_WATCH=unsupported
        ;;
    -h|--help)
        usage
        exit 0
        ;;
    *)
        usage >&2
        exit 2
        ;;
esac

[ -f "$HANDOFF" ] || {
    printf 'ERROR: attestor handoff not found: %s\nStart the selected local stack first.\n' "$HANDOFF" >&2
    exit 1
}

# shellcheck disable=SC1090
. "$HANDOFF"

# Older Arbitrum handoffs predate ATTESTOR_PUBLIC_KEY. The local identity is
# public test data and is exactly what run_arbitrum_node.sh uses when no key was
# supplied by the caller; do not overwrite either handoff or E2E override data.
if [ -z "${ATTESTOR_PUBLIC_KEY:-}" ] && [ -z "${ATTESTOR_E2E_PUBLIC_KEY:-}" ]; then
    if [ -n "${ATTESTOR_SIGNING_KEY:-}" ]; then
        printf 'ERROR: ATTESTOR_SIGNING_KEY is set but the handoff has no ATTESTOR_PUBLIC_KEY. Set ATTESTOR_E2E_PUBLIC_KEY explicitly.\n' >&2
        exit 1
    fi
    # shellcheck disable=SC1091
    . "$REPO_ROOT/scripts/local/devnet_attestor_identity.sh"
fi

# The Arbitrum handoff writes a bare host:port while the gRPC suite passes the
# endpoint to net/http. Keep an explicit E2E override untouched, but give the
# handoff value the HTTP scheme that getL2Header requires.
with_scheme() {
    case "$2" in
        '' | http://* | https://* | ws://* | wss://*) printf '%s' "$2" ;;
        *) printf '%s://%s' "$1" "$2" ;;
    esac
}
L2_RPC_URL=$(with_scheme http "${L2_RPC_URL:-}")

GRPC_PORT=${GRPC_PORT:-$DEFAULT_GRPC_PORT}
METRICS_PORT=${METRICS_PORT:-$DEFAULT_METRICS_PORT}

if [ -z "${ATTESTOR_E2E_L2_ROUTER:-}" ] && [ -f "$REPO_ROOT/.l2-deployment.json" ] && command -v jq >/dev/null 2>&1; then
    candidate_router=$(jq -r '.ics26Router // empty' "$REPO_ROOT/.l2-deployment.json")
    if [ -n "$candidate_router" ]; then
        ATTESTOR_E2E_L2_ROUTER=$candidate_router
    fi
fi

export ATTESTOR_E2E_GRPC_ADDR=${ATTESTOR_E2E_GRPC_ADDR:-127.0.0.1:$GRPC_PORT}
export ATTESTOR_E2E_SRC_CHAIN=${ATTESTOR_E2E_SRC_CHAIN:-${SRC_CHAIN:?SRC_CHAIN is missing from $HANDOFF}}
export ATTESTOR_E2E_L2_RPC_URL=${ATTESTOR_E2E_L2_RPC_URL:-${L2_RPC_URL:?L2_RPC_URL is missing from $HANDOFF}}
export ATTESTOR_E2E_L2_CHAIN_ID=${ATTESTOR_E2E_L2_CHAIN_ID:-${L2_CHAIN_ID:?L2_CHAIN_ID is missing from $HANDOFF}}
export ATTESTOR_E2E_PUBLIC_KEY=${ATTESTOR_E2E_PUBLIC_KEY:-${ATTESTOR_PUBLIC_KEY:?ATTESTOR_PUBLIC_KEY is required}}
export ATTESTOR_E2E_L2_ROUTER=${ATTESTOR_E2E_L2_ROUTER:-0x0000000000000000000000000000000000000001}
export ATTESTOR_E2E_METRICS_URL=${ATTESTOR_E2E_METRICS_URL:-http://127.0.0.1:$METRICS_PORT/metrics}
export ATTESTOR_E2E_METRIC_PREFIX=${ATTESTOR_E2E_METRIC_PREFIX:-$DEFAULT_METRIC_PREFIX}
export ATTESTOR_E2E_EXPECT_WATCH=${ATTESTOR_E2E_EXPECT_WATCH:-$DEFAULT_WATCH}
export ATTESTOR_E2E_TIMEOUT=${ATTESTOR_E2E_TIMEOUT:-2m}

exec bash -c 'cd attestor/grpc && go test -count=1 -run "^TestLiveAttestorE2E$" -v'
