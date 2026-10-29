#!/usr/bin/env bash

# Local Base devnet in its own Kurtosis enclave (base-devnet by default).
#
# Base no longer runs vanilla OP Stack binaries — the base/base monorepo ships
# its own unified Rust node (`base`: execution + consensus in one process,
# sequencer/rpc/bootnode roles) and its own batcher. This script brings that
# stack up via the local package in scripts/local/kurtosis/base:
#
#   base-bootnode  EL/CL discovery bootstrap
#   base-builder   sequencer (EL + CL)
#   base-batcher   posts L2 batches to the L1
#   base-client    independent follower — what the attestor and the L2 IBC
#                  contracts point at
#
# The L1 is never owned by this script, only used: if ENCLAVE exists it attaches
# to whatever ethereum-package L1 is in it; if it does not, the L1 is created
# here via run_eth_node.sh. Because ENCLAVE defaults to its own name, a Base run
# gets its own L1 and cannot disturb an OP or Arbitrum devnet. To settle Base to
# an L1 shared with them, run this with ENCLAVE pointed at their enclave
# (ENCLAVE=op-devnet ./scripts/local/run_base_node.sh) — the package attaches by
# kurtosis-internal service DNS and never creates a second L1.
#
# Bring-up order:
#   1. This script: builds the base/base images if missing, creates or attaches
#      to the L1, funds the Base role accounts on it, runs the Base Kurtosis
#      package, and writes RUN_DIR/attestor.env.
#   2. scripts/local/run_op_attestor.sh (sourced with that env) attests Base —
#      the OP Stack attestor needs no changes: base-node's consensus RPC keeps
#      the op-node namespace (optimism_syncStatus / optimism_outputAtBlock,
#      base/base crates/consensus/rpc/src/rollup.rs).
#
# NOTE: nothing on this devnet posts dispute games (Base replaced op-proposer
# with a ZK prover pipeline this package does not run), so the attestor's game
# feed stays at gameCount=0 and what gets exercised is the proposal-independent
# derived-root path — which is exactly the feed consumers wait on
# (opstack.go attestDerived).
#
# The base/base source is cloned+patched into RUN_DIR at a pinned ref (BASE_REF)
# and the images are built from that clone — no sibling checkout needed. The
# first build compiles the base Rust workspace and takes tens of minutes; later
# runs reuse the images (docker image tags, which is where Kurtosis picks them
# up: the package names them and image_download defaults to "missing").
#
# Prerequisites: kurtosis, docker (+ buildx), git, python3, jq, cast.

set -euo pipefail

cd "$(dirname "$0")/../.."
REPO_ROOT=$PWD
# shellcheck disable=SC1091
. "$REPO_ROOT/scripts/local/devnet_attestor_identity.sh"

