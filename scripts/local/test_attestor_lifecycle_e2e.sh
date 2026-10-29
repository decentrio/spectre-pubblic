#!/usr/bin/env bash

# Runs the attestor feed-lifecycle E2E suite (TestLiveAttestorFeedLifecycleE2E)
# against an ISOLATED attestor route: a dedicated daemon started by this script
# on its own ports, with its own config and state file in a temporary
# directory, attached read-only to the running devnet's L1, replica, and L2
# endpoints. The shared attestor, its state, the relayer, and every chain are
# left untouched; the temporary directory is removed on exit.
#
# Usage:
#   scripts/local/test_attestor_lifecycle_e2e.sh <op|base|arbitrum> [provisional|finalized|all]
#
# Phases (default: all, run back to back, each with a fresh route):
#   provisional  head=$PROVISIONAL_HEAD (unsafe), default retention cap. Covers
#                derived cadence, WatchAttested semantics, provisional ->
#                confirmed promotion, and game/assertion provenance.
#   finalized    head=finalized, max_derived_roots=$RETENTION_MAX_DERIVED_ROOTS
#                (3). Covers "no provisional entries" and retention/pruning.
#
# Prerequisite: the chosen devnet is up and its handoff exists
# (.op-devnet-run/, .base-devnet-run/, or .arbitrum-devnet-run/attestor.env).
#
# Optional env (defaults in parentheses):
#   PROVISIONAL_HEAD (unsafe)          unsafe | safe
#   LIFECYCLE_DERIVED_GAP_BLOCKS (5)   derived cadence under test (the handoff's
#                                      DERIVED_GAP_BLOCKS is deliberately ignored)
#   RETENTION_MAX_DERIVED_ROOTS (3)    cap for the finalized phase
#   LIFECYCLE_TIMEOUT (30m)            per-phase suite budget
#   LIFECYCLE_DERIVED_ROOTS (4)        distinct derived heights to observe
#   ISOLATED_GRPC_PORT / ISOLATED_METRICS_PORT
#                                      op 3201/3200, base 3211/3210, arbitrum 3221/3220
#   EVIDENCE_DIR (.attestor-e2e-evidence)
#                                      a failed phase's go test output, daemon
#                                      log, metrics, config, and final state are
#                                      copied to EVIDENCE_DIR/<time>-<route>-<phase>.
#                                      KEEP_EVIDENCE=1 keeps them for a passing
#                                      phase too.

set -euo pipefail

cd "$(dirname "$0")/../.."
REPO_ROOT=$PWD

usage() {
    sed -n '3,/^$/p' "$0" | sed -e '/^$/d' -e 's/^# \{0,1\}//'
}

ROUTE=${1:-}
PHASE=${2:-all}
case "$ROUTE" in
    op)
        HANDOFF=$REPO_ROOT/.op-devnet-run/attestor.env
        DEFAULT_GRPC_PORT=3201
        DEFAULT_METRICS_PORT=3200
        ;;
    base)
        HANDOFF=$REPO_ROOT/.base-devnet-run/attestor.env
        DEFAULT_GRPC_PORT=3211
        DEFAULT_METRICS_PORT=3210
        ;;
    arbitrum)
        HANDOFF=$REPO_ROOT/.arbitrum-devnet-run/attestor.env
        DEFAULT_GRPC_PORT=3221
        DEFAULT_METRICS_PORT=3220
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
case "$PHASE" in
    provisional | finalized | all) ;;
    *)
        usage >&2
        exit 2
        ;;
esac

log() { printf '\n[attestor-lifecycle] %s\n' "$*"; }
fail() {
    printf '[attestor-lifecycle] ERROR: %s\n' "$*" >&2
    exit 1
}

[ -f "$HANDOFF" ] || fail "devnet handoff not found: $HANDOFF (start the $ROUTE devnet first)"
command -v jq >/dev/null 2>&1 || fail "jq is required"
command -v curl >/dev/null 2>&1 || fail "curl is required"

