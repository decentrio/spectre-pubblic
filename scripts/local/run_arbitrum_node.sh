#!/usr/bin/env bash

# Local Arbitrum Nitro/BoLD devnet on a shared Ethereum L1 in a Kurtosis enclave.
#
# The L1 is shared, not owned: if ENCLAVE already exists (typically because
# run_optimism_node.sh brought up Ethereum + OP in it) this attaches to that L1 so
# both rollups settle to the same chain. If it does not exist, this brings the L1 up
# itself via run_eth_node.sh, so an Arbitrum-only devnet does not require the OP
# stack. Either way exactly one L1 is created.
#
# Bring-up order:
#   1. (optional) scripts/local/run_optimism_node.sh starts Ethereum + OP in ENCLAVE,
#      to share one L1 between both rollups.
#   2. This script runs the local Arbitrum Kurtosis package in that enclave — creating
#      the L1 first if needed. The package deploys RollupCore and starts a simple Nitro
#      sequencer/batch-poster/staker; it never creates another L1.
#   3. scripts/local/run_arbitrum_attestor.sh starts an independent,
#      non-sequencing Nitro replica and the attestor outside the enclave.
#
# The package publishes the sequencer RPC/WS/feed ports through Kurtosis and
# stores chain-info/config artifacts. This wrapper validates them, waits for a
# finalized BoLD assertion, and writes RUN_DIR/attestor.env.

set -euo pipefail

cd "$(dirname "$0")/../.."
REPO_ROOT=$PWD
# shellcheck disable=SC1091
. "$REPO_ROOT/scripts/local/devnet_attestor_identity.sh"

ENCLAVE=${ENCLAVE:-op-devnet}
RUN_DIR=${RUN_DIR:-$REPO_ROOT/.arbitrum-devnet-run}
PACKAGE_DIR=${PACKAGE_DIR:-$REPO_ROOT/scripts/local/kurtosis/arbitrum}
STARTUP_WAIT_SECS=${STARTUP_WAIT_SECS:-900}
FINALITY_WAIT_SECS=${FINALITY_WAIT_SECS:-900}
ASSERTION_WAIT_SECS=${ASSERTION_WAIT_SECS:-900}
L2_CHAIN_ID=${L2_CHAIN_ID:-412346}
NITRO_IMAGE=${NITRO_IMAGE:-offchainlabs/nitro-node:v3.11.2-3599aca}
NITRO_CONTRACTS_REF=${NITRO_CONTRACTS_REF:-v3.1.0}
ASSERTIONS_MAPPING_SLOT=${ASSERTIONS_MAPPING_SLOT:-0x0000000000000000000000000000000000000000000000000000000000000075}
ASSERTION_STATUS_OFFSET=${ASSERTION_STATUS_OFFSET:-25}

# Public development keys. The funder is the prefunded account already used by
# optimism-package's external-L1 setup; the remaining keys match Nitro's
# standard local test accounts. Never use these keys outside local devnets.
L1_FUNDER_PRIVATE_KEY=${L1_FUNDER_PRIVATE_KEY:-eaba42282ad33c8ef2524f07277c03a776d98ae19f581990ce75becb7cfa1c23}
ARB_OWNER_PRIVATE_KEY=${ARB_OWNER_PRIVATE_KEY:-dc04c5399f82306ec4b4d654a342f40e2e0620fe39950d967e1e574b32d4dd36}
ARB_OWNER_ADDRESS=${ARB_OWNER_ADDRESS:-0x5E1497dD1f08C87b2d8FE23e9AAB6c1De833D927}
ARB_SEQUENCER_PRIVATE_KEY=${ARB_SEQUENCER_PRIVATE_KEY:-cb5790da63720727af975f42c79f69918580209889225fa7128c92402a6d3a65}
ARB_SEQUENCER_ADDRESS=${ARB_SEQUENCER_ADDRESS:-0xe2148eE53c0755215Df69b2616E552154EdC584f}
ARB_VALIDATOR_PRIVATE_KEY=${ARB_VALIDATOR_PRIVATE_KEY:-182fecf15bdf909556a0f617a63e05ab22f1493d25a9f1e27c228266c772a890}
ARB_VALIDATOR_ADDRESS=${ARB_VALIDATOR_ADDRESS:-0x6A568afe0f82d34759347bb36F14A6bB171d2CBe}
L2_DEPLOYER_PRIVATE_KEY=${L2_DEPLOYER_PRIVATE_KEY:-bcdf20249abf0ed6d944c0288fad489e33f66b3960d9e6229c1cd214ed3bbe31}
L2_DEPLOYER_ADDRESS=${L2_DEPLOYER_ADDRESS:-0x8943545177806ED17B9F23F0a21ee5948eCaa776}