# Base gets its own enclave (and therefore its own L1) by default, so a Base run
# can never disturb an OP or Arbitrum devnet. To settle Base to a shared L1
# instead, point ENCLAVE at the enclave that already holds it — this script
# attaches to any L1 it finds and only creates one when the enclave is absent.
ENCLAVE=${ENCLAVE:-base-devnet}
RUN_DIR=${RUN_DIR:-$REPO_ROOT/.base-devnet-run}
PACKAGE_DIR=${PACKAGE_DIR:-$REPO_ROOT/scripts/local/kurtosis/base}
# base/base is cloned+patched into RUN_DIR (like the ethereum-/optimism-package
# clones) rather than read from a sibling checkout, so the images are built from
# a known ref. main @ 2026-07-25, the revision this stack was brought up on.
BASE_GIT_URL=${BASE_GIT_URL:-https://github.com/base/base.git}
BASE_REF=${BASE_REF:-dab0d0a275ea88f598001c6c6b99933df9f6d208}
L2_CHAIN_ID=${L2_CHAIN_ID:-84538453}
STARTUP_WAIT_SECS=${STARTUP_WAIT_SECS:-900}
FINALITY_WAIT_SECS=${FINALITY_WAIT_SECS:-900}
FINALIZED_WAIT_SECS=${FINALIZED_WAIT_SECS:-600}
SKIP_IMAGE_BUILD=${SKIP_IMAGE_BUILD:-0}
REBUILD_IMAGES=${REBUILD_IMAGES:-0}

NODE_IMAGE=${NODE_IMAGE:-base:local}
BATCHER_IMAGE=${BATCHER_IMAGE:-base-batcher:local}
SETUP_IMAGE=${SETUP_IMAGE:-devnet-setup:local}

# Public development keys. The funder is the prefunded ethereum-package dev
# account the OP and Arbitrum stacks already use; the Base role accounts are the
# standard anvil accounts base/base's etc/docker/devnet-env assigns. Never use
# these keys outside local devnets.
L1_FUNDER_PRIVATE_KEY=${L1_FUNDER_PRIVATE_KEY:-eaba42282ad33c8ef2524f07277c03a776d98ae19f581990ce75becb7cfa1c23}
BASE_DEPLOYER_ADDRESS=${BASE_DEPLOYER_ADDRESS:-0xf39Fd6e51aad88F6F4ce6aB8827279cffFb92266}
BASE_DEPLOYER_PRIVATE_KEY=${BASE_DEPLOYER_PRIVATE_KEY:-0xac0974bec39a17e36ba4a6b4d238ff944bacb478cbed5efcae784d7bf4f2ff80}
BASE_SEQUENCER_ADDRESS=${BASE_SEQUENCER_ADDRESS:-0x9965507D1a55bcC2695C58ba16FB37d819B0A4dc}
BASE_BATCHER_ADDRESS=${BASE_BATCHER_ADDRESS:-0x976EA74026E726554dB657fA54763abd0C3a0aa9}
BASE_PROPOSER_ADDRESS=${BASE_PROPOSER_ADDRESS:-0x14dC79964da2C08b23698B3D3cc7Ca32193d9955}
BASE_CHALLENGER_ADDRESS=${BASE_CHALLENGER_ADDRESS:-0x23618e81E3f5cdF7f54C3d65f7FBc0aBf5B21E8f}
L2_DEPLOYER_ADDRESS=${L2_DEPLOYER_ADDRESS:-0x8943545177806ED17B9F23F0a21ee5948eCaa776}
# Pays for the L2 top-up above. op-deployer's fundDevAccounts prefunds anvil
# accounts 1-9 in the L2 genesis but NOT account 0 (BASE_DEPLOYER_ADDRESS), and
# never 0x8943… — that address comes from the ethpandaops mnemonic and is only
# prefunded on optimism-package's own L2. So the funder has to be one of the
# accounts actually in the genesis alloc: anvil #1, which holds no role here.
L2_FUNDER_ADDRESS=${L2_FUNDER_ADDRESS:-0x70997970C51812dc3A010C7d01b50e0d17dc79C8}
L2_FUNDER_PRIVATE_KEY=${L2_FUNDER_PRIVATE_KEY:-0x59c6995e998f97a5a0044966f0945389dc9e86dae88c7a8412f4603b6b78690d}

BASE_SERVICES="base-client base-batcher base-builder base-bootnode"

# Where run_op_attestor.sh keeps its state when driven from this handoff (the
# RUN_DIR exported at the bottom). Its attested-roots file is keyed by L2 block
# height and survives enclave recreation, so a fresh chain inherits the previous
# one's high-water mark: the attestor then goes quiet for as long as the new
# chain needs to climb past it — looking hung while being, from its own point of
# view, perfectly correct. Deploying a new chain invalidates that state.
ATTESTOR_RUN_DIR=${ATTESTOR_RUN_DIR:-$REPO_ROOT/.base-attestor-run}
ATTESTOR_SRC_CHAIN=basedev

ACTION=up
RESET=false

usage() {
    cat <<'EOF'
Usage: scripts/local/run_base_node.sh [--reset|--stop|--status]

Adds the Base stack (bootnode + sequencer + batcher + follower) to a Kurtosis
enclave on a shared Ethereum L1. Attaches to the L1 already in ENCLAVE (e.g. one
run_optimism_node.sh created, so every rollup settles to the same chain), or
brings that L1 up itself when the enclave does not exist.

Options:
  --reset   Redeploy only the Base services/contracts on the existing L1.
            The shared Ethereum, OP and Arbitrum services are preserved.
  --stop    Stop the Base services, preserving the L1.
  --status  Show the Base services in the shared enclave.
  -h        Show this help.

Environment:
  ENCLAVE                Kurtosis enclave (default: base-devnet — its own, so a
                         Base run cannot disturb an OP/Arbitrum devnet). Point it
                         at an existing enclave to share that L1 instead.
  RUN_DIR                Runtime artifacts (default: .base-devnet-run).
  PACKAGE_DIR            Local external-L1 Base Kurtosis package.
  BASE_GIT_URL           base/base remote to clone (default: github.com/base/base).
  BASE_REF               base/base commit the images are built from.
  BASE_REPO              Build from this existing checkout instead of the pinned
                         clone (used as-is: no fetch, no patching).
  SKIP_IMAGE_BUILD       1 = assume base:local / base-batcher:local /
                         devnet-setup:local already exist.
  REBUILD_IMAGES         1 = rebuild the images from the pinned clone even when
                         they already exist.
  NODE_IMAGE             Base node image (default: base:local).
  BATCHER_IMAGE          Base batcher image (default: base-batcher:local).
  SETUP_IMAGE            op-deployer/setup image (default: devnet-setup:local).
  L2_CHAIN_ID            Base devnet chain ID (default: 84538453).
  ATTESTOR_SIGNING_KEY / ATTESTOR_PUBLIC_KEY
                         Matching Ed25519 identity for the local attestor. Set
                         both to override the disposable test identity.
  STARTUP_WAIT_SECS      Service startup timeout (default: 900).
  FINALITY_WAIT_SECS     Shared-L1 finality timeout (default: 900).
  FINALIZED_WAIT_SECS    First finalized L2 block timeout (default: 600).

The development keys/addresses may be overridden with L1_FUNDER_PRIVATE_KEY,
BASE_DEPLOYER_{ADDRESS,PRIVATE_KEY}, BASE_{SEQUENCER,BATCHER,PROPOSER,CHALLENGER}_ADDRESS,
L2_DEPLOYER_ADDRESS and L2_FUNDER_{ADDRESS,PRIVATE_KEY}. The L1 funder must already
have ETH on the L1; the L2 funder must be prefunded in the L2 genesis alloc.
EOF
}

fail() {
    printf '[run_base_node] ERROR: %s\n' "$*" >&2
    exit 1
}

log() {
    printf '\n[run_base_node] %s\n' "$*"
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
require_command docker
require_command curl
require_command jq
require_command cast

[ -f "$PACKAGE_DIR/kurtosis.yml" ] ||
    fail "Base Kurtosis package not found at $PACKAGE_DIR"

enclave_inspect() {
    kurtosis enclave inspect "$ENCLAVE"
}

# Matches a whitespace-delimited name anywhere in `kurtosis enclave inspect`,
# which lists services and file artifacts in one table.
#
# The inspect output is captured first and matched with a here-string rather
# than piped into `grep -q`: grep exits at the first match, and for a name in an
# early section (file artifacts precede services) that kills the still-writing
# kurtosis with SIGPIPE — under `set -o pipefail` the pipeline then reports
# failure and an existing artifact reads as missing. The bug only shows for
# early matches, which makes it look like the artifact is the problem.
enclave_has() {
    local text
    text=$(enclave_inspect 2>/dev/null) || return 1
    grep -qE "(^|[[:space:]])$1([[:space:]]|$)" <<<"$text"
}

service_exists() {
    enclave_has "$1"
}

# Same table, different section — file artifacts are listed by name too.
artifact_exists() {
    enclave_has "$1"
}

# Kurtosis refuses to store a file artifact under a name the enclave already
# holds, and offers no way to delete one, so a retry (or --reset) in the same
# enclave must claim fresh names. Find the lowest free suffix; the package
# appends it to every artifact it stores.
next_artifact_suffix() {
    local suffix="" n=1
    while artifact_exists "base-devnet-configs$suffix"; do
        n=$((n + 1))
        suffix="-$n"
    done
    printf '%s\n' "$suffix"
}

# The suffix of the artifacts the running deployment actually uses (the highest
# one present), for the reuse path.
current_artifact_suffix() {
    local suffix="" n=1 last="" found=false
    while artifact_exists "base-devnet-configs$suffix"; do
        last=$suffix
        found=true
        n=$((n + 1))
        suffix="-$n"
    done
    [ "$found" = true ] || return 1
    printf '%s\n' "$last"
}

# port_url <service> <port-id> <scheme> — a published endpoint with a scheme.
#
# `kurtosis port print` prefixes the scheme only for ports that declare an
# application_protocol; the ethereum-package geth rpc/ws ports do not, so they
# come back as a bare host:port. curl and cast accept that, which hides the
# problem all the way to the handoff — the attestor then refuses the config with
# `not a valid URL: first path segment in URL cannot contain colon`. Strip any
# scheme and re-add the wanted one, so this is right either way.
port_url() {
    local raw
    raw=$(kurtosis port print "$ENCLAVE" "$1" "$2") || return 1
    printf '%s://%s\n' "$3" "${raw#*://}"
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

# --------------------------------------------------------- enclave actions ---
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
        enclave_inspect | grep -E 'base-(bootnode|builder|batcher|client)' || {
            log "no Base services found in enclave $ENCLAVE"
            exit 1
        }
        exit 0
        ;;
    stop)
        found=false
        for service in $BASE_SERVICES; do
            if service_exists "$service"; then
                log "stopping $service in enclave $ENCLAVE"
                kurtosis service stop "$ENCLAVE" "$service"
                found=true
            fi
        done
        [ "$found" = true ] || fail "no Base services found in enclave $ENCLAVE"
        exit 0
        ;;
esac

mkdir -p "$RUN_DIR"
RUN_DIR=$(cd "$RUN_DIR" && pwd)

if [ "$RESET" = true ]; then
    log "resetting only the Base services; the shared L1 and other rollups are preserved"
    for service in $BASE_SERVICES; do
        if service_exists "$service"; then
            kurtosis service rm "$ENCLAVE" "$service"
        fi
    done
    rm -f "$RUN_DIR/attestor.env"
fi

# ------------------------------------------------------------------ images ---
# The package references the base/base images by tag; Kurtosis uses a locally
# built image when one exists (image_download defaults to "missing"), so the
# images are built here — from a PINNED CLONE of base/base, the same clone+patch
# recipe run_eth_node.sh and run_optimism_node.sh use for their packages. The
# devnet is therefore reproducible from this repo alone: no sibling checkout is
# required, and an unrelated working-tree state in someone's own base/base
# clone cannot change what runs here.
#
# Docker is driven directly rather than through the base repo's `just` recipes:
# those are shebang recipes, which die with EACCES on WSL2 (XDG_RUNTIME_DIR is
# mounted noexec).
image_exists() {
    docker image inspect "$1" >/dev/null 2>&1
}

missing_images=""
for image in "$NODE_IMAGE" "$BATCHER_IMAGE" "$SETUP_IMAGE"; do
    image_exists "$image" || missing_images="$missing_images $image"
done
[ "$REBUILD_IMAGES" = "1" ] && missing_images=" $NODE_IMAGE $BATCHER_IMAGE $SETUP_IMAGE"

if [ -n "$missing_images" ]; then
    if [ "$SKIP_IMAGE_BUILD" = "1" ]; then
        fail "SKIP_IMAGE_BUILD=1 but these images are missing:$missing_images"
    fi

    if [ -n "${BASE_REPO:-}" ]; then
        # Explicit override: build from an existing checkout as-is (no clone, no
        # patching, no ref check).
        [ -f "$BASE_REPO/etc/docker/docker-bake.hcl" ] ||
            fail "BASE_REPO=$BASE_REPO is not a base/base checkout (etc/docker/docker-bake.hcl missing)"
        BASE_SRC=$(cd "$BASE_REPO" && pwd)
        log "using the BASE_REPO override at $BASE_SRC (pinned clone skipped)"
    else
        require_command git
        require_command python3
        BASE_SRC=$RUN_DIR/base
        if [ ! -e "$BASE_SRC/.pinned-ref" ] || [ "$(cat "$BASE_SRC/.pinned-ref")" != "$BASE_REF" ]; then
            log "cloning base/base@$BASE_REF into $BASE_SRC"
            rm -rf "$BASE_SRC"
            mkdir -p "$BASE_SRC"
            git -C "$BASE_SRC" init --quiet
            git -C "$BASE_SRC" fetch --quiet --depth 1 "$BASE_GIT_URL" "$BASE_REF"
            git -C "$BASE_SRC" checkout --quiet FETCH_HEAD
            echo "$BASE_REF" >"$BASE_SRC/.pinned-ref"
        fi

        # WSL2: timesyncd steps the clock backwards under heavy build load, and
        # the vendored openssl-src's mtime-sensitive `make depend` then aborts
        # with "modification time N s in the future" on every run. Link the
        # system OpenSSL instead (libssl-dev is installed one line above, and
        # the runtime images are the same Debian release, so dynamic linking is
        # safe). Portable, idempotent edit — no sed -i.
        RUST_DOCKERFILE=$BASE_SRC/etc/docker/Dockerfile.rust-services
        if ! grep -q 'OPENSSL_NO_VENDOR' "$RUST_DOCKERFILE"; then
            log "patching Dockerfile.rust-services (OPENSSL_NO_VENDOR=1 for WSL2 clock skew)"
            python3 - "$RUST_DOCKERFILE" <<'PYEOF'
import sys

path = sys.argv[1]
src = open(path).read()
anchor = "rm -rf /var/lib/apt/lists/*\n"
index = src.find(anchor)
if index < 0:
    sys.exit("anchor for the OPENSSL_NO_VENDOR patch not found in " + path)
index += len(anchor)
src = src[:index] + "ENV OPENSSL_NO_VENDOR=1\n" + src[index:]
open(path, "w").write(src)
PYEOF
        fi
    fi

    case "$(uname -m)" in
        x86_64) platform_pair=linux-amd64 ;;
        arm64|aarch64) platform_pair=linux-arm64 ;;
        *) fail "unsupported architecture: $(uname -m)" ;;
    esac

    log "building base/base images:$missing_images (from $BASE_SRC; the first build compiles the Rust workspace — be patient)"
    if [ "$REBUILD_IMAGES" = "1" ] || ! image_exists "$SETUP_IMAGE"; then
        docker build -t "$SETUP_IMAGE" -f "$BASE_SRC/etc/docker/Dockerfile.devnet" "$BASE_SRC"
    fi
    bake_targets=""
    if [ "$REBUILD_IMAGES" = "1" ]; then
        bake_targets="base batcher"
    else
        image_exists "$NODE_IMAGE" || bake_targets="$bake_targets base"
        image_exists "$BATCHER_IMAGE" || bake_targets="$bake_targets batcher"
    fi
    if [ -n "$bake_targets" ]; then
        # Mirrors `just build-image <target> dev` in the base repo: dev profile,
        # no SP1 ELF requirement (the ZK prover pipeline is not part of this
        # stack).
        (cd "$BASE_SRC" && PROFILE=dev ZK_HOST_PROFILE=release \
            BASE_SUCCINCT_ELF_REQUIRE=0 PLATFORM_PAIR="$platform_pair" \
            docker buildx bake -f etc/docker/docker-bake.hcl $bake_targets --load)
    fi
    for image in "$NODE_IMAGE" "$BATCHER_IMAGE" "$SETUP_IMAGE"; do
        image_exists "$image" || fail "image $image still missing after the build"
    done