# The handoff also exports the shared attestor's ATTESTATION_HEAD, ports and
# run dir; capture what this script needs and ignore the rest.
# shellcheck disable=SC1090
. "$HANDOFF"
if [ -z "${ATTESTOR_PUBLIC_KEY:-}" ]; then
    [ -z "${ATTESTOR_SIGNING_KEY:-}" ] ||
        fail "handoff sets ATTESTOR_SIGNING_KEY without ATTESTOR_PUBLIC_KEY; set ATTESTOR_PUBLIC_KEY explicitly"
    # shellcheck disable=SC1091
    . "$REPO_ROOT/scripts/local/devnet_attestor_identity.sh"
fi
: "${SRC_CHAIN:?SRC_CHAIN missing from $HANDOFF}"
: "${L1_RPC_URL:?L1_RPC_URL missing from $HANDOFF}"
: "${L1_CHAIN_ID:?L1_CHAIN_ID missing from $HANDOFF}"
: "${L2_RPC_URL:?L2_RPC_URL missing from $HANDOFF}"
: "${L2_CHAIN_ID:?L2_CHAIN_ID missing from $HANDOFF}"
: "${ATTESTOR_SIGNING_KEY:?ATTESTOR_SIGNING_KEY missing from $HANDOFF}"
export ATTESTOR_SIGNING_KEY
if [ "$ROUTE" = arbitrum ]; then
    : "${L2_WS_URL:?L2_WS_URL missing from $HANDOFF}"
    : "${ROLLUP_CORE_ADDRESS:?ROLLUP_CORE_ADDRESS missing from $HANDOFF}"
    : "${ROLLUP_DEPLOYMENT_BLOCK:?ROLLUP_DEPLOYMENT_BLOCK missing from $HANDOFF}"
    : "${ASSERTIONS_MAPPING_SLOT:?ASSERTIONS_MAPPING_SLOT missing from $HANDOFF}"
    : "${ASSERTION_STATUS_OFFSET:?ASSERTION_STATUS_OFFSET missing from $HANDOFF}"
else
    : "${OP_NODE_RPC_URL:?OP_NODE_RPC_URL missing from $HANDOFF}"
    : "${DISPUTE_GAME_FACTORY:?DISPUTE_GAME_FACTORY missing from $HANDOFF}"
    : "${RESPECTED_GAME_TYPE:?RESPECTED_GAME_TYPE missing from $HANDOFF}"
fi

# The Arbitrum handoff writes L1 endpoints exactly as `kurtosis port print`
# returns them, a bare host:port; run_arbitrum_attestor.sh adds the scheme in
# container_url. Normalize once here so the daemon config and the Go suite
# both get a URL the daemons accept.
with_scheme() {
    case "$2" in
        '' | http://* | https://* | ws://* | wss://*) printf '%s' "$2" ;;
        *) printf '%s://%s' "$1" "$2" ;;
    esac
}
L1_RPC_URL=$(with_scheme http "$L1_RPC_URL")
L1_WS_URL=$(with_scheme ws "${L1_WS_URL:-}")
L2_RPC_URL=$(with_scheme http "$L2_RPC_URL")
L2_WS_URL=$(with_scheme ws "${L2_WS_URL:-}")

PROVISIONAL_HEAD=${PROVISIONAL_HEAD:-unsafe}
case "$PROVISIONAL_HEAD" in
    unsafe | safe) ;;
    *) fail "PROVISIONAL_HEAD must be unsafe or safe" ;;
esac
# Deliberately not the handoff's DERIVED_GAP_BLOCKS: the suite owns its
# cadence, and the handoff's value for the shared attestor must not change it.
GAP_BLOCKS=${LIFECYCLE_DERIVED_GAP_BLOCKS:-5}
RETENTION_MAX=${RETENTION_MAX_DERIVED_ROOTS:-3}
DEFAULT_MAX_DERIVED=1000
TIMEOUT=${LIFECYCLE_TIMEOUT:-30m}
DERIVED_ROOTS=${LIFECYCLE_DERIVED_ROOTS:-4}
GRPC_PORT=${ISOLATED_GRPC_PORT:-$DEFAULT_GRPC_PORT}
METRICS_PORT=${ISOLATED_METRICS_PORT:-$DEFAULT_METRICS_PORT}
STAMP=$(date +%Y%m%dT%H%M%S)