ACTION=up
RESET=false

usage() {
    cat <<'EOF'
Usage: scripts/local/run_arbitrum_node.sh [--reset|--stop|--status]

Adds Arbitrum Nitro to a Kurtosis enclave on a shared Ethereum L1. Attaches to the
L1 already in ENCLAVE (e.g. one run_optimism_node.sh created, so both rollups settle
to the same chain), or brings that L1 up itself when the enclave does not exist.

Options:
  --reset   Redeploy only the Arbitrum services/contracts on the existing L1.
            The shared Ethereum and OP services are preserved.
  --stop    Stop the Arbitrum sequencer/deployer services, preserving the L1.
  --status  Show the Arbitrum services in the shared enclave.
  -h        Show this help.

Environment:
  ENCLAVE                  Existing OP/Kurtosis enclave (default: op-devnet).
  RUN_DIR                  Runtime artifacts (default: .arbitrum-devnet-run).
  PACKAGE_DIR              Local external-L1 Arbitrum Kurtosis package.
  L2_CHAIN_ID              Arbitrum devnet chain ID (default: 412346).
  ATTESTOR_SIGNING_KEY / ATTESTOR_PUBLIC_KEY
                           Matching Ed25519 identity for the local attestor. Set
                           both to override the disposable test identity.
  NITRO_IMAGE              Nitro image shared with the attestor replica.
  NITRO_CONTRACTS_REF      BoLD nitro-contracts ref (default: v3.1.0).
  STARTUP_WAIT_SECS        L2 startup timeout (default: 900).
  FINALITY_WAIT_SECS       Shared-L1 finality timeout (default: 900).
  ASSERTION_WAIT_SECS      Finalized AssertionCreated timeout (default: 900).
  ASSERTIONS_MAPPING_SLOT  BoLD _assertions mapping slot (default: 0x75 for
                           the pinned nitro-contracts v3.1.0).
  ASSERTION_STATUS_OFFSET  AssertionNode status byte offset (default: 25).

The development keys may be overridden with L1_FUNDER_PRIVATE_KEY,
ARB_OWNER_{PRIVATE_KEY,ADDRESS}, ARB_SEQUENCER_{PRIVATE_KEY,ADDRESS}, and
ARB_VALIDATOR_{PRIVATE_KEY,ADDRESS}. L2_DEPLOYER_{PRIVATE_KEY,ADDRESS}
controls the account funded for scripts/local/deploy_l2_contracts.sh. The
key/address pairs must match and the funder must already have ETH on the
shared L1.
EOF
}

fail() {
    printf '[run_arbitrum_node] ERROR: %s\n' "$*" >&2
    exit 1
}

log() {
    printf '\n[run_arbitrum_node] %s\n' "$*"
}

while [ "$#" -gt 0 ]; do
    case "$1" in
        --reset)
            [ "$ACTION" = up ] || fail "--reset cannot be combined with --stop or --status"
            RESET=true
            ;;
        --stop)
            [ "$ACTION" = up ] || fail "--stop cannot be combined with another action"
            [ "$RESET" = false ] || fail "--reset cannot be combined with --stop"
            ACTION=stop
            ;;
        --status)
            [ "$ACTION" = up ] || fail "--status cannot be combined with another action"
            [ "$RESET" = false ] || fail "--reset cannot be combined with --status"
            ACTION=status
            ;;
        -h|--help)
            usage
            exit 0
            ;;
        *)
            usage >&2
            fail "unknown argument: $1"
            ;;
    esac
    shift
done

require_command() {
    command -v "$1" >/dev/null 2>&1 || fail "$1 is required"
}

require_command kurtosis
require_command curl
require_command jq
require_command cast

[ -f "$PACKAGE_DIR/kurtosis.yml" ] ||
    fail "Arbitrum Kurtosis package not found at $PACKAGE_DIR"

