#!/usr/bin/env bash

# Relayer <-> attestor integration E2E (issue #524). Everything this script
# starts is disposable and lives in one temporary directory: attestor daemons
# with fresh keys and state, a relayer config copy and its state dir, and a
# throwaway Cosmos receiver key. The devnet keeps running untouched apart
# from the test transactions the packet part sends.
#
# Usage:
#   scripts/local/test_relayer_attestor_e2e.sh <op|base|arbitrum> [--packet]
#
# Part 1 (always): five attestor processes beside the devnet — three set
# members with distinct keys, an imposter whose key is not in the set, and a
# member gating on the wrong head. Runs TestLiveAttestorQuorumE2E (the
# relayer's real quorum frontier + signed-header builder) and
# TestLiveAttestorStartupValidationE2E (the check `relayer start` performs).
#
# Part 2 (--packet): real L2 -> Cosmos packets through the relayer binary,
# against the devnet's existing wasm client (whose pinned set is the devnet
# attestor identity, threshold 1):
#   P3 invalid quorum   relayer points at an imposter: no packet is relayed
#   P2 transient outage relayer points at a stopped attestor: no progress;
#                       starting the attestor lets the same packet through
#   P1 happy path       a fresh packet is relayed with a signed header
# Requires the full Cosmos <-> L2 devnet from docs/E2E.md (contracts, clients,
# relayer/config.json) and the SHARED RELAYER STOPPED: this run signs with the
# same Cosmos key, and `relayer start` refuses a second process on it. The
# packet part deploys one TestERC20 to the devnet L2 for the transfers.
#
# Optional env (defaults in parentheses):
#   RELAYER_CONFIG (relayer/config.json)  config whose l2_to_cosmos module is copied
#   QUORUM_HEAD (unsafe)                  head of the quorum members
#   BASE_PORT (3400)                      attestors use BASE_PORT.. BASE_PORT+11
#   PACKET_OBSERVE (120s)                 how long "no relay" must hold
#   PACKET_TIMEOUT (900)                  seconds to wait for a relay
#   EVIDENCE_DIR (.attestor-e2e-evidence) failed runs keep logs here;
#                                         KEEP_EVIDENCE=1 keeps passing runs too

set -euo pipefail

cd "$(dirname "$0")/../.."
REPO_ROOT=$PWD

usage() {
    sed -n '3,/^$/p' "$0" | sed -e '/^$/d' -e 's/^# \{0,1\}//'
}

ROUTE=${1:-}
PACKET=0
[ "${2:-}" = --packet ] && PACKET=1
case "$ROUTE" in
    op) HANDOFF=$REPO_ROOT/.op-devnet-run/attestor.env ;;
    base) HANDOFF=$REPO_ROOT/.base-devnet-run/attestor.env ;;
    arbitrum) HANDOFF=$REPO_ROOT/.arbitrum-devnet-run/attestor.env ;;
    -h | --help)
        usage
        exit 0
        ;;
    *)
        usage >&2
        exit 2
        ;;
esac

log() { printf '\n[relayer-attestor-e2e] %s\n' "$*"; }
fail() {
    printf '[relayer-attestor-e2e] ERROR: %s\n' "$*" >&2
    exit 1
}
for tool in jq curl openssl go; do
    command -v "$tool" >/dev/null 2>&1 || fail "$tool is required"
done
[ -f "$HANDOFF" ] || fail "devnet handoff not found: $HANDOFF (start the $ROUTE devnet first)"
# shellcheck disable=SC1090
. "$HANDOFF"
if [ -z "${ATTESTOR_PUBLIC_KEY:-}" ]; then
    # shellcheck disable=SC1091
    . "$REPO_ROOT/scripts/local/devnet_attestor_identity.sh"
fi
DEVNET_SIGNING_KEY=$ATTESTOR_SIGNING_KEY

# The Arbitrum handoff writes L1 endpoints exactly as `kurtosis port print`
# returns them, a bare host:port; run_arbitrum_attestor.sh adds the scheme in
# container_url. The daemons reject a URL without one, so normalize before any
# config, cast/forge call, or Go suite sees it: HTTP for RPC, WS for sockets.
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

