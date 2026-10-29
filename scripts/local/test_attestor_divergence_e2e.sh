#!/usr/bin/env bash

# Runs the attestor divergence drills (TestLiveAttestorDivergenceE2E, #533)
# against ISOLATED routes: disposable daemons on their own ports with their own
# state files in a temporary directory, beside the running devnet. The shared
# attestor and the relayer are never touched; the directory is removed on exit.
#
# Usage:
#   scripts/local/test_attestor_divergence_e2e.sh <op|arbitrum> [invalid-game|correction|all]
#
# Drills:
#   invalid-game (op)  creates a respected-type game whose rootClaim is not the
#                      replica's output root, with the devnet proposer's key,
#                      and asserts the mismatch verdict, roots_mismatched_total
#                      +1, the challenge-hook log line, and that VerifyStateRoot
#                      refuses the claim. The game is PERMANENT on L1: an OP
#                      lifecycle run whose bootstrap lookback (1000 L1 blocks,
#                      ~33 min at 2s slots) covers it will see a mismatch and
#                      fail its L5 roots_mismatched_total == 0 check.
#   correction         (op and arbitrum) a JSON-RPC interposer in front of the
#                      route's replica pins the finalized head and forges every
#                      root above it, then releases; asserts the provisional
#                      root is served and refused for signing, then corrected by
#                      the finalized recheck (CorrectDerived, and on OP also
#                      CorrectProvisional for a real proposer game), with the
#                      divergence metric and HEAD DIVERGENCE log line. The
#                      stock Kurtosis sequencers cannot reorg their unsafe head
#                      on demand, so the divergence is simulated at the replica
#                      RPC; the daemon itself runs unmodified.
#
# Arbitrum only builds blocks on transactions, so the arbitrum drill sends
# zero-value transfers from a throwaway wallet for its duration. The wallet is
# funded once from ARB_TRAFFIC_FUNDER_KEY (default: the devnet owner key that
# run_arbitrum_node.sh uses), never from a relayer key.
#
# Optional env (defaults in parentheses):
#   DIVERGENCE_TIMEOUT (60m)   suite budget
#   DIVERGENCE_PROPOSER_KEY    op proposer key; read from the op-proposer
#                              container when unset
#   ARB_TRAFFIC_FUNDER_KEY     see above
#   ISOLATED_GRPC_PORT / ISOLATED_METRICS_PORT
#                              op 3401/3400, arbitrum 3421/3420; the
#                              correction route uses each port + 10
#   EVIDENCE_DIR (.attestor-e2e-evidence)
#                              a failed run's go test output, daemon logs,
#                              configs, and state files are copied to
#                              EVIDENCE_DIR/<time>-<route>-divergence.
#                              KEEP_EVIDENCE=1 keeps them for a passing run too.

set -euo pipefail

cd "$(dirname "$0")/../.."
REPO_ROOT=$PWD

usage() {
    sed -n '3,/^$/p' "$0" | sed -e '/^$/d' -e 's/^# \{0,1\}//'
}

ROUTE=${1:-}
DRILLS=${2:-all}
case "$ROUTE" in
    op)
        HANDOFF=$REPO_ROOT/.op-devnet-run/attestor.env
        DEFAULT_GRPC_PORT=3401
        DEFAULT_METRICS_PORT=3400
        ;;
    arbitrum)
        HANDOFF=$REPO_ROOT/.arbitrum-devnet-run/attestor.env
        DEFAULT_GRPC_PORT=3421
        DEFAULT_METRICS_PORT=3420
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
case "$ROUTE:$DRILLS" in
    op:invalid-game | *:correction | *:all) ;;
    *)
        usage >&2
        exit 2
        ;;
esac

log() { printf '\n[attestor-divergence] %s\n' "$*"; }
fail() {
    printf '[attestor-divergence] ERROR: %s\n' "$*" >&2
    exit 1
}

[ -f "$HANDOFF" ] || fail "devnet handoff not found: $HANDOFF (start the $ROUTE devnet first)"
command -v cast >/dev/null 2>&1 || fail "cast (Foundry) is required"
command -v jq >/dev/null 2>&1 || fail "jq is required"
# shellcheck disable=SC1090
. "$HANDOFF"
if [ -z "${ATTESTOR_PUBLIC_KEY:-}" ]; then
    [ -z "${ATTESTOR_SIGNING_KEY:-}" ] ||
        fail "handoff sets ATTESTOR_SIGNING_KEY without ATTESTOR_PUBLIC_KEY; set ATTESTOR_PUBLIC_KEY explicitly"
    # shellcheck disable=SC1091
    . "$REPO_ROOT/scripts/local/devnet_attestor_identity.sh"
fi
# Same normalization as the fault suite: the Arbitrum handoff writes bare
# host:port L1 endpoints.
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