fi

# ------------------------------------------------------------- shared L1 ---
# Internal URLs are handed to the package; published URLs go to the host-side
# handoff and to cast.
L1_SVC=$(enclave_inspect | grep -oE 'el-1-[a-zA-Z0-9-]+' | head -1)
CL_SVC=$(enclave_inspect | grep -oE 'cl-1-[a-zA-Z0-9-]+' | head -1)
[ -n "$L1_SVC" ] || fail "no ethereum-package execution service found in enclave $ENCLAVE"
[ -n "$CL_SVC" ] || fail "no ethereum-package beacon service found in enclave $ENCLAVE"

L1_RPC_URL=$(port_url "$L1_SVC" rpc http)
L1_WS_URL=$(port_url "$L1_SVC" ws ws)
L1_BEACON_API_URL=$(port_url "$CL_SVC" http http)
INTERNAL_L1_RPC_URL="http://$L1_SVC:8545"
INTERNAL_L1_BEACON_URL="http://$CL_SVC:4000"

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

# A single-validator 2s-slot devnet cannot survive the host pausing (WSL2 VM
# suspend, laptop sleep): on resume the wall clock has moved thousands of slots
# but the chain has not, and with no peers to sync from lighthouse produces every
# block at an already-stale slot and never catches up. geth still looks healthy —
# it happily builds payloads nobody ever seals — so without this check the run
# proceeds and dies ~10 minutes later inside op-deployer with a wall of
# "context deadline exceeded". Detect the dead L1 up front and say what it is.
l1_head_advances() {
    local before after
    before=$(rpc_result "$L1_RPC_URL" eth_blockNumber) || return 1
    sleep 8
    after=$(rpc_result "$L1_RPC_URL" eth_blockNumber) || return 1
    [ "$(hex_to_decimal "$after")" -gt "$(hex_to_decimal "$before")" ]
}