port_in_use() { (exec 3<>"/dev/tcp/127.0.0.1/$1") 2>/dev/null; }

WORK_DIR=
DAEMON_PID=
stop_daemon() {
    [ -n "$DAEMON_PID" ] || return 0
    kill "$DAEMON_PID" 2>/dev/null || true
    for _ in $(seq 1 40); do
        kill -0 "$DAEMON_PID" 2>/dev/null || break
        sleep 0.5
    done
    kill -9 "$DAEMON_PID" 2>/dev/null || true
    wait "$DAEMON_PID" 2>/dev/null || true
    DAEMON_PID=
}
cleanup() {
    stop_daemon
    if [ -n "$WORK_DIR" ] && [ -d "$WORK_DIR" ]; then
        rm -rf "$WORK_DIR"
    fi
}
trap cleanup EXIT INT TERM

# wait_until <timeout-secs> <description> <command...>: polls every 2s and
# fails fast if the isolated daemon has exited.
wait_until() {
    local timeout=$1 description=$2
    shift 2
    local deadline=$((SECONDS + timeout))
    until "$@" >/dev/null 2>&1; do
        if ! kill -0 "$DAEMON_PID" 2>/dev/null; then
            tail -n 50 "$WORK_DIR/attestor.log" >&2 || true
            fail "isolated attestor exited while waiting for $description"
        fi
        [ "$SECONDS" -lt "$deadline" ] || fail "timed out after ${timeout}s waiting for $description"
        sleep 2
    done
}

build_daemon() {
    log "building the $ROUTE attestor into $WORK_DIR"
    if [ "$ROUTE" = arbitrum ]; then
        (cd attestor/arbitrum && go build -o "$WORK_DIR/attestor" ./cmd)
    else
        (cd attestor/optimism && go build -o "$WORK_DIR/attestor" ./cmd)
    fi
}

write_op_config() {
    local head=$1 max=$2
    jq -n \
        --arg src_chain "$SRC_CHAIN" \
        --arg l1_rpc_url "$L1_RPC_URL" \
        --argjson l1_chain_id "$L1_CHAIN_ID" \
        --arg op_node_rpc_url "$OP_NODE_RPC_URL" \
        --arg dispute_game_factory "$DISPUTE_GAME_FACTORY" \
        --argjson respected_game_type "$RESPECTED_GAME_TYPE" \
        --arg attestation_head "$head" \
        --argjson derived_attestation_gap_blocks "$GAP_BLOCKS" \
        --argjson max_derived_roots "$max" \
        --arg state_path "$STATE_PATH" \
        --argjson l1_bootstrap_lookback_blocks "${LOOKBACK_BLOCKS:-1000}" \
        --argjson l2_chain_id "$L2_CHAIN_ID" \
        --argjson port "$METRICS_PORT" \
        --argjson grpc_port "$GRPC_PORT" \
        '{
            modules: [{
                name: "op_source",
                src_chain: $src_chain,
                config: {
                    l1_rpc_url: $l1_rpc_url,
                    l1_chain_id: $l1_chain_id,
                    op_node_rpc_url: $op_node_rpc_url,
                    dispute_game_factory: $dispute_game_factory,
                    respected_game_type: $respected_game_type,
                    attestation_head: $attestation_head,
                    poll_interval_seconds: 2,
                    derived_attestation_gap_blocks: $derived_attestation_gap_blocks,
                    max_derived_roots: $max_derived_roots,
                    state_path: $state_path,
                    l1_bootstrap_lookback_blocks: $l1_bootstrap_lookback_blocks,
                    l2_chain_id: $l2_chain_id,
                    attestation_signing_key: "env:ATTESTOR_SIGNING_KEY"
                }
            }],
            server: {address: "127.0.0.1", port: $port, grpc_port: $grpc_port}
        }' >"$WORK_DIR/config.json"
}