enclave_inspect() {
    kurtosis enclave inspect "$ENCLAVE"
}

service_exists() {
    enclave_inspect 2>/dev/null | grep -qE "(^|[[:space:]])$1([[:space:]]|$)"
}

# The L1 is shared: when the enclave already exists (typically because
# run_optimism_node.sh brought up Ethereum + OP in it) this attaches to that L1 and
# never creates a second one. When it does not exist, bring the L1 up here so an
# Arbitrum-only devnet does not require running the OP stack first — same recipe,
# via the same run_eth_node.sh the OP flow reuses.
if ! enclave_inspect >/dev/null 2>&1; then
    case "$ACTION" in
        status|stop)
            fail "Kurtosis enclave '$ENCLAVE' does not exist; nothing to $ACTION"
            ;;
    esac
    log "enclave '$ENCLAVE' does not exist; bringing up the shared L1 via run_eth_node.sh"
    ENCLAVE="$ENCLAVE" RUN_DIR="$RUN_DIR" L1_PARAMS=eth-network-params.yaml \
        SKIP_COSMOS_RESET=1 sh scripts/local/run_eth_node.sh
    enclave_inspect >/dev/null 2>&1 ||
        fail "L1 bring-up did not produce enclave '$ENCLAVE'"
fi

case "$ACTION" in
    status)
        enclave_inspect | grep -E 'arb-(rollup-deployer|sequencer)' || {
            log "no Arbitrum services found in enclave $ENCLAVE"
            exit 1
        }
        exit 0
        ;;
    stop)
        found=false
        for service in arb-sequencer arb-rollup-deployer; do
            if service_exists "$service"; then
                log "stopping $service in enclave $ENCLAVE"
                kurtosis service stop "$ENCLAVE" "$service"
                found=true
            fi
        done
        [ "$found" = true ] || fail "no Arbitrum services found in enclave $ENCLAVE"
        exit 0
        ;;
esac

mkdir -p "$RUN_DIR"
RUN_DIR=$(cd "$RUN_DIR" && pwd)

if [ "$RESET" = true ]; then
    log "resetting only the Arbitrum services; shared L1 and OP services are preserved"
    for service in arb-sequencer arb-rollup-deployer; do
        if service_exists "$service"; then
            kurtosis service rm "$ENCLAVE" "$service"
        fi
    done
    rm -f "$RUN_DIR/attestor.env"
fi

# Discover the shared L1 created by run_optimism_node.sh. Internal URLs are passed
# to the package; published URLs are written to the external attestor handoff.
L1_SVC=$(enclave_inspect | grep -oE 'el-1-[a-zA-Z0-9-]+' | head -1)
CL_SVC=$(enclave_inspect | grep -oE 'cl-1-[a-zA-Z0-9-]+' | head -1)
[ -n "$L1_SVC" ] || fail "no ethereum-package execution service found in enclave $ENCLAVE"
[ -n "$CL_SVC" ] || fail "no ethereum-package beacon service found in enclave $ENCLAVE"

L1_RPC_URL=$(kurtosis port print "$ENCLAVE" "$L1_SVC" rpc)
L1_WS_URL=$(kurtosis port print "$ENCLAVE" "$L1_SVC" ws)
L1_BEACON_API_URL=$(kurtosis port print "$ENCLAVE" "$CL_SVC" http)
INTERNAL_L1_RPC_URL="http://$L1_SVC:8545"
INTERNAL_L1_BEACON_URL="http://$CL_SVC:4000"

rpc_result() {
    local url=$1
    local method=$2
    local params=${3:-'[]'}

    curl --fail --silent --show-error \
        --header 'Content-Type: application/json' \
        --data "{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"$method\",\"params\":$params}" \
        "$url" |
        jq -er 'if .error then error(.error.message) else .result end'
}

hex_to_decimal() {
    local value=$1
    [[ "$value" =~ ^0x[0-9a-fA-F]+$ ]] ||
        fail "invalid hexadecimal integer returned by JSON-RPC: $value"
    printf '%d\n' "$((value))"
}

wait_until() {
    local timeout=$1
    local description=$2
    shift 2
    local deadline=$((SECONDS + timeout))

    until "$@" >/dev/null 2>&1; do
        if [ "$SECONDS" -ge "$deadline" ]; then
            log "TIMEOUT after ${timeout}s waiting for: $description"
            return 1
        fi
        sleep 5
    done
}