beacon_sync_distance() {
    curl --fail --silent "$L1_BEACON_API_URL/eth/v1/node/syncing" |
        jq -r '.data.sync_distance // "unknown"'
}

log "checking the shared Ethereum L1 in enclave $ENCLAVE"
wait_until "$STARTUP_WAIT_SECS" "shared L1 execution RPC" l1_ready
wait_until "$STARTUP_WAIT_SECS" "shared L1 beacon API" beacon_ready
if ! l1_head_advances; then
    log "the L1 in enclave $ENCLAVE is not producing blocks"
    log "  execution head:       $(hex_to_decimal "$(rpc_result "$L1_RPC_URL" eth_blockNumber)")"
    log "  beacon sync distance: $(beacon_sync_distance) slots behind the wall clock"
    log "A large sync distance means the host was paused (VM suspend / sleep) while"
    log "the chain was running. A one-validator devnet cannot catch up from that —"
    log "there are no peers to sync from. Recreate the enclave:"
    log "  kurtosis enclave rm -f $ENCLAVE && $0"
    fail "shared L1 has stalled"
fi
wait_until "$FINALITY_WAIT_SECS" "non-genesis finalized shared-L1 block" l1_finalized
L1_CHAIN_ID=$(hex_to_decimal "$(rpc_result "$L1_RPC_URL" eth_chainId)")
# The Base node needs the L1 slot duration (its bundled L1 uses 4s, this one 2s).
L1_SLOT_DURATION=${L1_SLOT_DURATION:-$(curl --fail --silent "$L1_BEACON_API_URL/eth/v1/config/spec" |
    jq -r '.data.SECONDS_PER_SLOT // empty' || true)}
