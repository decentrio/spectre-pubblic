#!/usr/bin/env bash

# Local OP Stack devnet for attestor testing, via Kurtosis, in two layers inside one
# enclave:
#   1. L1 — brought up by scripts/local/run_eth_node.sh (ethereum-package, geth +
#      lighthouse, Fulu-from-genesis, eth-network-params.yaml). This script passes it
#      the exact ethereum-package ref optimism-package pins, in the op-devnet enclave.
#   2. L2 — ethpandaops/optimism-package attached to that L1 via
#      external_l1_network_params: sequencer + an independent replica op-node +
#      batcher + proposer posting real DisputeGameFactory games every 60s.
#
# The OP Stack components (op-geth, op-node, op-batcher, op-proposer, op-deployer,
# contracts) are pinned to the LATEST ethereum-optimism releases via the `registry:`
# map in scripts/local/op-network-params.yaml. Ends by printing (and writing to
# $RUN_DIR/attestor.env) everything scripts/local/run_op_attestor.sh needs.
#
# Optional env:
#   ENCLAVE (op-devnet)         kurtosis enclave name
#   OP_PACKAGE_REF              ethpandaops/optimism-package git ref to clone
#   RUN_DIR (.op-devnet-run)    package clones + downloaded artifacts + attestor.env
#   GAME_WAIT_SECS (900)        max wait for the first proposed game
#   OP_OBSERVABILITY (0)        set to 1 to include Grafana/Loki/Prometheus
#   ATTESTOR_SIGNING_KEY / ATTESTOR_PUBLIC_KEY
#                              optional matching Ed25519 devnet identity; set both
#                              to replace the checked-in disposable test identity

set -euo pipefail

cd "$(dirname "$0")/../.."
REPO_ROOT=$PWD
# shellcheck disable=SC1091
. "$REPO_ROOT/scripts/local/devnet_attestor_identity.sh"

ENCLAVE=${ENCLAVE:-op-devnet}
# main @ 2025-09-19 — newest optimism-package; its ethereum-package pin is Fulu-era,
# which lighthouse v8 genesis handling requires.
OP_PACKAGE_REF=${OP_PACKAGE_REF:-7bef190d7c0b9f619438ed08b17bd5e5f51e72ff}
RUN_DIR=${RUN_DIR:-$REPO_ROOT/.op-devnet-run}
GAME_WAIT_SECS=${GAME_WAIT_SECS:-900}
OP_OBSERVABILITY=${OP_OBSERVABILITY:-0}

mkdir -p "$RUN_DIR"

log() { printf '\n[run_optimism_node] %s\n' "$*"; }

case "$OP_OBSERVABILITY" in
    0|1) ;;
    *)
        log "ERROR: OP_OBSERVABILITY must be 0 or 1, got $OP_OBSERVABILITY"
        exit 1
        ;;
esac

# wait_until <timeout-secs> <description> <command...> — poll every 5s.
wait_until() {
    local timeout=$1 desc=$2; shift 2
    local deadline=$((SECONDS + timeout))
    until "$@" >/dev/null 2>&1; do
        if [ "$SECONDS" -ge "$deadline" ]; then
            log "TIMEOUT after ${timeout}s waiting for: $desc"
            return 1
        fi
        sleep 5
    done
}

# ----------------------------------------- optimism-package clone + patch ---
# Cloned locally so it can be patched: op-deployer >= v0.5 requires a non-zero
# operatorFeeVaultRecipient the package (1.4.0) predates; op-node >= v1.18 needs an
# --rollup.l1-chain-config; the fund script needs an explicit gas price on a
# near-zero-basefee L1.
PKG_DIR=$RUN_DIR/optimism-package
if [ ! -e "$PKG_DIR/.pinned-ref" ] || [ "$(cat "$PKG_DIR/.pinned-ref")" != "$OP_PACKAGE_REF" ]; then
    log "cloning ethpandaops/optimism-package@$OP_PACKAGE_REF into $PKG_DIR"
    rm -rf "$PKG_DIR"
    mkdir -p "$PKG_DIR"
    git -C "$PKG_DIR" init --quiet
    git -C "$PKG_DIR" fetch --quiet --depth 1 \
        https://github.com/ethpandaops/optimism-package "$OP_PACKAGE_REF"
    git -C "$PKG_DIR" checkout --quiet FETCH_HEAD
    echo "$OP_PACKAGE_REF" > "$PKG_DIR/.pinned-ref"
fi