l1_ready() {
    rpc_result "$L1_RPC_URL" eth_chainId
}

beacon_ready() {
    curl --fail --silent "$L1_BEACON_API_URL/eth/v1/node/version" |
        jq -e '.data.version | length > 0'
}

l1_finalized() {
    local block
    block=$(rpc_result "$L1_RPC_URL" eth_getBlockByNumber '["finalized",false]') ||
        return 1
    [ "$(printf '%s' "$block" | jq -r '.number // "0x0"')" != 0x0 ]
}

log "checking the shared Ethereum L1 in enclave $ENCLAVE"
wait_until "$STARTUP_WAIT_SECS" "shared L1 execution RPC" l1_ready
wait_until "$STARTUP_WAIT_SECS" "shared L1 beacon API" beacon_ready
wait_until "$FINALITY_WAIT_SECS" "non-genesis finalized shared-L1 block" l1_finalized
L1_CHAIN_ID=$(hex_to_decimal "$(rpc_result "$L1_RPC_URL" eth_chainId)")

address_for_key() {
    cast wallet address --private-key "$1"
}

[ "$(address_for_key "$ARB_OWNER_PRIVATE_KEY")" = "$ARB_OWNER_ADDRESS" ] ||
    fail "ARB_OWNER_PRIVATE_KEY does not match ARB_OWNER_ADDRESS"
[ "$(address_for_key "$ARB_SEQUENCER_PRIVATE_KEY")" = "$ARB_SEQUENCER_ADDRESS" ] ||
    fail "ARB_SEQUENCER_PRIVATE_KEY does not match ARB_SEQUENCER_ADDRESS"
[ "$(address_for_key "$ARB_VALIDATOR_PRIVATE_KEY")" = "$ARB_VALIDATOR_ADDRESS" ] ||
    fail "ARB_VALIDATOR_PRIVATE_KEY does not match ARB_VALIDATOR_ADDRESS"
[ "$(address_for_key "$L2_DEPLOYER_PRIVATE_KEY")" = "$L2_DEPLOYER_ADDRESS" ] ||
    fail "L2_DEPLOYER_PRIVATE_KEY does not match L2_DEPLOYER_ADDRESS"

PACKAGE_ARGS=$RUN_DIR/kurtosis-args.json
jq -n \
    --arg chain_name arbdev \
    --argjson l1_chain_id "$L1_CHAIN_ID" \
    --arg l1_rpc_url "$INTERNAL_L1_RPC_URL" \
    --arg l1_beacon_url "$INTERNAL_L1_BEACON_URL" \
    --argjson l2_chain_id "$L2_CHAIN_ID" \
    --arg nitro_image "$NITRO_IMAGE" \
    --arg nitro_contracts_ref "$NITRO_CONTRACTS_REF" \
    --arg funder_private_key "$L1_FUNDER_PRIVATE_KEY" \
    --arg owner_private_key "$ARB_OWNER_PRIVATE_KEY" \
    --arg owner_address "$ARB_OWNER_ADDRESS" \
    --arg sequencer_private_key "$ARB_SEQUENCER_PRIVATE_KEY" \
    --arg sequencer_address "$ARB_SEQUENCER_ADDRESS" \
    --arg validator_private_key "$ARB_VALIDATOR_PRIVATE_KEY" \
    --arg validator_address "$ARB_VALIDATOR_ADDRESS" \
    '{
        arbitrum_package: {
            chain_name: $chain_name,
            l1_chain_id: $l1_chain_id,
            l1_rpc_url: $l1_rpc_url,
            l1_beacon_url: $l1_beacon_url,
            l2_chain_id: $l2_chain_id,
            nitro_image: $nitro_image,
            nitro_contracts_ref: $nitro_contracts_ref,
            funder_private_key: $funder_private_key,
            owner_private_key: $owner_private_key,
            owner_address: $owner_address,
            sequencer_private_key: $sequencer_private_key,
            sequencer_address: $sequencer_address,
            validator_private_key: $validator_private_key,
            validator_address: $validator_address
        }
    }' >"$PACKAGE_ARGS"
chmod 600 "$PACKAGE_ARGS"

if service_exists arb-sequencer; then
    log "reusing the existing Arbitrum deployment and starting its sequencer if stopped"
    kurtosis service start "$ENCLAVE" arb-sequencer >/dev/null 2>&1 || true