[[ "$L1_SLOT_DURATION" =~ ^[0-9]+$ ]] || L1_SLOT_DURATION=2

[ "$(cast wallet address --private-key "$BASE_DEPLOYER_PRIVATE_KEY")" = "$BASE_DEPLOYER_ADDRESS" ] ||
    fail "BASE_DEPLOYER_PRIVATE_KEY does not match BASE_DEPLOYER_ADDRESS"

# --------------------------------------------------------------- funding ---
# op-deployer (setup-l2.sh, inside the enclave) deploys from BASE_DEPLOYER; the
# batcher pays for every batch. The ethereum-package L1 does not prefund the
# anvil accounts base/base uses, so top them up here. A quiet devnet L1 decays
# to a few wei of basefee, which makes cast compute maxFee < priorityFee — send
# legacy with an explicit gas price, as the Arbitrum package does.
l1_account_is_funded() {
    local balance
    balance=$(cast balance "$1" --rpc-url "$L1_RPC_URL" --ether) || return 1
    awk -v balance="$balance" 'BEGIN { exit !(balance >= 100) }'
}

fund_l1_account() {
    local address=$1
    l1_account_is_funded "$address" && return 0
    log "funding $address on the shared L1"
    cast send "$address" \
        --rpc-url "$L1_RPC_URL" \
        --private-key "$L1_FUNDER_PRIVATE_KEY" \
        --legacy \
        --gas-price 2gwei \
        --value 1000ether >/dev/null
}

for address in \
    "$BASE_DEPLOYER_ADDRESS" \
    "$BASE_SEQUENCER_ADDRESS" \
    "$BASE_BATCHER_ADDRESS" \
    "$BASE_PROPOSER_ADDRESS" \
    "$BASE_CHALLENGER_ADDRESS"; do
    fund_l1_account "$address"
done

# ------------------------------------------------------------- package run ---
# A previous run (including one that failed part-way) leaves its file artifacts
# behind, and Kurtosis cannot overwrite or delete them — so claim the next free
# suffix instead of colliding.
if service_exists base-client; then
    ARTIFACT_SUFFIX=$(current_artifact_suffix) ||
        fail "base-client exists in $ENCLAVE but its config artifact does not; run --reset"