QUORUM_HEAD=${QUORUM_HEAD:-unsafe}
WRONG_HEAD=finalized
[ "$QUORUM_HEAD" != finalized ] || WRONG_HEAD=unsafe
BASE_PORT=${BASE_PORT:-3400}
PACKET_OBSERVE=${PACKET_OBSERVE:-120s}
PACKET_TIMEOUT=${PACKET_TIMEOUT:-900}
STAMP=$(date +%Y%m%dT%H%M%S)

WORK_DIR=$(mktemp -d "${TMPDIR:-/tmp}/relayer-attestor-$ROUTE.XXXXXX")
WORK_DIR=$(cd "$WORK_DIR" && pwd -P)
PIDS=()
STATUS=1

cleanup() {
    trap - EXIT INT TERM
    for pid in "${PIDS[@]:-}"; do
        [ -n "$pid" ] && kill "$pid" 2>/dev/null || true
    done
    for pid in "${PIDS[@]:-}"; do
        [ -n "$pid" ] && wait "$pid" 2>/dev/null || true
    done
    if [ "$STATUS" -ne 0 ] || [ "${KEEP_EVIDENCE:-0}" = 1 ]; then
        target=${EVIDENCE_DIR:-$REPO_ROOT/.attestor-e2e-evidence}/$STAMP-$ROUTE-relayer
        mkdir -p "$target"
        # Keep the per-daemon layout: every attestor writes attestor.log,
        # config.json, and attested-roots.json under its own name, and the
        # relayer trackers live under .spectre-state/<identity>/. The receiver
        # keyring and its mnemonic stay out.
        (cd "$WORK_DIR" && find . -type f \( -name '*.log' -o -name '*.json' \) \
            ! -path './gaia-recv/*' ! -name gaia-recv.json -print0 |
            tar -cf - --null -T -) | tar -xf - -C "$target" 2>/dev/null || true
        log "evidence preserved in $target"
    fi
    rm -rf "$WORK_DIR"
}
# Signals only exit; the EXIT trap does the cleanup. A handler that cleaned up
# without exiting would let the script carry on after Ctrl-C and start daemons
# no trap tracks any more.
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

port_in_use() { (exec 3<>"/dev/tcp/127.0.0.1/$1") 2>/dev/null; }
for offset in $(seq 0 11); do
    ! port_in_use $((BASE_PORT + offset)) || fail "port $((BASE_PORT + offset)) is in use; set BASE_PORT"
done

log "building the $ROUTE attestor into $WORK_DIR"
if [ "$ROUTE" = arbitrum ]; then
    (cd attestor/arbitrum && go build -o "$WORK_DIR/attestor" ./cmd)
else
    (cd attestor/optimism && go build -o "$WORK_DIR/attestor" ./cmd)
fi