else
    log "deploying Arbitrum Nitro into the existing enclave (first run builds RollupCreator)"
    kurtosis run --enclave "$ENCLAVE" "$PACKAGE_DIR" --args-file "$PACKAGE_ARGS"
fi

L2_RPC_URL=$(kurtosis port print "$ENCLAVE" arb-sequencer rpc)
L2_WS_URL=$(kurtosis port print "$ENCLAVE" arb-sequencer ws)
NITRO_FEED_URL=$(kurtosis port print "$ENCLAVE" arb-sequencer feed)

l2_ready() {
    rpc_result "$L2_RPC_URL" eth_chainId
}

log "waiting for the Arbitrum sequencer RPC"
wait_until "$STARTUP_WAIT_SECS" "Arbitrum L2 RPC" l2_ready
OBSERVED_L2_CHAIN_ID=$(hex_to_decimal "$(rpc_result "$L2_RPC_URL" eth_chainId)")
[ "$OBSERVED_L2_CHAIN_ID" -eq "$L2_CHAIN_ID" ] ||
    fail "Arbitrum L2 chain ID is $OBSERVED_L2_CHAIN_ID, expected $L2_CHAIN_ID"

download_artifact() {
    local artifact=$1
    local destination=$2
    rm -rf "$destination"
    kurtosis files download "$ENCLAVE" "$artifact" "$destination"
}

CHAIN_INFO_DIR=$RUN_DIR/chain-info
SEQUENCER_CONFIG_DIR=$RUN_DIR/sequencer-config
download_artifact arb-chain-info "$CHAIN_INFO_DIR"
download_artifact arb-sequencer-config "$SEQUENCER_CONFIG_DIR"
CHAIN_INFO_FILE=$(find "$CHAIN_INFO_DIR" -name chain_info.json -type f | head -1)
SEQUENCER_CONFIG=$(find "$SEQUENCER_CONFIG_DIR" -name sequencer_config.json -type f | head -1)
[ -n "$CHAIN_INFO_FILE" ] || fail "arb-chain-info artifact did not contain chain_info.json"
[ -n "$SEQUENCER_CONFIG" ] || fail "arb-sequencer-config artifact did not contain sequencer_config.json"

ROLLUP_CORE_ADDRESS=$(jq -r '.[0].rollup.rollup // empty' "$CHAIN_INFO_FILE")
[[ "$ROLLUP_CORE_ADDRESS" =~ ^0x[0-9a-fA-F]{40}$ ]] ||
    fail "could not read RollupCore from $CHAIN_INFO_FILE"
INBOX_ADDRESS=$(jq -r '.[0].rollup.inbox // empty' "$CHAIN_INFO_FILE")
[[ "$INBOX_ADDRESS" =~ ^0x[0-9a-fA-F]{40}$ ]] ||
    fail "could not read Inbox from $CHAIN_INFO_FILE"

l2_account_is_funded() {
    local balance
    balance=$(cast balance "$1" --rpc-url "$L2_RPC_URL" --ether) || return 1
    awk -v balance="$balance" 'BEGIN { exit !(balance >= 1) }'
}

# The previous standalone devnet did this implicitly. Preserve the same
# handoff for deploy_l2_contracts.sh: bridge ETH from the L1 owner into L2,
# then fund its well-known development deployer. Normal restarts skip both
# transactions.
if ! l2_account_is_funded "$ARB_OWNER_ADDRESS"; then
    log "depositing ETH from the shared L1 into the Arbitrum owner account"
    cast send "$INBOX_ADDRESS" 'depositEth()' \
        --rpc-url "$L1_RPC_URL" \
        --private-key "$ARB_OWNER_PRIVATE_KEY" \
        --legacy \
        --gas-price 2gwei \
        --value 500ether >/dev/null
    wait_until "$STARTUP_WAIT_SECS" "Arbitrum owner L2 deposit" \
        l2_account_is_funded "$ARB_OWNER_ADDRESS"
fi