write_arbitrum_config() {
    local head=$1 max=$2
    local start_block=$ROLLUP_DEPLOYMENT_BLOCK
    [ "$start_block" != 0 ] || start_block=1
    jq -n \
        --arg grpc_listen_address "127.0.0.1:$GRPC_PORT" \
        --arg metrics_listen_address "127.0.0.1:$METRICS_PORT" \
        --arg attestation_head "$head" \
        --argjson derived_attestation_gap_blocks "$GAP_BLOCKS" \
        --argjson max_derived_roots "$max" \
        --arg src_chain "$SRC_CHAIN" \
        --arg l1_rpc_url "$L1_RPC_URL" \
        --argjson l1_chain_id "$L1_CHAIN_ID" \
        --argjson l2_chain_id "$L2_CHAIN_ID" \
        --arg rollup_core_address "$ROLLUP_CORE_ADDRESS" \
        --arg assertions_mapping_slot "$ASSERTIONS_MAPPING_SLOT" \
        --argjson assertion_status_offset "$ASSERTION_STATUS_OFFSET" \
        --argjson assertion_start_block "$start_block" \
        --arg attestor_state_path "$STATE_PATH" \
        --arg nitro_rpc_url "$L2_RPC_URL" \
        --arg nitro_ws_url "$L2_WS_URL" \
        '{
            grpc_listen_address: $grpc_listen_address,
            metrics_listen_address: $metrics_listen_address,
            runtime_poll_interval: "2s",
            attestation_head: $attestation_head,
            derived_attestation_gap_blocks: $derived_attestation_gap_blocks,
            max_derived_roots: $max_derived_roots,
            src_chain: $src_chain,
            l1_rpc_url: $l1_rpc_url,
            l1_chain_id: $l1_chain_id,
            l2_chain_id: $l2_chain_id,
            attestation_signing_key: "env:ATTESTOR_SIGNING_KEY",
            rollup_core_address: $rollup_core_address,
            assertions_mapping_slot: $assertions_mapping_slot,
            assertion_status_offset: $assertion_status_offset,
            assertion_start_block: $assertion_start_block,
            assertion_poll_interval: "2s",
            attestor_state_path: $attestor_state_path,
            nitro_rpc_url: $nitro_rpc_url,
            nitro_ws_url: $nitro_ws_url
        }' >"$WORK_DIR/config.json"
}

preserve_evidence() {
    local phase=$1
    local target=${EVIDENCE_DIR:-$REPO_ROOT/.attestor-e2e-evidence}/$STAMP-$ROUTE-$phase
    mkdir -p "$target"
    # The binary is the only artifact not worth keeping; the state file is
    # copied as inert evidence and is never read by any running attestor.
    find "$WORK_DIR" -maxdepth 1 -type f ! -name attestor -exec cp {} "$target/" \;
    log "evidence preserved in $target"
}