# start_attestor <name> <seed-hex> <head> <grpc-port> <metrics-port>
# Writes the config into WORK_DIR/<name>/ and records the pid in PIDS and
# WORK_DIR/<name>/pid. The seed is passed through the environment only.
start_attestor() {
    local name=$1 seed=$2 head=$3 grpc=$4 metrics=$5
    local dir=$WORK_DIR/$name
    mkdir -p "$dir"
    if [ "$ROUTE" = arbitrum ]; then
        local start_block=$ROLLUP_DEPLOYMENT_BLOCK
        [ "$start_block" != 0 ] || start_block=1
        jq -n --arg grpc "127.0.0.1:$grpc" --arg metrics "127.0.0.1:$metrics" --arg head "$head" \
            --arg src "$SRC_CHAIN" --arg l1 "$L1_RPC_URL" --argjson l1id "$L1_CHAIN_ID" --argjson l2id "$L2_CHAIN_ID" \
            --arg core "$ROLLUP_CORE_ADDRESS" --arg slot "$ASSERTIONS_MAPPING_SLOT" \
            --argjson offset "$ASSERTION_STATUS_OFFSET" --argjson start "$start_block" \
            --arg state "$dir/attested-roots.json" --arg nitro "$L2_RPC_URL" --arg nitro_ws "$L2_WS_URL" \
            '{grpc_listen_address: $grpc, metrics_listen_address: $metrics, runtime_poll_interval: "2s",
              attestation_head: $head, derived_attestation_gap_blocks: 5, src_chain: $src,
              l1_rpc_url: $l1, l1_chain_id: $l1id, l2_chain_id: $l2id,
              attestation_signing_key: "env:ATTESTOR_SIGNING_KEY", rollup_core_address: $core,
              assertions_mapping_slot: $slot, assertion_status_offset: $offset,
              assertion_start_block: $start, assertion_poll_interval: "2s",
              attestor_state_path: $state, nitro_rpc_url: $nitro, nitro_ws_url: $nitro_ws}' >"$dir/config.json"
        ATTESTOR_SIGNING_KEY=$seed "$WORK_DIR/attestor" start --config "$dir/config.json" >>"$dir/attestor.log" 2>&1 &
    else
        jq -n --arg src "$SRC_CHAIN" --arg l1 "$L1_RPC_URL" --argjson l1id "$L1_CHAIN_ID" \
            --arg opnode "$OP_NODE_RPC_URL" --arg factory "$DISPUTE_GAME_FACTORY" \
            --argjson gametype "$RESPECTED_GAME_TYPE" --arg head "$head" --arg state "$dir/attested-roots.json" \
            --argjson l2id "$L2_CHAIN_ID" --argjson port "$metrics" --argjson grpc "$grpc" \
            '{modules: [{name: "op_source", src_chain: $src, config: {
                l1_rpc_url: $l1, l1_chain_id: $l1id, op_node_rpc_url: $opnode,
                dispute_game_factory: $factory, respected_game_type: $gametype,
                attestation_head: $head, poll_interval_seconds: 2, derived_attestation_gap_blocks: 5,
                state_path: $state, l1_bootstrap_lookback_blocks: 1000, l2_chain_id: $l2id,
                attestation_signing_key: "env:ATTESTOR_SIGNING_KEY"}}],
              server: {address: "127.0.0.1", port: $port, grpc_port: $grpc}}' >"$dir/config.json"
        ATTESTOR_SIGNING_KEY=$seed "$WORK_DIR/attestor" --config "$dir/config.json" >>"$dir/attestor.log" 2>&1 &
    fi
    local pid=$!
    PIDS+=("$pid")
    echo "$pid" >"$dir/pid"
    local deadline=$((SECONDS + 120))
    until curl -sf "http://127.0.0.1:$metrics/metrics" >/dev/null 2>&1 && port_in_use "$grpc"; do
        kill -0 "$pid" 2>/dev/null || { tail -n 30 "$dir/attestor.log" >&2; fail "attestor $name exited during startup"; }
        [ "$SECONDS" -lt "$deadline" ] || fail "attestor $name did not open gRPC :$grpc"
        sleep 2
    done
}

# forget_pid <pid>: drop a process this script already reaped, so cleanup
# never signals a pid the OS may have handed to something else.
forget_pid() {
    local kept=() pid
    for pid in "${PIDS[@]:-}"; do
        [ -z "$pid" ] || [ "$pid" = "$1" ] || kept+=("$pid")
    done
    PIDS=("${kept[@]:-}")
}

stop_attestor() {
    local pid
    pid=$(cat "$WORK_DIR/$1/pid")
    kill "$pid" 2>/dev/null || true
    wait "$pid" 2>/dev/null || true
    forget_pid "$pid"
}

# public_key_b64 <name>: the base64 key the daemon logs at startup.
public_key_b64() {
    sed -n 's/.*base64 for relayer attestors.public_keys): \([A-Za-z0-9+/=]*\).*/\1/p' \
        "$WORK_DIR/$1/attestor.log" | tail -n 1
}

# AttestorConfig orders public keys by their decoded bytes, not their base64
# encoding. The tuple keeps the daemon identity attached while sorting.
key_sort_value() {
    printf '%s' "$1" | openssl base64 -d -A | od -An -v -tx1 | tr -d '[:space:]'
}

member_tuple() {
    local key=$1 seed=$2 endpoint=$3
    printf '%s %s %s %s\n' "$(key_sort_value "$key")" "$key" "$seed" "$endpoint"
}

new_seed() { openssl rand -hex 32; }