PROPOSER_KEY=
if [ "$ROUTE" = op ] && [ "$DRILLS" != correction ]; then
    PROPOSER_KEY=${DIVERGENCE_PROPOSER_KEY:-}
    if [ -z "$PROPOSER_KEY" ]; then
        command -v docker >/dev/null 2>&1 || fail "docker is required to read the op-proposer key (or set DIVERGENCE_PROPOSER_KEY)"
        container=$(docker ps --format '{{.Names}}' | grep -E "^op-proposer-$L2_CHAIN_ID-" | head -1 || true)
        [ -n "$container" ] || fail "no running op-proposer-$L2_CHAIN_ID-* container; set DIVERGENCE_PROPOSER_KEY"
        PROPOSER_KEY=$(docker inspect "$container" --format '{{json .Config.Cmd}}' |
            tr ',' '\n' | sed -n 's/.*--private-key=\(0x\)\{0,1\}\([0-9a-fA-F]\{64\}\).*/\2/p' | head -1)
        [ -n "$PROPOSER_KEY" ] || fail "could not read --private-key from $container"
    fi
    log "creating the invalid game as $(cast wallet address --private-key "$PROPOSER_KEY")"
    log "WARNING: the invalid game stays on L1; an OP lifecycle run within ~33 min will fail its L5 mismatch check"
fi

WORK_DIR=$(mktemp -d "${TMPDIR:-/tmp}/attestor-divergence-$ROUTE.XXXXXX")
WORK_DIR=$(cd "$WORK_DIR" && pwd -P)
TRAFFIC_PID=
cleanup() {
    if [ -n "$TRAFFIC_PID" ]; then
        kill "$TRAFFIC_PID" 2>/dev/null || true
        wait "$TRAFFIC_PID" 2>/dev/null || true
    fi
    rm -rf "$WORK_DIR"
}
trap cleanup EXIT INT TERM

if [ "$ROUTE" = arbitrum ]; then
    FUNDER_KEY=${ARB_TRAFFIC_FUNDER_KEY:-dc04c5399f82306ec4b4d654a342f40e2e0620fe39950d967e1e574b32d4dd36}
    wallet=$(cast wallet new --json)
    TRAFFIC_KEY=$(printf '%s' "$wallet" | jq -r '.[0].private_key')
    TRAFFIC_ADDRESS=$(printf '%s' "$wallet" | jq -r '.[0].address')
    log "funding throwaway traffic wallet $TRAFFIC_ADDRESS on L2"
    cast send "$TRAFFIC_ADDRESS" --value 1ether --private-key "$FUNDER_KEY" --rpc-url "$L2_RPC_URL" >/dev/null
    (
        while :; do
            cast send "$TRAFFIC_ADDRESS" --value 0 --private-key "$TRAFFIC_KEY" --rpc-url "$L2_RPC_URL" \
                >/dev/null 2>>"$WORK_DIR/traffic.log" || sleep 2
            sleep 1
        done
    ) &
    TRAFFIC_PID=$!
fi

log "building the $ROUTE attestor into $WORK_DIR"
if [ "$ROUTE" = arbitrum ]; then
    (cd attestor/arbitrum && go build -o "$WORK_DIR/attestor" ./cmd)
else
    (cd attestor/optimism && go build -o "$WORK_DIR/attestor" ./cmd)
fi

status=0
(
    cd attestor/grpc
    ATTESTOR_DIVERGENCE_E2E_FAMILY=$ROUTE \
        ATTESTOR_DIVERGENCE_E2E_BINARY=$WORK_DIR/attestor \
        ATTESTOR_DIVERGENCE_E2E_WORK_DIR=$WORK_DIR \
        ATTESTOR_DIVERGENCE_E2E_GRPC_PORT=$GRPC_PORT \
        ATTESTOR_DIVERGENCE_E2E_METRICS_PORT=$METRICS_PORT \
        ATTESTOR_DIVERGENCE_E2E_TIMEOUT=${DIVERGENCE_TIMEOUT:-60m} \
        ATTESTOR_DIVERGENCE_E2E_DRILLS=$DRILLS \
        ATTESTOR_DIVERGENCE_E2E_PROPOSER_KEY=$PROPOSER_KEY \
        go test -count=1 -timeout 2h -run '^TestLiveAttestorDivergenceE2E$' -v .
) 2>&1 | tee "$WORK_DIR/go-test.log" || status=$?

if [ "$status" -ne 0 ] || [ "${KEEP_EVIDENCE:-0}" = 1 ]; then
    target=${EVIDENCE_DIR:-$REPO_ROOT/.attestor-e2e-evidence}/$(date +%Y%m%dT%H%M%S)-$ROUTE-divergence
    mkdir -p "$target"
    cp "$WORK_DIR/go-test.log" "$target/"
    [ -f "$WORK_DIR/traffic.log" ] && cp "$WORK_DIR/traffic.log" "$target/"
    for dir in "$WORK_DIR"/*/; do
        [ -d "$dir" ] || continue
        mkdir -p "$target/$(basename "$dir")"
        find "$dir" -maxdepth 1 -type f -exec cp {} "$target/$(basename "$dir")/" \;
    done
    log "evidence preserved in $target"
fi
[ "$status" -eq 0 ] || fail "divergence drills failed"
log "divergence drills passed; isolated routes removed"