DEPLOYER_STAR=$PKG_DIR/src/contracts/contract_deployer.star
if ! grep -q operatorFeeVaultRecipient "$DEPLOYER_STAR"; then
    log "patching contract_deployer.star (operatorFeeVaultRecipient for op-deployer >= v0.5)"
    python3 - "$DEPLOYER_STAR" <<'PYEOF'
import re
import sys
path = sys.argv[1]
src = open(path).read()
pat = re.compile(
    r'([ \t]+)"sequencerFeeVaultRecipient": read_chain_cmd\(\s*\n'
    r'[ \t]+"sequencerFeeVaultRecipient", chain_id\s*\n'
    r'[ \t]+\),'
)
m = pat.search(src)
if not m:
    sys.exit("anchor for operatorFeeVaultRecipient patch not found")
indent = m.group(1)
insert = (
    f'\n{indent}"operatorFeeVaultRecipient": read_chain_cmd(\n'
    f'{indent}    "sequencerFeeVaultRecipient", chain_id\n'
    f'{indent}),'
)
src = src[: m.end()] + insert + src[m.end() :]
open(path, "w").write(src)
PYEOF
fi
# op-node >= v1.18 must be told the L1 chain config when the L1 chain id is not a
# known network. The ethereum-package L1 genesis carries the legacy
# terminalTotalDifficultyPassed key geth dropped in v1.15. Mount it in the fund
# step; fund.sh (which already has jq) writes the cleaned chain config. Keeping the
# shell program in fund.sh avoids a Kurtosis 1.11 Starlark interpreter hang caused
# by the equivalent inline run-string.
if ! grep -Fq 'plan.get_files_artifact(name="el_cl_genesis_data")' "$DEPLOYER_STAR"; then
    log "patching contract_deployer.star (mount L1 genesis for chain config)"
    python3 - "$DEPLOYER_STAR" <<'PYEOF'
import sys
path = sys.argv[1]
src = open(path).read()
new_mount = '"/l1-genesis": plan.get_files_artifact(name="el_cl_genesis_data"),'
old_mount = '"/l1-genesis": "el_cl_genesis_data",'
files_anchor = '''"/network-data": op_deployer_init.files_artifacts[0],
            "/fund-script": fund_script_artifact,
        },'''
if old_mount in src:
    # Migrate clones patched by the older script version, which used the
    # artifact name string rather than the artifact object.
    src = src.replace(old_mount, new_mount, 1)
elif files_anchor in src:
    src = src.replace(files_anchor, files_anchor.replace(
        '"/fund-script": fund_script_artifact,',
        '\"/fund-script\": fund_script_artifact,\n            \"/l1-genesis\": plan.get_files_artifact(name=\"el_cl_genesis_data\"),'), 1)
else:
    sys.exit("anchor for l1-chain-config patch not found in contract_deployer.star")
open(path, "w").write(src)
PYEOF
fi
# The fund script sends without an explicit max fee; on a quiet devnet L1 the basefee
# decays to a few wei and cast then computes maxFeePerGas < maxPriorityFeePerGas and
# every funding tx is rejected. Pin an explicit max fee. Portable edit (no sed -i).
FUND_SH=$(find "$PKG_DIR/static_files" -name fund.sh | head -1)
if [ -n "$FUND_SH" ] && ! grep -q -- '--gas-price 2gwei' "$FUND_SH"; then
    log "patching fund.sh (explicit --gas-price for a near-zero-basefee L1)"
    sed 's/--priority-gas-price 1gwei/--gas-price 2gwei --priority-gas-price 1gwei/' "$FUND_SH" > "$FUND_SH.tmp"
    mv "$FUND_SH.tmp" "$FUND_SH"
fi
if [ -n "$FUND_SH" ] && ! grep -q -- 'l1-chain-config.json' "$FUND_SH"; then
    log "patching fund.sh (write cleaned L1 chain config)"
    python3 - "$FUND_SH" <<'PYEOF'
import sys
path = sys.argv[1]
src = open(path).read()
anchor = "\nwait\n"
if anchor not in src:
    sys.exit("anchor for l1-chain-config patch not found in fund.sh")
src = src.replace(anchor, """
wait

# op-node rejects the legacy terminalTotalDifficultyPassed key in the generated
# ethereum-package genesis. It needs this cleaned config for an unknown local L1.
jq 'del(.config.terminalTotalDifficultyPassed)' /l1-genesis/genesis.json > /network-data/l1-chain-config.json
""", 1)
open(path, "w").write(src)
PYEOF
fi