# ------------------------------------------------------------------ part 1
port() { echo $((BASE_PORT + $1)); }
SEED0=$(new_seed) SEED1=$(new_seed) SEED2=$(new_seed) SEEDX=$(new_seed)
log "part 1: starting three set members, an imposter, and a wrong-head member (head=$QUORUM_HEAD)"
start_attestor member0 "$SEED0" "$QUORUM_HEAD" "$(port 0)" "$(port 1)"
start_attestor member1 "$SEED1" "$QUORUM_HEAD" "$(port 2)" "$(port 3)"
start_attestor member2 "$SEED2" "$QUORUM_HEAD" "$(port 4)" "$(port 5)"
KEY0=$(public_key_b64 member0)
KEY1=$(public_key_b64 member1)
KEY2=$(public_key_b64 member2)
case "$KEY0,$KEY1,$KEY2" in *,,* | ,* | *,) fail "could not read every member's public key from its log" ;; esac

MEMBER_KEYS=()
MEMBER_SEEDS=()
MEMBER_ENDPOINTS=()
previous_sort_key=
while IFS=' ' read -r sort_key key seed endpoint; do
    [ -n "$sort_key" ] && [ -n "$key" ] && [ -n "$seed" ] && [ -n "$endpoint" ] ||
        fail "could not decode every member public key"
    [ "${#sort_key}" -eq 64 ] || fail "member public key is not an Ed25519 key"
    [ "$sort_key" != "$previous_sort_key" ] || fail "generated duplicate attestor public keys"
    previous_sort_key=$sort_key
    MEMBER_KEYS+=("$key")
    MEMBER_SEEDS+=("$seed")
    MEMBER_ENDPOINTS+=("$endpoint")
done < <(
    {
        member_tuple "$KEY0" "$SEED0" "127.0.0.1:$(port 0)"
        member_tuple "$KEY1" "$SEED1" "127.0.0.1:$(port 2)"
        member_tuple "$KEY2" "$SEED2" "127.0.0.1:$(port 4)"
    } | LC_ALL=C sort -k1,1
)
[ "${#MEMBER_KEYS[@]}" -eq 3 ] || fail "could not sort all three attestor members"
KEYS=$(IFS=,; printf '%s' "${MEMBER_KEYS[*]}")
MEMBERS=$(IFS=,; printf '%s' "${MEMBER_ENDPOINTS[*]}")

start_attestor imposter "$SEEDX" "$QUORUM_HEAD" "$(port 6)" "$(port 7)"
# The wrong-head daemon must use canonical member 2's identity, not the third
# seed generated before the key/endpoint tuples were sorted.
start_attestor wronghead "${MEMBER_SEEDS[2]}" "$WRONG_HEAD" "$(port 8)" "$(port 9)"
[ "$(public_key_b64 wronghead)" = "${MEMBER_KEYS[2]}" ] ||
    fail "wrong-head daemon does not use canonical member 2's public key"

ROUTER=0x0000000000000000000000000000000000000001
if [ -f "$REPO_ROOT/.l2-deployment.json" ]; then
    ROUTER=$(jq -r '.ics26Router // "0x0000000000000000000000000000000000000001"' "$REPO_ROOT/.l2-deployment.json")
fi
export RELAYER_QUORUM_E2E_SRC_CHAIN=$SRC_CHAIN
# The builder proves the router at the attested height, below the head. The
# Base sequencer serves no historical eth_getProof, so read proofs from the
# follower the relayer itself is configured with (deploy_l2_contracts.sh).
export RELAYER_QUORUM_E2E_L2_RPC_URL=$(with_scheme http "${L2_FOLLOWER_RPC_URL:-$L2_RPC_URL}")
export RELAYER_QUORUM_E2E_L2_CHAIN_ID=$L2_CHAIN_ID
export RELAYER_QUORUM_E2E_HEAD=$QUORUM_HEAD
export RELAYER_QUORUM_E2E_ROUTER=$ROUTER
export RELAYER_QUORUM_E2E_PUBLIC_KEYS=$KEYS
export RELAYER_QUORUM_E2E_MEMBERS=$MEMBERS
export RELAYER_QUORUM_E2E_IMPOSTER=127.0.0.1:$(port 6)
export RELAYER_QUORUM_E2E_WRONG_HEAD=127.0.0.1:$(port 8)
# Nothing listens on port 10 (checked free above, never bound).
export RELAYER_QUORUM_E2E_UNREACHABLE=127.0.0.1:$(port 10)