if ! l2_account_is_funded "$L2_DEPLOYER_ADDRESS"; then
    log "funding the standard L2 contract deployer"
    cast send "$L2_DEPLOYER_ADDRESS" \
        --rpc-url "$L2_RPC_URL" \
        --private-key "$ARB_OWNER_PRIVATE_KEY" \
        --value 100ether >/dev/null
    wait_until "$STARTUP_WAIT_SECS" "Arbitrum L2 contract deployer funding" \
        l2_account_is_funded "$L2_DEPLOYER_ADDRESS"
fi

find_contract_deployment_block() {
    local address=$1
    local latest_hex
    local low=0
    local high
    local middle
    local block_tag
    local code

    latest_hex=$(rpc_result "$L1_RPC_URL" eth_blockNumber)
    high=$(hex_to_decimal "$latest_hex")
    while [ "$low" -lt "$high" ]; do
        middle=$(((low + high) / 2))
        printf -v block_tag '0x%x' "$middle"
        code=$(rpc_result "$L1_RPC_URL" eth_getCode "[\"$address\",\"$block_tag\"]")
        if [ "$code" = 0x ]; then
            low=$((middle + 1))
        else
            high=$middle
        fi
    done
    printf '%d\n' "$low"
}

ROLLUP_DEPLOYMENT_BLOCK=$(find_contract_deployment_block "$ROLLUP_CORE_ADDRESS")
if [ "$ROLLUP_DEPLOYMENT_BLOCK" -eq 0 ]; then
    ROLLUP_DEPLOYMENT_BLOCK=1
fi

ASSERTION_CREATED_SIGNATURE='AssertionCreated(bytes32,bytes32,((bytes32,bytes32,(bytes32,uint256,address,uint64,uint64)),((bytes32[2],uint64[2]),uint8,bytes32),((bytes32[2],uint64[2]),uint8,bytes32)),bytes32,uint256,bytes32,uint256,address,uint64)'
ASSERTION_CREATED_TOPIC=$(cast keccak "$ASSERTION_CREATED_SIGNATURE")

finalized_assertion_logs() {
    local finalized_block
    local params

    finalized_block=$(rpc_result "$L1_RPC_URL" eth_getBlockByNumber '["finalized",false]') ||
        return 1
    finalized_block=$(printf '%s' "$finalized_block" | jq -er '.number')
    params=$(jq -cn \
        --arg address "$ROLLUP_CORE_ADDRESS" \
        --arg from_block "$(printf '0x%x' "$ROLLUP_DEPLOYMENT_BLOCK")" \
        --arg to_block "$finalized_block" \
        --arg topic "$ASSERTION_CREATED_TOPIC" \
        '[{
            address: $address,
            fromBlock: $from_block,
            toBlock: $to_block,
            topics: [$topic]
        }]')
    rpc_result "$L1_RPC_URL" eth_getLogs "$params"
}

has_finalized_assertion() {
    finalized_assertion_logs | jq -e 'length > 0'
}