OPNODE_STAR=$PKG_DIR/src/cl/op-node/launcher.star
if ! grep -q 'rollup.l1-chain-config' "$OPNODE_STAR"; then
    log "patching op-node launcher.star (--rollup.l1-chain-config for op-node >= v1.18)"
    python3 - "$OPNODE_STAR" <<'PYEOF'
import sys
path = sys.argv[1]
src = open(path).read()
cmd_anchor = '"--rpc.enable-admin",'
if cmd_anchor not in src:
    sys.exit("anchor for l1-chain-config patch not found in launcher.star")
src = src.replace(cmd_anchor, cmd_anchor + '''
        "--rollup.l1-chain-config=/network-configs/l1-chain-config.json",''', 1)
open(path, "w").write(src)
PYEOF
fi

# optimism-package's observability bundle (Grafana/Loki/Prometheus) can deadlock the
# Kurtosis 1.11 interpreter during plan construction on this local setup. It is not
# needed by the OP attestor or IBC test path, so skip it by default while keeping its
# helper enabled for the batcher/proposer launchers. Set OP_OBSERVABILITY=1 to restore
# the package default.
MAIN_STAR=$PKG_DIR/main.star
# The marker is matched without its project prefix so a main.star patched by an
# older checkout (which used a different prefix) is still recognised.
OBS_MARKER=': observability intentionally skipped in the local devnet.'
if [ "$OP_OBSERVABILITY" = "0" ] && ! grep -Fq "$OBS_MARKER" "$MAIN_STAR"; then
    log "patching main.star (skip optional observability bundle in local devnet)"
    python3 - "$MAIN_STAR" <<'PYEOF'
import sys
path = sys.argv[1]
src = open(path).read()
anchor = '''    observability.launch(
        plan, observability_helper, global_node_selectors, observability_params
    )'''
if anchor not in src:
    sys.exit("anchor for observability patch not found in main.star")
src = src.replace(anchor, '''    # spectre: observability intentionally skipped in the local devnet.
    plan.print("Skipping optional observability bundle")''', 1)
open(path, "w").write(src)
PYEOF
elif [ "$OP_OBSERVABILITY" = "1" ] && grep -Fq "$OBS_MARKER" "$MAIN_STAR"; then
    log "restoring main.star (enable optional observability bundle)"
    python3 - "$MAIN_STAR" <<'PYEOF'
import re, sys
path = sys.argv[1]
src = open(path).read()
skipped = re.compile(
    r'    # [\w-]+: observability intentionally skipped in the local devnet\.\n'
    r'    plan\.print\("Skipping optional observability bundle"\)'
)
launch = '''    observability.launch(
        plan, observability_helper, global_node_selectors, observability_params
    )'''
if not skipped.search(src):
    sys.exit("anchor for observability restore not found in main.star")
src = skipped.sub(lambda _: launch, src, count=1)
open(path, "w").write(src)
PYEOF
fi

# ------------------------------------------------------------------- L1 ---
# Reuse run_eth_node.sh for the L1, in THIS enclave, with the exact ethereum-package
# ref optimism-package pins (so the OP package's Starlark import is not clobbered).
ETH_PIN=$(grep -oE 'github.com/ethpandaops/ethereum-package@[0-9a-f]+' \
    "$PKG_DIR/kurtosis.yml" | head -1 | cut -d@ -f2)
[ -n "$ETH_PIN" ] || { log "ERROR: no ethereum-package pin in $PKG_DIR/kurtosis.yml"; exit 1; }

log "bringing up the L1 via run_eth_node.sh (ethereum-package@$ETH_PIN, enclave $ENCLAVE)"
ENCLAVE="$ENCLAVE" ETH_PIN="$ETH_PIN" RUN_DIR="$RUN_DIR" \
    L1_PARAMS=eth-network-params.yaml SKIP_COSMOS_RESET=1 \
    sh scripts/local/run_eth_node.sh

# shellcheck disable=SC1091
. "$RUN_DIR/eth.env" # ETH_RPC / ETH_WS / ETH_BEACON_API
L1_RPC_URL=$ETH_RPC
L1_CHAIN_ID=$(cast chain-id --rpc-url "$L1_RPC_URL")

# Validate up front: fail fast if the beacon endpoint itself is unreachable (wrong port
# discovered, or the CL service is down) instead of the finality wait silently
# looping for 360s. A live beacon answers this endpoint within seconds of the L1
# coming up — even before finality, it returns 200 with finalized.epoch == 0.
beacon_reachable() {
    curl -sf "$ETH_BEACON_API/eth/v1/beacon/states/head/finality_checkpoints" >/dev/null 2>&1
}
wait_until 60 "L1 beacon reachable at $ETH_BEACON_API" beacon_reachable || {
    log "ERROR: L1 beacon not reachable at $ETH_BEACON_API — check the run_eth_node.sh port discovery"
    exit 1
}