log "running the quorum and startup-validation suites"
(cd relayer && go test -race -count=1 -timeout 30m -v \
    -run '^(TestLiveAttestorQuorumE2E|TestLiveAttestorStartupValidationE2E)$' \
    ./chain/l2rollup/ ./cmd/) 2>&1 | tee "$WORK_DIR/go-test.log" ||
    fail "quorum/startup suites failed"
for name in member0 member1 member2 imposter wronghead; do stop_attestor "$name"; done

if [ "$PACKET" = 0 ]; then
    STATUS=0
    log "part 1 passed (run with --packet for the relayer packet flow)"
    exit 0
fi

# ------------------------------------------------------------------ part 2
for tool in cast forge; do
    command -v "$tool" >/dev/null 2>&1 || fail "$tool is required for --packet"
done
RELAYER_CONFIG=${RELAYER_CONFIG:-$REPO_ROOT/relayer/config.json}
[ -f "$RELAYER_CONFIG" ] || fail "relayer config not found: $RELAYER_CONFIG"
[ -f "$REPO_ROOT/.l2-deployment.json" ] || fail ".l2-deployment.json not found (deploy the L2 contracts first)"
# shellcheck disable=SC1091
set -a && . "$REPO_ROOT/relayer/.env" && set +a
# shellcheck disable=SC1091
. "$REPO_ROOT/scripts/local/gaiad_binary.sh"

L2_MODULE='.modules[] | select(.config.attestor_endpoints != null)'
HEAD_KIND=$(jq -r "$L2_MODULE | .config.head_kind // \"safe\"" "$RELAYER_CONFIG")
L2_CLIENT=$(jq -r "$L2_MODULE | .config.l2_ics26_client_id" "$RELAYER_CONFIG")
TM_RPC=$(jq -r "$L2_MODULE | .config.tm_rpc_url" "$RELAYER_CONFIG" | sed 's#^http#tcp#')
PINNED=$(jq -r "$L2_MODULE | .config.attestors.public_keys | join(\",\")" "$RELAYER_CONFIG")
THRESHOLD=$(jq -r "$L2_MODULE | .config.attestors.threshold" "$RELAYER_CONFIG")
ICS20=$(jq -r '.ics20Transfer' "$REPO_ROOT/.l2-deployment.json")
DEVNET_KEY_B64=$ATTESTOR_PUBLIC_KEY
if [[ "$DEVNET_KEY_B64" =~ ^(0x)?[0-9a-fA-F]{64}$ ]]; then
    DEVNET_KEY_B64=$(printf '%s' "${DEVNET_KEY_B64#0x}" | xxd -r -p | base64)
fi
[ "$PINNED" = "$DEVNET_KEY_B64" ] && [ "$THRESHOLD" = 1 ] ||
    fail "the packet part expects the devnet wasm client set [devnet attestor key], threshold 1; config pins [$PINNED] threshold $THRESHOLD"

log "building the relayer into $WORK_DIR"
(cd relayer && go build -o "$WORK_DIR/relayer" ./cmd)

# relayer_config <endpoint> <file>: a copy of RELAYER_CONFIG whose L2->Cosmos
# module asks only <endpoint>. Living in WORK_DIR also puts the relayer's
# state directory there, so no cursor or tombstone outlives the run.
relayer_config() {
    jq --arg endpoint "$1" \
        '(.modules[] | select(.config.attestor_endpoints != null) | .config.attestor_endpoints) = [{address: $endpoint, attestor_index: 0}]' \
        "$RELAYER_CONFIG" >"$2"
}
# The L2 source logs this once it has seeded its scan cursor: the relayer
# passed startup validation and is polling the L2 router.
RELAYER_READY='[l2->cosmos Subscribe] polling ICS26Router'
RELAYER_PID=
start_relayer() {
    local config=$1 logfile=$2 deadline=$((SECONDS + 120))
    (cd relayer && exec "$WORK_DIR/relayer" start --config "$config") >"$logfile" 2>&1 &
    RELAYER_PID=$!
    PIDS+=("$RELAYER_PID")
    until grep -qF "$RELAYER_READY" "$logfile"; do
        if ! kill -0 "$RELAYER_PID" 2>/dev/null; then
            tail -n 30 "$logfile" >&2
            grep -qi lock "$logfile" && fail "relayer refused to boot on a held signer lock: stop the shared relayer first"
            fail "relayer exited during startup"
        fi
        [ "$SECONDS" -lt "$deadline" ] || { tail -n 30 "$logfile" >&2; fail "relayer did not start polling the L2 within 120s"; }
        sleep 2
    done
}
stop_relayer() {
    [ -n "$RELAYER_PID" ] || return 0
    kill "$RELAYER_PID" 2>/dev/null || true
    wait "$RELAYER_PID" 2>/dev/null || true
    forget_pid "$RELAYER_PID"
    RELAYER_PID=
}

