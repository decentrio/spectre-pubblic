#!/usr/bin/env bash

# Runs the attestor fault-recovery E2E suite (TestLiveAttestorFaultRecoveryE2E).
# The suite starts its own disposable attestor daemons beside the running
# devnet. Each daemon reaches L1 RPC, the L1/Nitro WebSocket, and its replica
# only through in-process TCP fault proxies, so an "outage" is this route
# losing its connection: the devnet, the shared attestor, and the relayer are
# never stopped or reconfigured. Proxies are restored and daemons stopped by
# the test; this script removes the temporary directory on exit.
#
# Usage:
#   scripts/local/test_attestor_fault_e2e.sh <op|base|arbitrum>
#
# Covered faults: replica outage, L1 RPC outage, WebSocket loss, SIGTERM and
# SIGKILL restarts on the same state file, and replica lag behind the
# relayer's L2 RPC (a second, finalized-head daemon).
#
# Optional env (defaults in parentheses):
#   FAULT_TIMEOUT (45m)        suite budget
#   FAULT_OUTAGE (20s)         how long a halted feed must stay frozen
#   ISOLATED_GRPC_PORT / ISOLATED_METRICS_PORT
#                              op 3301/3300, base 3311/3310, arbitrum 3321/3320;
#                              the lag route uses each port + 10
#   EVIDENCE_DIR (.attestor-e2e-evidence)
#                              a failed run's go test output, daemon logs,
#                              configs, and state files are copied to
#                              EVIDENCE_DIR/<time>-<route>-fault. KEEP_EVIDENCE=1
#                              keeps them for a passing run too.

set -euo pipefail

cd "$(dirname "$0")/../.."
REPO_ROOT=$PWD

usage() {
    sed -n '3,/^$/p' "$0" | sed -e '/^$/d' -e 's/^# \{0,1\}//'
}

ROUTE=${1:-}
case "$ROUTE" in
    op)
        HANDOFF=$REPO_ROOT/.op-devnet-run/attestor.env
        DEFAULT_GRPC_PORT=3301
        DEFAULT_METRICS_PORT=3300
        ;;
    base)
        HANDOFF=$REPO_ROOT/.base-devnet-run/attestor.env
        DEFAULT_GRPC_PORT=3311
        DEFAULT_METRICS_PORT=3310
        ;;
    arbitrum)
        HANDOFF=$REPO_ROOT/.arbitrum-devnet-run/attestor.env
        DEFAULT_GRPC_PORT=3321
        DEFAULT_METRICS_PORT=3320
        ;;
    -h | --help)
        usage
        exit 0
        ;;
    *)
        usage >&2
        exit 2
        ;;
esac

log() { printf '\n[attestor-fault] %s\n' "$*"; }
fail() {
    printf '[attestor-fault] ERROR: %s\n' "$*" >&2
    exit 1
}

[ -f "$HANDOFF" ] || fail "devnet handoff not found: $HANDOFF (start the $ROUTE devnet first)"
# shellcheck disable=SC1090
. "$HANDOFF"
if [ -z "${ATTESTOR_PUBLIC_KEY:-}" ]; then
    [ -z "${ATTESTOR_SIGNING_KEY:-}" ] ||
        fail "handoff sets ATTESTOR_SIGNING_KEY without ATTESTOR_PUBLIC_KEY; set ATTESTOR_PUBLIC_KEY explicitly"
    # shellcheck disable=SC1091
    . "$REPO_ROOT/scripts/local/devnet_attestor_identity.sh"
fi
# The Arbitrum handoff writes L1 endpoints exactly as `kurtosis port print`
# returns them, a bare host:port; run_arbitrum_attestor.sh adds the scheme in
# container_url. The suite parses every endpoint as a URL to put a fault proxy
# in front of it, so normalize before exporting: HTTP for RPC, WS for sockets.
with_scheme() {
    case "$2" in
        '' | http://* | https://* | ws://* | wss://*) printf '%s' "$2" ;;
        *) printf '%s://%s' "$1" "$2" ;;
    esac
}
L1_RPC_URL=$(with_scheme http "${L1_RPC_URL:-}")
L2_RPC_URL=$(with_scheme http "${L2_RPC_URL:-}")
OP_NODE_RPC_URL=$(with_scheme http "${OP_NODE_RPC_URL:-}")
L1_WS_URL=$(with_scheme ws "${L1_WS_URL:-}")
L2_WS_URL=$(with_scheme ws "${L2_WS_URL:-}")