else
    ARTIFACT_SUFFIX=$(next_artifact_suffix)
    [ -z "$ARTIFACT_SUFFIX" ] ||
        log "existing Base artifacts found; this deployment stores its own as base-devnet-configs$ARTIFACT_SUFFIX"
fi

PACKAGE_ARGS=$RUN_DIR/kurtosis-args.json
jq -n \
    --arg artifact_suffix "$ARTIFACT_SUFFIX" \
    --argjson l1_chain_id "$L1_CHAIN_ID" \
    --arg l1_rpc_url "$INTERNAL_L1_RPC_URL" \
    --arg l1_beacon_url "$INTERNAL_L1_BEACON_URL" \
    --argjson l1_slot_duration "$L1_SLOT_DURATION" \
    --argjson l2_chain_id "$L2_CHAIN_ID" \
    --arg node_image "$NODE_IMAGE" \
    --arg batcher_image "$BATCHER_IMAGE" \
    --arg setup_image "$SETUP_IMAGE" \
    --arg deployer_address "$BASE_DEPLOYER_ADDRESS" \
    --arg deployer_private_key "$BASE_DEPLOYER_PRIVATE_KEY" \
    --arg sequencer_address "$BASE_SEQUENCER_ADDRESS" \
    --arg batcher_address "$BASE_BATCHER_ADDRESS" \
    --arg proposer_address "$BASE_PROPOSER_ADDRESS" \
    --arg challenger_address "$BASE_CHALLENGER_ADDRESS" \
    '{
        base_package: {
            artifact_suffix: $artifact_suffix,
            l1_chain_id: $l1_chain_id,
            l1_rpc_url: $l1_rpc_url,
            l1_beacon_url: $l1_beacon_url,
            l1_slot_duration: $l1_slot_duration,
            l2_chain_id: $l2_chain_id,
            node_image: $node_image,
            batcher_image: $batcher_image,
            setup_image: $setup_image,
            deployer_address: $deployer_address,
            deployer_private_key: $deployer_private_key,
            sequencer_address: $sequencer_address,
            batcher_address: $batcher_address,
            proposer_address: $proposer_address,
            challenger_address: $challenger_address
        }
    }' >"$PACKAGE_ARGS"
chmod 600 "$PACKAGE_ARGS"

if service_exists base-client; then
    log "reusing the existing Base deployment and starting its services if stopped"
    for service in base-bootnode base-builder base-batcher base-client; do
        kurtosis service start "$ENCLAVE" "$service" >/dev/null 2>&1 || true
    done
else
    log "deploying the Base stack into enclave $ENCLAVE (op-deployer runs live against the shared L1)"
    kurtosis run --enclave "$ENCLAVE" "$PACKAGE_DIR" --args-file "$PACKAGE_ARGS"

    # This is a brand-new chain from block 0, so anything the attestor recorded
    # for the previous one is both stale and wrong (its roots are keyed by L2
    # height, and those heights now belong to different blocks).
    STALE_STATE=$ATTESTOR_RUN_DIR/$ATTESTOR_SRC_CHAIN.attested-roots.json
    if [ -f "$STALE_STATE" ]; then
        log "discarding attestor state from the previous chain ($STALE_STATE)"
        rm -f "$STALE_STATE"
    fi
fi

# -------------------------------------------------------------- discovery ---
# Two L2 execution endpoints, with distinct jobs — the same split
# run_optimism_node.sh makes between the sequencer's op-geth and the replica's:
#   L2_RPC_URL          the SEQUENCER (base-builder). Transactions go here: it is
#                       the canonical block producer, so sends land and receipts
#                       appear immediately. deploy_l2_contracts.sh sends here.
#   L2_FOLLOWER_RPC_URL the independent follower (base-client), whose consensus
#                       RPC (OP_NODE_RPC_URL) is what the attestor reads. The
#                       relayer is patched to this endpoint so its proof reads and
#                       event reads come from the same node. Writes are forwarded
#                       to the sequencer; if the follower is catching up, receipt
#                       visibility can lag accordingly.
L2_RPC_URL=$(port_url base-builder rpc http)
L2_WS_URL=$(port_url base-builder ws ws)
L2_FOLLOWER_RPC_URL=$(port_url base-client rpc http)
L2_FOLLOWER_WS_URL=$(port_url base-client ws ws)
OP_NODE_RPC_URL=$(port_url base-client cl-rpc http)

l2_ready() {
    rpc_result "$L2_RPC_URL" eth_chainId
}

follower_ready() {
    rpc_result "$L2_FOLLOWER_RPC_URL" eth_chainId
}

log "waiting for the Base sequencer RPC at $L2_RPC_URL"
wait_until "$STARTUP_WAIT_SECS" "Base sequencer RPC" l2_ready
log "waiting for the Base follower RPC at $L2_FOLLOWER_RPC_URL"
wait_until "$STARTUP_WAIT_SECS" "Base follower RPC" follower_ready
OBSERVED_L2_CHAIN_ID=$(hex_to_decimal "$(rpc_result "$L2_RPC_URL" eth_chainId)")
[ "$OBSERVED_L2_CHAIN_ID" -eq "$L2_CHAIN_ID" ] ||
    fail "Base L2 chain ID is $OBSERVED_L2_CHAIN_ID, expected $L2_CHAIN_ID"