# The beacon is up; now wait only for finality to advance past genesis (epoch 0).
l1_finalized() {
    local epoch
    epoch=$(curl -sf "$ETH_BEACON_API/eth/v1/beacon/states/head/finality_checkpoints" \
        | jq -r '.data.finalized.epoch // 0') || return 1
    [ "${epoch:-0}" -gt 0 ]
}
log "waiting for the first finalized L1 epoch (~2min at 2s slots) before layering the L2 on top"
wait_until 360 "first finalized L1 epoch" l1_finalized

# ------------------------------------------------------------------- L2 ---
log "starting OP Stack L2 (package: $PKG_DIR @ $OP_PACKAGE_REF, patched)"
kurtosis run --enclave "$ENCLAVE" "$PKG_DIR" \
    --args-file scripts/local/op-network-params.yaml

# ------------------------------------------------------------ discovery ---
# op-node services: one per participant. The attestor uses the *replica*
# participant's op-node (independent verifier); the sequencer's is first.
mapfile -t OP_NODE_SVCS < <(kurtosis enclave inspect "$ENCLAVE" | grep -oE 'op-cl-[a-zA-Z0-9-]+' | sort -u)
if [ "${#OP_NODE_SVCS[@]}" -eq 0 ]; then
    log "ERROR: no op-node (op-cl-*) services found in enclave $ENCLAVE"
    kurtosis enclave inspect "$ENCLAVE"
    exit 1
fi
REPLICA_SVC=""
for svc in "${OP_NODE_SVCS[@]}"; do
    case $svc in *replica*) REPLICA_SVC=$svc ;; esac