# The suite reads the handoff variables by their handoff names.
export SRC_CHAIN L1_RPC_URL L1_CHAIN_ID L2_RPC_URL L2_CHAIN_ID ATTESTOR_SIGNING_KEY ATTESTOR_PUBLIC_KEY
export L1_WS_URL L2_WS_URL OP_NODE_RPC_URL
export DISPUTE_GAME_FACTORY=${DISPUTE_GAME_FACTORY:-} RESPECTED_GAME_TYPE=${RESPECTED_GAME_TYPE:-}
export ROLLUP_CORE_ADDRESS=${ROLLUP_CORE_ADDRESS:-} ROLLUP_DEPLOYMENT_BLOCK=${ROLLUP_DEPLOYMENT_BLOCK:-}
export ASSERTIONS_MAPPING_SLOT=${ASSERTIONS_MAPPING_SLOT:-} ASSERTION_STATUS_OFFSET=${ASSERTION_STATUS_OFFSET:-}

GRPC_PORT=${ISOLATED_GRPC_PORT:-$DEFAULT_GRPC_PORT}
METRICS_PORT=${ISOLATED_METRICS_PORT:-$DEFAULT_METRICS_PORT}
port_in_use() { (exec 3<>"/dev/tcp/127.0.0.1/$1") 2>/dev/null; }
for port in "$GRPC_PORT" "$METRICS_PORT" $((GRPC_PORT + 10)) $((METRICS_PORT + 10)); do
    ! port_in_use "$port" || fail "port $port is in use; set ISOLATED_GRPC_PORT/ISOLATED_METRICS_PORT"
done

WORK_DIR=$(mktemp -d "${TMPDIR:-/tmp}/attestor-fault-$ROUTE.XXXXXX")
WORK_DIR=$(cd "$WORK_DIR" && pwd -P)
trap 'rm -rf "$WORK_DIR"' EXIT INT TERM

log "building the $ROUTE attestor into $WORK_DIR"
if [ "$ROUTE" = arbitrum ]; then
    (cd attestor/arbitrum && go build -o "$WORK_DIR/attestor" ./cmd)
else
    (cd attestor/optimism && go build -o "$WORK_DIR/attestor" ./cmd)
fi

status=0
(
    cd attestor/grpc
    ATTESTOR_FAULT_E2E_FAMILY=$ROUTE \
        ATTESTOR_FAULT_E2E_BINARY=$WORK_DIR/attestor \
        ATTESTOR_FAULT_E2E_WORK_DIR=$WORK_DIR \
        ATTESTOR_FAULT_E2E_GRPC_PORT=$GRPC_PORT \
        ATTESTOR_FAULT_E2E_METRICS_PORT=$METRICS_PORT \
        ATTESTOR_FAULT_E2E_TIMEOUT=${FAULT_TIMEOUT:-45m} \
        ATTESTOR_FAULT_E2E_OUTAGE=${FAULT_OUTAGE:-20s} \
        go test -count=1 -timeout 2h -run '^TestLiveAttestorFaultRecoveryE2E$' -v .
) 2>&1 | tee "$WORK_DIR/go-test.log" || status=$?

if [ "$status" -ne 0 ] || [ "${KEEP_EVIDENCE:-0}" = 1 ]; then
    target=${EVIDENCE_DIR:-$REPO_ROOT/.attestor-e2e-evidence}/$(date +%Y%m%dT%H%M%S)-$ROUTE-fault
    mkdir -p "$target"
    cp "$WORK_DIR/go-test.log" "$target/"
    for dir in "$WORK_DIR"/*/; do
        [ -d "$dir" ] || continue
        mkdir -p "$target/$(basename "$dir")"
        find "$dir" -maxdepth 1 -type f -exec cp {} "$target/$(basename "$dir")/" \;
    done
    log "evidence preserved in $target"
fi
[ "$status" -eq 0 ] || fail "fault-recovery suite failed"
log "fault-recovery suite passed; disposable routes removed"