sync_status() {
    curl --fail --silent -X POST -H 'Content-Type: application/json' \
        --data '{"jsonrpc":"2.0","id":1,"method":"optimism_syncStatus","params":[]}' \
        "$OP_NODE_RPC_URL"
}

unsafe_head() { sync_status | jq -e '.result.unsafe_l2.number >= 1'; }
log "waiting for base-client to answer optimism_syncStatus with unsafe head >= 1 at $OP_NODE_RPC_URL"
wait_until "$STARTUP_WAIT_SECS" "base-client unsafe head >= 1" unsafe_head

# The attestor's low-latency profile attests the unsafe head immediately, but
# provisional roots only confirm once the finalized head covers them — wait for
# L2 finality once so the full pipeline is known to work, warn otherwise.
finalized_head() { sync_status | jq -e '.result.finalized_l2.number >= 1'; }
log "waiting for the first finalized L2 block (max ${FINALIZED_WAIT_SECS}s)"
if ! wait_until "$FINALIZED_WAIT_SECS" "first finalized L2 block" finalized_head; then
    log "WARNING: no finalized L2 block yet — derived roots will stay provisional until finality arrives"
    log "check: kurtosis service logs $ENCLAVE base-batcher"
fi

# L1 contract addresses from the package's config artifact (op-deployer inspect
# l1 output). Keys are matched case-insensitively so an op-deployer bump that
# changes casing/nesting does not break discovery.
CONFIG_DIR=$RUN_DIR/configs
rm -rf "$CONFIG_DIR"
kurtosis files download "$ENCLAVE" "base-devnet-configs$ARTIFACT_SUFFIX" "$CONFIG_DIR"
ADDR_JSON=$(find "$CONFIG_DIR" -name 'l1-addresses.json' -type f | head -1)
[ -n "$ADDR_JSON" ] || fail "l1-addresses.json not found in the base-devnet-configs artifact"

addr_by_key() {
    jq -r --arg re "$1" \
        '[.. | objects | to_entries[] | select(.key | test($re; "i")) | .value] | first // empty' \
        "$ADDR_JSON"
}
DISPUTE_GAME_FACTORY=$(addr_by_key 'disputeGameFactoryProxy')
OPTIMISM_PORTAL=$(addr_by_key 'optimismPortalProxy')
if [ -z "$DISPUTE_GAME_FACTORY" ] || [ -z "$OPTIMISM_PORTAL" ]; then
    log "ERROR: DisputeGameFactoryProxy/OptimismPortalProxy not found in $ADDR_JSON — keys present:"
    jq '[.. | objects | keys[]] | unique' "$ADDR_JSON"
    exit 1
fi
# --gas-limit: cast defaults an eth_call to 50M gas, above the 16.7M --rpc.gascap
# the ethereum-package geth runs with, which rejects the call outright.
RESPECTED_GAME_TYPE=$(cast call "$OPTIMISM_PORTAL" 'respectedGameType()(uint32)' \
    --gas-limit 200000 --rpc-url "$L1_RPC_URL" | awk '{print $1}')

# scripts/local/deploy_l2_contracts.sh deploys from the well-known devnet key
# 0x8943…, which op-deployer does NOT prefund here (see L2_FUNDER_ADDRESS), so
# top it up from a genesis-funded account before handing over.
l2_account_is_funded() {
    local balance
    balance=$(cast balance "$1" --rpc-url "$L2_RPC_URL" --ether) || return 1
    awk -v balance="$balance" 'BEGIN { exit !(balance >= 1) }'
}
if ! l2_account_is_funded "$L2_DEPLOYER_ADDRESS"; then
    [ "$(cast wallet address --private-key "$L2_FUNDER_PRIVATE_KEY")" = "$L2_FUNDER_ADDRESS" ] ||
        fail "L2_FUNDER_PRIVATE_KEY does not match L2_FUNDER_ADDRESS"
    l2_account_is_funded "$L2_FUNDER_ADDRESS" ||
        fail "L2 funder $L2_FUNDER_ADDRESS has no balance on the L2 — op-deployer's fundDevAccounts did not prefund it; pick a funder from the genesis alloc (.base-devnet-run/configs/l2/genesis.json)"
    log "funding the standard L2 contract deployer $L2_DEPLOYER_ADDRESS from $L2_FUNDER_ADDRESS"
    cast send "$L2_DEPLOYER_ADDRESS" \
        --rpc-url "$L2_RPC_URL" \
        --private-key "$L2_FUNDER_PRIVATE_KEY" \
        --value 100ether >/dev/null
    wait_until "$STARTUP_WAIT_SECS" "L2 contract deployer funding" \
        l2_account_is_funded "$L2_DEPLOYER_ADDRESS"
fi