# to_dec <value>: cast --json prints quantities as 0x-hex; accept decimal too.
# Anything else (an empty answer, jq's "null") prints nothing, so the caller
# fails on it instead of reading it as block 0.
to_dec() {
    case "$1" in
        0x[0-9a-fA-F]* | 0X[0-9a-fA-F]*)
            [[ "${1:2}" =~ ^[0-9a-fA-F]+$ ]] && echo $((16#${1:2})) ;;
        *) [[ "$1" =~ ^[0-9]+$ ]] && echo $((10#$1)) ;;
    esac
    return 0
}

# A throwaway Cosmos receiver: its voucher balance is the relay signal.
"$GAIAD" keys add e2e-recv --keyring-backend test --home "$WORK_DIR/gaia-recv" --output json >"$WORK_DIR/gaia-recv.json" 2>&1
RECEIVER=$(jq -r '.address' "$WORK_DIR/gaia-recv.json")
received() {
    "$GAIAD" q bank balances "$RECEIVER" --node "$TM_RPC" --output json |
        jq '[.balances[] | select(.denom | startswith("ibc/")) | .amount | tonumber] | add // 0'
}

SENDER=$(cast wallet address --private-key "$ETH_PRIVATE_KEY")
log "deploying a TestERC20 on the L2 for the transfers"
TOKEN=$(forge create test/mocks/TestERC20.sol:TestERC20 --rpc-url "$L2_RPC_URL" \
    --private-key "$ETH_PRIVATE_KEY" --broadcast 2>&1 | sed -n 's/^Deployed to: //p')
[ -n "$TOKEN" ] || fail "TestERC20 deployment failed"
cast send "$TOKEN" "mint(address,uint256)" "$SENDER" 1000000 --private-key "$ETH_PRIVATE_KEY" --rpc-url "$L2_RPC_URL" >/dev/null
cast send "$TOKEN" "approve(address,uint256)" "$ICS20" 1000000 --private-key "$ETH_PRIVATE_KEY" --rpc-url "$L2_RPC_URL" >/dev/null

send_packet() {
    local timeout=$(($(date +%s) + 3600))
    cast send "$ICS20" "sendTransfer((address,uint256,string,string,string,uint64,string))" \
        "($TOKEN,$1,$RECEIVER,$L2_CLIENT,transfer,$timeout,\"\")" \
        --private-key "$ETH_PRIVATE_KEY" --rpc-url "$L2_RPC_URL" --json | jq -r '.transactionHash'
}

# no_relay_for <duration> <expected> <what>: the receiver balance must stay at
# <expected> for the whole window. The expectation is absolute, not a sample
# taken after the relayer started: a relay inside its startup would otherwise
# become the baseline and pass both this check and the wait that follows.
# A failed balance query is retried, not fatal and not a pass: the window
# closes only on a successful read taken at or after its deadline, and reads
# that keep failing for 60s past it fail the check.
no_relay_for() {
    local seconds=${1%s} expected=$2 deadline balance
    deadline=$((SECONDS + seconds))
    while :; do
        if balance=$(received) && [ -n "$balance" ]; then
            [ "$balance" = "$expected" ] || fail "$3: receiver balance is $balance, want $expected; a packet was relayed although it must not be"
            [ "$SECONDS" -lt "$deadline" ] || return 0
        else
            [ "$SECONDS" -lt $((deadline + 60)) ] || fail "$3: could not read the receiver balance from $TM_RPC"
        fi
        sleep 5
    done
}
wait_balance() {
    local want=$1 deadline=$((SECONDS + PACKET_TIMEOUT))
    until [ "$(received)" -ge "$want" ]; do
        [ "$SECONDS" -lt "$deadline" ] || fail "receiver balance did not reach $want within ${PACKET_TIMEOUT}s"
        sleep 5
    done
}

P_GRPC=$(port 0) P_METRICS=$(port 1) I_GRPC=$(port 6) I_METRICS=$(port 7)

log "P3: relayer asks only an imposter (key not pinned in the wasm client)"
start_attestor packet-imposter "$(new_seed)" "$HEAD_KIND" "$I_GRPC" "$I_METRICS"
relayer_config "127.0.0.1:$I_GRPC" "$WORK_DIR/relayer-imposter.json"
no_relay_for 0 0 "P3 fresh receiver"
start_relayer "$WORK_DIR/relayer-imposter.json" "$WORK_DIR/relayer-p3.log"
TX1=$(send_packet 100)
no_relay_for "$PACKET_OBSERVE" 0 "P3 invalid quorum"
grep -q "attestor signature does not verify" "$WORK_DIR/relayer-p3.log" ||
    fail "P3: relayer log has no signature rejection"
stop_relayer
stop_attestor packet-imposter
log "EVIDENCE P3: packet $TX1 not relayed for $PACKET_OBSERVE; relayer rejected the imposter signature (log: relayer-p3.log)"

log "P2: relayer asks the pinned attestor while it is down, then it starts"
# A fresh relayer finds TX1 only through its startup lookback. Size that
# window from TX1's own block instead of relying on the default, and check
# the scan really starts at or below it.
TX1_BLOCK=$(to_dec "$(cast receipt "$TX1" --json --rpc-url "$L2_RPC_URL" | jq -r '.blockNumber')")
[ -n "$TX1_BLOCK" ] || fail "P2: could not read the block of TX1 $TX1"
TX1_TIME=$(to_dec "$(cast block "$TX1_BLOCK" --json --rpc-url "$L2_RPC_URL" | jq -r '.timestamp')")
[ -n "$TX1_TIME" ] || fail "P2: could not read the timestamp of L2 block $TX1_BLOCK"
export L2_STARTUP_LOOKBACK=$(($(date +%s) - TX1_TIME + 300))s
relayer_config "127.0.0.1:$P_GRPC" "$WORK_DIR/relayer-pinned.json"
no_relay_for 0 0 "P2 before start"
start_relayer "$WORK_DIR/relayer-pinned.json" "$WORK_DIR/relayer-p2.log"
SCAN_FROM=$(grep -F "$RELAYER_READY" "$WORK_DIR/relayer-p2.log" | sed -n 's/.* from block \([0-9]*\) .*/\1/p' | head -n 1)
[ -n "$SCAN_FROM" ] && [ "$SCAN_FROM" -le "$TX1_BLOCK" ] ||
    fail "P2: relayer scans from block ${SCAN_FROM:-?}, after TX1's block $TX1_BLOCK (L2_STARTUP_LOOKBACK=$L2_STARTUP_LOOKBACK)"
no_relay_for "$PACKET_OBSERVE" 0 "P2 attestor down"
start_attestor packet-pinned "$DEVNET_SIGNING_KEY" "$HEAD_KIND" "$P_GRPC" "$P_METRICS"
P2_START=$SECONDS
wait_balance 100
log "EVIDENCE P2: packet $TX1 held while the attestor was down, relayed $((SECONDS - P2_START))s after it started (log: relayer-p2.log)"

log "P1: happy path with the pinned attestor up"
P1_START=$SECONDS
TX2=$(send_packet 200)
wait_balance 300
stop_relayer
log "EVIDENCE P1: packet $TX2 relayed in $((SECONDS - P1_START))s; receiver $RECEIVER holds $(received) vouchers"

STATUS=0
log "relayer-attestor E2E passed; disposable attestors, relayer state, and receiver key removed (TestERC20 $TOKEN stays on the devnet L2)"