done
# Fall back to the last op-node service if none is named "replica".
[ -n "$REPLICA_SVC" ] || REPLICA_SVC=${OP_NODE_SVCS[${#OP_NODE_SVCS[@]}-1]}
OP_NODE_RPC_URL="http://$(kurtosis port print "$ENCLAVE" "$REPLICA_SVC" rpc | sed 's|^http://||')"

# op-geth (L2 exec) services. IBC contracts deploy here (deploy_l2_contracts.sh),
# so target the SEQUENCER's exec — the canonical block producer — not the replica.
mapfile -t OP_EL_SVCS < <(kurtosis enclave inspect "$ENCLAVE" | grep -oE 'op-el-[a-zA-Z0-9-]+' | sort -u)
if [ "${#OP_EL_SVCS[@]}" -eq 0 ]; then
    log "ERROR: no op-geth (op-el-*) services found in enclave $ENCLAVE"
    kurtosis enclave inspect "$ENCLAVE"
    exit 1
fi
SEQ_EL_SVC=""
for svc in "${OP_EL_SVCS[@]}"; do
    case $svc in *replica*) ;; *) SEQ_EL_SVC=$svc; break ;; esac
done
[ -n "$SEQ_EL_SVC" ] || SEQ_EL_SVC=${OP_EL_SVCS[0]}
L2_RPC_URL="http://$(kurtosis port print "$ENCLAVE" "$SEQ_EL_SVC" rpc | sed 's|^http://||')"
L2_WS_URL="ws://$(kurtosis port print "$ENCLAVE" "$SEQ_EL_SVC" ws 2>/dev/null | sed 's|^ws://||')" || L2_WS_URL=""
L2_CHAIN_ID=$(cast chain-id --rpc-url "$L2_RPC_URL")

log "L1 RPC:            $L1_RPC_URL"
log "replica op-node:   $OP_NODE_RPC_URL   ($REPLICA_SVC)"
log "sequencer op-geth: $L2_RPC_URL   ($SEQ_EL_SVC, chain $L2_CHAIN_ID)"

# Contract addresses from op-deployer's state artifact.
rm -rf "$RUN_DIR/op-deployer"
kurtosis files download "$ENCLAVE" op-deployer-configs "$RUN_DIR/op-deployer"
STATE_JSON=$(find "$RUN_DIR/op-deployer" -name 'state.json' | head -1)
if [ -z "$STATE_JSON" ]; then
    log "ERROR: state.json not found in the op-deployer-configs artifact"
    find "$RUN_DIR/op-deployer" -maxdepth 2 -type f
    exit 1
fi
DISPUTE_GAME_FACTORY=$(jq -r '.opChainDeployments[0].DisputeGameFactoryProxy' "$STATE_JSON")
OPTIMISM_PORTAL=$(jq -r '.opChainDeployments[0].OptimismPortalProxy' "$STATE_JSON")
if [ -z "$DISPUTE_GAME_FACTORY" ] || [ "$DISPUTE_GAME_FACTORY" = "null" ]; then
    log "ERROR: DisputeGameFactoryProxy not in $STATE_JSON — dumping keys:"
    jq '.opChainDeployments[0] | keys' "$STATE_JSON"
    exit 1
fi
# The proposer posts the game type from op-network-params.yaml.
RESPECTED_GAME_TYPE=$(grep -A2 'proposer_params:' scripts/local/op-network-params.yaml | grep 'game_type:' | awk '{print $2}')

log "DisputeGameFactory: $DISPUTE_GAME_FACTORY"
log "OptimismPortal:     $OPTIMISM_PORTAL"
log "game type:          $RESPECTED_GAME_TYPE (proposer_params in op-network-params.yaml)"

# ----------------------------------------------------------- readiness ---
log "waiting for the replica op-node to answer optimism_syncStatus"
sync_status() {
    curl -sf -X POST -H 'Content-Type: application/json' \
        --data '{"jsonrpc":"2.0","id":1,"method":"optimism_syncStatus","params":[]}' \
        "$OP_NODE_RPC_URL" | jq -e '.result.finalized_l2.number'
}
wait_until 180 "optimism_syncStatus on the replica" sync_status

game_count() {
    local n
    n=$(cast call "$DISPUTE_GAME_FACTORY" 'gameCount()(uint256)' --rpc-url "$L1_RPC_URL" | awk '{print $1}')
    [ "${n:-0}" -ge 1 ]
}
log "waiting for the proposer's first DisputeGameFactory game (max ${GAME_WAIT_SECS}s; interval 60s but the proposer waits for a safe/finalized L2 block first)"
if wait_until "$GAME_WAIT_SECS" "first proposed game" game_count; then
    log "games proposed: $(cast call "$DISPUTE_GAME_FACTORY" 'gameCount()(uint256)' --rpc-url "$L1_RPC_URL")"
else
    log "WARNING: no game proposed yet — check: kurtosis service logs $ENCLAVE \$(kurtosis enclave inspect $ENCLAVE | grep -oE 'op-proposer[a-zA-Z0-9-]*' | head -1)"
fi

# ------------------------------------------------------------- handoff ---
ENV_FILE=$RUN_DIR/attestor.env
cat > "$ENV_FILE" <<EOF
export L1_RPC_URL=$L1_RPC_URL
export L1_WS_URL=$ETH_WS
export L1_CHAIN_ID=$L1_CHAIN_ID
export ETH_BEACON_API=$ETH_BEACON_API
export OP_NODE_RPC_URL=$OP_NODE_RPC_URL
export L2_RPC_URL=$L2_RPC_URL
export L2_WS_URL=$L2_WS_URL
export L2_CHAIN_ID=$L2_CHAIN_ID
export ATTESTOR_SIGNING_KEY=$ATTESTOR_SIGNING_KEY
export ATTESTOR_PUBLIC_KEY=$ATTESTOR_PUBLIC_KEY
export DISPUTE_GAME_FACTORY=$DISPUTE_GAME_FACTORY
export OPTIMISM_PORTAL=$OPTIMISM_PORTAL
export RESPECTED_GAME_TYPE=$RESPECTED_GAME_TYPE
export SRC_CHAIN=opdev
# Low-latency profile (attestor/optimism/GUIDE.md "fastest safe profile"): attest
# unsafe-head roots ~seconds after block production; roots stay provisional
# until the finalized recheck confirms them (consumers pass
# include_provisional=true for the fresh frontier).
export ATTESTATION_HEAD=unsafe
export POLL_INTERVAL_SECONDS=2
export DERIVED_GAP_BLOCKS=5
export LOOKBACK_BLOCKS=1000
EOF
chmod 600 "$ENV_FILE"

log "devnet is up. Attestor env written to $ENV_FILE"
cat <<EOF

Run the attestor against it:

  source $ENV_FILE
  ./scripts/local/run_op_attestor.sh

Inspect the devnet:

  kurtosis enclave inspect $ENCLAVE
  cast call $DISPUTE_GAME_FACTORY 'gameCount()(uint256)' --rpc-url $L1_RPC_URL

Tear down:

  kurtosis enclave rm -f $ENCLAVE

EOF