# ---------------------------------------------------------------- handoff ---
ENV_FILE=$RUN_DIR/attestor.env
{
    printf 'export L1_RPC_URL=%q\n' "$L1_RPC_URL"
    printf 'export L1_WS_URL=%q\n' "$L1_WS_URL"
    printf 'export L1_BEACON_URL=%q\n' "$L1_BEACON_API_URL"
    printf 'export ETH_BEACON_API=%q\n' "$L1_BEACON_API_URL"
    printf 'export L1_CHAIN_ID=%q\n' "$L1_CHAIN_ID"
    printf 'export OP_NODE_RPC_URL=%q\n' "$OP_NODE_RPC_URL"
    printf 'export L2_RPC_URL=%q\n' "$L2_RPC_URL"
    printf 'export L2_WS_URL=%q\n' "$L2_WS_URL"
    printf 'export L2_CHAIN_ID=%q\n' "$OBSERVED_L2_CHAIN_ID"
    printf 'export ATTESTOR_SIGNING_KEY=%q\n' "$ATTESTOR_SIGNING_KEY"
    printf 'export ATTESTOR_PUBLIC_KEY=%q\n' "$ATTESTOR_PUBLIC_KEY"
    printf 'export L2_SEQUENCER_RPC_URL=%q\n' "$L2_RPC_URL"
    printf 'export L2_FOLLOWER_RPC_URL=%q\n' "$L2_FOLLOWER_RPC_URL"
    printf 'export L2_FOLLOWER_WS_URL=%q\n' "$L2_FOLLOWER_WS_URL"
    printf 'export DISPUTE_GAME_FACTORY=%q\n' "$DISPUTE_GAME_FACTORY"
    printf 'export OPTIMISM_PORTAL=%q\n' "$OPTIMISM_PORTAL"
    printf 'export RESPECTED_GAME_TYPE=%q\n' "$RESPECTED_GAME_TYPE"
    printf 'export SRC_CHAIN=%q\n' basedev
    # Low-latency profile (attestor/optimism/GUIDE.md "fastest safe profile"):
    # attest unsafe-head roots ~seconds after block production; roots stay
    # provisional until the finalized recheck confirms them (consumers pass
    # include_provisional=true for the fresh frontier).
    printf 'export ATTESTATION_HEAD=%q\n' unsafe
    printf 'export POLL_INTERVAL_SECONDS=%q\n' 2
    printf 'export DERIVED_GAP_BLOCKS=%q\n' 5
    printf 'export LOOKBACK_BLOCKS=%q\n' 1000
    # NOT the run_op_attestor.sh defaults (3000/3001): a Base attestor must be
    # able to coexist with an OP-devnet one (which owns those ports and
    # .op-attestor-run).
    printf 'export METRICS_PORT=%q\n' 3100
    printf 'export GRPC_PORT=%q\n' 3101
    # Deliberately NOT `export RUN_DIR`. run_op_attestor.sh honours RUN_DIR too,
    # but so does this script — exporting it into the caller's shell means the
    # next `run_base_node.sh` in that same shell writes its clone, artifacts and
    # handoff into the ATTESTOR's directory. The attestor scripts read
    # ATTESTOR_RUN_DIR first, so sourcing this file is enough.
    printf 'export ATTESTOR_RUN_DIR=%q\n' "$ATTESTOR_RUN_DIR"
} >"$ENV_FILE"
chmod 600 "$ENV_FILE"

log "local Base stack is ready on the shared L1"
cat <<EOF
Kurtosis enclave:       $ENCLAVE
L1 execution RPC:       $L1_RPC_URL
L1 beacon API:          $L1_BEACON_API_URL
L1 chain ID:            $L1_CHAIN_ID
L2 sequencer RPC:       $L2_RPC_URL   (base-builder — send transactions here)
L2 sequencer WS:        $L2_WS_URL
L2 follower RPC:        $L2_FOLLOWER_RPC_URL   (base-client)
L2 consensus RPC:       $OP_NODE_RPC_URL   (base-client, op-node namespace)
L2 chain ID:            $OBSERVED_L2_CHAIN_ID
Funded L2 deployer:     $L2_DEPLOYER_ADDRESS
DisputeGameFactory:     $DISPUTE_GAME_FACTORY
OptimismPortal:         $OPTIMISM_PORTAL
respectedGameType:      $RESPECTED_GAME_TYPE (no games are posted here — derived-root path only)
Attestor environment:   $ENV_FILE

Run the OP Stack attestor against it:
  source "$ENV_FILE"
  scripts/local/run_op_attestor.sh

(The explicit 'source' matters: a bare run_op_attestor.sh auto-attaches to the
OP devnet's .op-devnet-run/attestor.env, not this one.)

Deploy the L2 IBC contracts onto Base:
  L2_ENV_FILE="$ENV_FILE" scripts/local/deploy_l2_contracts.sh

Inspect the devnet:
  kurtosis enclave inspect $ENCLAVE
  kurtosis service logs $ENCLAVE base-client

Stop only Base while preserving the shared L1:
  scripts/local/run_base_node.sh --stop

Redeploy only Base on the existing L1:
  scripts/local/run_base_node.sh --reset

Remove the complete shared L1 + all attached rollups:
  kurtosis enclave rm -f "$ENCLAVE"
EOF