validate_assertion_storage_layout() {
    local assertion_hash=$1
    local storage_key
    local storage_word
    local byte_from_left
    local status_hex
    local status

    [[ "$ASSERTIONS_MAPPING_SLOT" =~ ^0x[0-9a-fA-F]{64}$ ]] ||
        fail "ASSERTIONS_MAPPING_SLOT must be a 32-byte hexadecimal value"
    [[ "$ASSERTION_STATUS_OFFSET" =~ ^[0-9]+$ ]] &&
        [ "$ASSERTION_STATUS_OFFSET" -lt 32 ] ||
        fail "ASSERTION_STATUS_OFFSET must be between 0 and 31"

    storage_key=$(cast index bytes32 "$assertion_hash" "$ASSERTIONS_MAPPING_SLOT")
    storage_word=$(rpc_result "$L1_RPC_URL" eth_getStorageAt \
        "[\"$ROLLUP_CORE_ADDRESS\",\"$storage_key\",\"finalized\"]")
    [[ "$storage_word" =~ ^0x[0-9a-fA-F]{64}$ ]] ||
        fail "RollupCore returned a malformed _assertions storage word"

    # Solidity storage offsets count from the least-significant byte, whereas
    # the RPC word is rendered most-significant byte first.
    byte_from_left=$((31 - ASSERTION_STATUS_OFFSET))
    status_hex=${storage_word:$((2 + byte_from_left * 2)):2}
    status=$((16#$status_hex))
    [ "$status" -eq 1 ] || [ "$status" -eq 2 ] ||
        fail "configured BoLD storage layout did not resolve a pending or confirmed assertion (status=$status)"
}

log "waiting for the first AssertionCreated event covered by finalized shared L1 (max ${ASSERTION_WAIT_SECS}s)"
if wait_until "$ASSERTION_WAIT_SECS" "first finalized BoLD assertion" has_finalized_assertion; then
    ASSERTION_HASH=$(finalized_assertion_logs | jq -er '.[-1].topics[1]')
    validate_assertion_storage_layout "$ASSERTION_HASH"
    log "validated BoLD _assertions layout with finalized assertion $ASSERTION_HASH"
else
    log "WARNING: no finalized assertion yet; keeping the reviewed $NITRO_CONTRACTS_REF storage-layout defaults"
fi

ENV_FILE=$RUN_DIR/attestor.env
{
    printf 'export L1_RPC_URL=%q\n' "$L1_RPC_URL"
    printf 'export L1_WS_URL=%q\n' "$L1_WS_URL"
    printf 'export L1_BEACON_URL=%q\n' "$L1_BEACON_API_URL"
    printf 'export L1_BEACON_API_URL=%q\n' "$L1_BEACON_API_URL"
    printf 'export L1_CHAIN_ID=%q\n' "$L1_CHAIN_ID"
    printf 'export L2_RPC_URL=%q\n' "$L2_RPC_URL"
    printf 'export L2_WS_URL=%q\n' "$L2_WS_URL"
    printf 'export L2_CHAIN_ID=%q\n' "$OBSERVED_L2_CHAIN_ID"
    printf 'export ATTESTOR_SIGNING_KEY=%q\n' "$ATTESTOR_SIGNING_KEY"
    printf 'export ATTESTOR_PUBLIC_KEY=%q\n' "$ATTESTOR_PUBLIC_KEY"
    printf 'export ROLLUP_CORE_ADDRESS=%q\n' "$ROLLUP_CORE_ADDRESS"
    printf 'export ROLLUP_DEPLOYMENT_BLOCK=%q\n' "$ROLLUP_DEPLOYMENT_BLOCK"
    printf 'export ASSERTIONS_MAPPING_SLOT=%q\n' "$ASSERTIONS_MAPPING_SLOT"
    printf 'export ASSERTION_STATUS_OFFSET=%q\n' "$ASSERTION_STATUS_OFFSET"
    printf 'export SRC_CHAIN=%q\n' arbdev
    # Match what run_optimism_node.sh and run_base_node.sh hand off. Without this
    # the Arbitrum attestor took the 150-block default and a local devnet paid a
    # multi-minute return-direction wait for nothing — the frontier advances one
    # derived root per gap, so the gap IS the latency floor. Named
    # DERIVED_GAP_BLOCKS to match the other two stacks.
    printf 'export DERIVED_GAP_BLOCKS=%q\n' 5
} >"$ENV_FILE"
chmod 600 "$ENV_FILE"

log "local Arbitrum stack is ready on the shared L1"
cat <<EOF
Kurtosis enclave:       $ENCLAVE
L1 execution RPC:       $L1_RPC_URL
L1 WebSocket:           $L1_WS_URL
L1 beacon API:          $L1_BEACON_API_URL
L1 chain ID:            $L1_CHAIN_ID
L2 execution RPC:       $L2_RPC_URL
L2 WebSocket:           $L2_WS_URL
L2 chain ID:            $OBSERVED_L2_CHAIN_ID
Funded L2 deployer:     $L2_DEPLOYER_ADDRESS
Sequencer feed:         $NITRO_FEED_URL
RollupCore:             $ROLLUP_CORE_ADDRESS
Rollup deployment L1:   $ROLLUP_DEPLOYMENT_BLOCK
BoLD assertions slot:   $ASSERTIONS_MAPPING_SLOT
Attestor environment:   $ENV_FILE

Run the independent Nitro replica and Arbitrum attestor:
  source "$ENV_FILE"
  scripts/local/run_arbitrum_attestor.sh

Stop only Arbitrum while preserving the shared L1 and OP:
  scripts/local/run_arbitrum_node.sh --stop

Redeploy only Arbitrum on the existing L1:
  scripts/local/run_arbitrum_node.sh --reset

Remove the complete shared L1 + all attached rollups:
  kurtosis enclave rm -f "$ENCLAVE"
EOF