run_phase() {
    local phase=$1 head max
    case "$phase" in
        provisional) head=$PROVISIONAL_HEAD max=$DEFAULT_MAX_DERIVED ;;
        finalized) head=finalized max=$RETENTION_MAX ;;
    esac

    if port_in_use "$GRPC_PORT" || port_in_use "$METRICS_PORT"; then
        fail "port $GRPC_PORT or $METRICS_PORT is already in use; set ISOLATED_GRPC_PORT/ISOLATED_METRICS_PORT"
    fi
    WORK_DIR=$(mktemp -d "${TMPDIR:-/tmp}/attestor-lifecycle-$ROUTE-$phase.XXXXXX")
    WORK_DIR=$(cd "$WORK_DIR" && pwd -P)
    STATE_PATH=$WORK_DIR/attested-roots.json
    build_daemon
    if [ "$ROUTE" = arbitrum ]; then
        write_arbitrum_config "$head" "$max"
    else
        write_op_config "$head" "$max"
    fi

    log "phase $phase: isolated $ROUTE route src=$SRC_CHAIN head=$head gap=$GAP_BLOCKS max_derived_roots=$max grpc=:$GRPC_PORT metrics=:$METRICS_PORT"
    if [ "$ROUTE" = arbitrum ]; then
        "$WORK_DIR/attestor" start --config "$WORK_DIR/config.json" >"$WORK_DIR/attestor.log" 2>&1 &
    else
        "$WORK_DIR/attestor" --config "$WORK_DIR/config.json" >"$WORK_DIR/attestor.log" 2>&1 &
    fi
    DAEMON_PID=$!
    wait_until 120 "metrics on :$METRICS_PORT" curl -sf "http://127.0.0.1:$METRICS_PORT/metrics"
    wait_until 120 "gRPC on :$GRPC_PORT" port_in_use "$GRPC_PORT"

    local metric_prefix=spectre_op_attestor_ watch=supported
    if [ "$ROUTE" = arbitrum ]; then
        metric_prefix=spectre_arbitrum_attestor_
        watch=unsupported
    fi
    local status=0
    (
        cd attestor/grpc
        ATTESTOR_E2E_GRPC_ADDR=127.0.0.1:$GRPC_PORT \
            ATTESTOR_E2E_SRC_CHAIN=$SRC_CHAIN \
            ATTESTOR_E2E_L2_RPC_URL=$L2_RPC_URL \
            ATTESTOR_E2E_L2_CHAIN_ID=$L2_CHAIN_ID \
            ATTESTOR_E2E_PUBLIC_KEY=$ATTESTOR_PUBLIC_KEY \
            ATTESTOR_E2E_METRICS_URL=http://127.0.0.1:$METRICS_PORT/metrics \
            ATTESTOR_E2E_METRIC_PREFIX=$metric_prefix \
            ATTESTOR_E2E_EXPECT_WATCH=$watch \
            ATTESTOR_E2E_LIFECYCLE_FAMILY=$ROUTE \
            ATTESTOR_E2E_LIFECYCLE_HEAD=$head \
            ATTESTOR_E2E_LIFECYCLE_STATE_PATH=$STATE_PATH \
            ATTESTOR_E2E_LIFECYCLE_LOG_PATH=$WORK_DIR/attestor.log \
            ATTESTOR_E2E_LIFECYCLE_DERIVED_GAP_BLOCKS=$GAP_BLOCKS \
            ATTESTOR_E2E_LIFECYCLE_MAX_DERIVED_ROOTS=$max \
            ATTESTOR_E2E_LIFECYCLE_DERIVED_ROOTS=$DERIVED_ROOTS \
            ATTESTOR_E2E_LIFECYCLE_L1_RPC_URL=$L1_RPC_URL \
            ATTESTOR_E2E_LIFECYCLE_OP_NODE_RPC_URL=${OP_NODE_RPC_URL:-} \
            ATTESTOR_E2E_LIFECYCLE_DISPUTE_GAME_FACTORY=${DISPUTE_GAME_FACTORY:-} \
            ATTESTOR_E2E_LIFECYCLE_ROLLUP_CORE=${ROLLUP_CORE_ADDRESS:-} \
            ATTESTOR_E2E_LIFECYCLE_TIMEOUT=$TIMEOUT \
            go test -count=1 -timeout 2h -run '^TestLiveAttestorFeedLifecycleE2E$' -v .
    ) 2>&1 | tee "$WORK_DIR/go-test.log" || status=$?

    curl -sf "http://127.0.0.1:$METRICS_PORT/metrics" >"$WORK_DIR/metrics.txt" 2>/dev/null || true
    [ -f "$STATE_PATH" ] && cp "$STATE_PATH" "$WORK_DIR/final-state.json"
    if ! kill -0 "$DAEMON_PID" 2>/dev/null; then
        log "isolated attestor exited during the suite"
        status=1
    fi
    stop_daemon

    if [ "$status" -ne 0 ] || [ "${KEEP_EVIDENCE:-0}" = 1 ]; then
        preserve_evidence "$phase"
    fi
    rm -rf "$WORK_DIR"
    WORK_DIR=
    [ "$status" -eq 0 ] || fail "phase $phase failed"
    log "phase $phase passed; isolated route state removed"
}

case "$PHASE" in
    all)
        run_phase provisional
        run_phase finalized
        ;;
    *) run_phase "$PHASE" ;;
esac
