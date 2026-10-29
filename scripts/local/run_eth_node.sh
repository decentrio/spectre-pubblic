#!/bin/sh

set -eux

# Local Ethereum PoS L1 (geth + lighthouse, Fulu-from-genesis) via Kurtosis. Owns
# ONLY the L1 node: clones+patches the ethereum-package, starts the enclave,
# discovers the RPC/WS/beacon endpoints, and writes them to a handoff file. Contract
# deployment lives in scripts/local/deploy_eth_contracts.sh (which sources that file).
#
#   run_eth_node.sh          -> $RUN_DIR/eth.env  (ETH_RPC / ETH_WS / ETH_BEACON_API)
#   deploy_eth_contracts.sh  -> deploys E2ETestDeploy, patches relayer config
#
# scripts/local/run_optimism_node.sh reuses this to bring up the OP Stack's L1 in the
# op-devnet enclave (same package, same Fulu recipe) before layering its L2 on top.
#
# The ethereum-package is cloned+patched (not run straight from github) for two
# reasons: (1) the pinned version still passes --allow-insecure-unlock, which geth
# v1.17 removed; (2) a local clone is its own kurtosis package identity, so when the
# OP flow runs optimism-package in the same enclave it does NOT clobber the copy
# optimism-package imports Starlark helpers from (kurtosis caches by package NAME).
#
# Env (defaults in parens):
#   ENCLAVE (my-testnet)                 kurtosis enclave name
#   ETH_PIN (Fulu-validated ref below)   ethereum-package git ref; run_optimism_node
#                                        passes the exact ref optimism-package pins
#   L1_PARAMS (eth-network-params.yaml)  --args-file for the L1
#   RUN_DIR ($REPO_ROOT/.eth-devnet-run) clone + handoff dir
#   SKIP_COSMOS_RESET (unset)            when set, do NOT kill gaiad / wipe ~/.gaia
#   FORCE_RECREATE (unset)               when set, destroy and rebuild the L1 even if
#                                        its enclave is already up
#
# The L1 is SHARED: an existing enclave is reused, not rebuilt. Each rollup script
# (run_optimism_node.sh, run_arbitrum_node.sh, ...) calls this to guarantee an L1
# exists in its enclave, so bringing up a second rollup must not destroy the first
# one's L2 services, its contracts, or the L1 they are anchored to. Reuse still
# re-discovers the endpoints and rewrites the handoff, because callers source it
# unconditionally.
cd "$(dirname "$0")/../.."
REPO_ROOT=$PWD

ENCLAVE=${ENCLAVE:-my-testnet}
# ethereum-package the optimism-package pins (Fulu-era, lighthouse-v8 genesis) —
# used everywhere so the ETH<->Cosmos and OP devnets share one validated L1.
ETH_PIN=${ETH_PIN:-e964e305a19d56b798800e84d264f97b87952c55}
L1_PARAMS=${L1_PARAMS:-eth-network-params.yaml}
RUN_DIR=${RUN_DIR:-$REPO_ROOT/.eth-devnet-run}

log() { printf '\n[run_eth_node] %s\n' "$*"; }

# The Cosmos reset is deliberately INDEPENDENT of enclave reuse: one wipes a local
# gaiad home, the other manages a Kurtosis enclave, and coupling them would make
# "redo the Cosmos side against the L1 that is already running" impossible to ask
# for. Reusing the enclave therefore still wipes ~/.gaia unless SKIP_COSMOS_RESET
# is set, exactly as before.
#
# It runs BEFORE the enclave decision now (it used to follow `kurtosis enclave rm`).
# That is deliberate and has no functional effect — neither touches the other's
# state — but it keeps the unconditional work in one place and everything that
# depends on whether the enclave already exists in the block below.
if [ -z "${SKIP_COSMOS_RESET:-}" ]; then
    killall gaiad || true
    rm -rf "$HOME/.gaia"
fi

mkdir -p "$RUN_DIR"
ETH_DIR=$RUN_DIR/ethereum-package

REUSE=0
if [ -z "${FORCE_RECREATE:-}" ] && kurtosis enclave inspect "$ENCLAVE" >/dev/null 2>&1; then
    REUSE=1
fi

if [ "$REUSE" = 1 ]; then
    log "enclave '$ENCLAVE' is already up — reusing its L1 (set FORCE_RECREATE=1 to rebuild)"
    # The running L1 was built from whatever ref was pinned at the time. Reuse cannot
    # change that, so say so rather than letting the requested pin imply otherwise —
    # run_optimism_node.sh passes the exact ref optimism-package expects.
    if [ -e "$ETH_DIR/.pinned-ref" ] && [ "$(cat "$ETH_DIR/.pinned-ref")" != "$ETH_PIN" ]; then
        log "WARNING: running L1 was built from ethereum-package@$(cat "$ETH_DIR/.pinned-ref"), \
requested @$ETH_PIN; re-run with FORCE_RECREATE=1 if that difference matters"
    fi
else
    kurtosis enclave rm -f "$ENCLAVE" || true
fi

# ----------------------------------- package clone + patch, then run the L1 ---
# Skipped entirely on reuse: the clone exists only to feed `kurtosis run`, and the
# L1 it would start is already running.
if [ "$REUSE" = 0 ]; then
    if [ ! -e "$ETH_DIR/.pinned-ref" ] || [ "$(cat "$ETH_DIR/.pinned-ref")" != "$ETH_PIN" ]; then
        log "cloning ethpandaops/ethereum-package@$ETH_PIN into $ETH_DIR"
        rm -rf "$ETH_DIR"
        mkdir -p "$ETH_DIR"
        git -C "$ETH_DIR" init --quiet
        git -C "$ETH_DIR" fetch --quiet --depth 1 \
            https://github.com/ethpandaops/ethereum-package "$ETH_PIN"
        git -C "$ETH_DIR" checkout --quiet FETCH_HEAD
        echo "$ETH_PIN" > "$ETH_DIR/.pinned-ref"
    fi
    # Drop --allow-insecure-unlock (removed in geth v1.17). Portable edit (no sed -i,
    # which differs on BSD/macOS vs GNU).
    GETH_STAR=$ETH_DIR/src/el/geth/geth_launcher.star
    if grep -q -- '--allow-insecure-unlock' "$GETH_STAR"; then
        log "patching geth_launcher.star (drop --allow-insecure-unlock; removed in geth v1.17)"
        grep -v -- '--allow-insecure-unlock' "$GETH_STAR" > "$GETH_STAR.tmp"
        mv "$GETH_STAR.tmp" "$GETH_STAR"
    fi

    log "starting L1 (enclave $ENCLAVE, package $ETH_DIR) — first run pulls images, be patient"
    kurtosis run --enclave "$ENCLAVE" "$ETH_DIR" --args-file "$L1_PARAMS"

    sleep 30
fi

ETH_RPC=$(kurtosis enclave inspect "$ENCLAVE" \
| perl -ne '
  if (/el-1-geth-lighthouse/) { $in=1 }
  elsif ($in && /^\S/) { $in=0 }
  elsif ($in && /^\s+rpc:.*->\s+(127\.0\.0\.1:\d+)/) {
    print "http://$1\n";
    exit;
  }
')

ETH_WS=$(kurtosis enclave inspect "$ENCLAVE" \
| perl -ne '
  if (/el-1-geth-lighthouse/) { $in=1 }
  elsif ($in && /^\S/) { $in=0 }
  elsif ($in && /^\s+ws:.*->\s+(127\.0\.0\.1:\d+)/) {
    print "ws://$1\n";
    exit;
  }
')

ETH_BEACON_API=$(kurtosis enclave inspect "$ENCLAVE" \
| awk '
  $0 ~ /cl-1-lighthouse-geth/ {in_service=1}
  in_service && /http:/ {
      match($0, /127\.0\.0\.1:[0-9]+/)
      print "http://" substr($0, RSTART, RLENGTH)
      exit
  }
  in_service && /^[^[:space:]]/ {in_service=0}
')

echo "ETH_RPC: $ETH_RPC"
echo "ETH_WS: $ETH_WS"
echo "ETH_BEACON_API: $ETH_BEACON_API"

# An enclave can exist with its L1 services gone (a partial teardown, a crashed
# container). Reuse would then write an EMPTY handoff and every caller would fail
# somewhere far from the cause, so refuse here and name the way out.
if [ -z "$ETH_RPC" ] || [ -z "$ETH_WS" ] || [ -z "$ETH_BEACON_API" ]; then
    log "ERROR: enclave '$ENCLAVE' exists but its L1 endpoints could not be discovered."
    log "Re-run with FORCE_RECREATE=1 to rebuild the L1 in that enclave."
    exit 1
fi

# Discovering a port mapping is not the same as the node being alive: kurtosis still
# reports the mapping for a container that exists but is stopped (a crashed process
# that did not tear its container down, stale state after a reboot). Reuse would then
# hand a dead endpoint to every caller. Only the reuse path needs this — the fresh
# path just ran `kurtosis run` and waited on it.
if [ "$REUSE" = 1 ]; then
    probe=0
    for attempt in 1 2 3; do
        if curl -sf -m 5 -X POST -H 'Content-Type: application/json' \
            --data '{"jsonrpc":"2.0","id":1,"method":"eth_blockNumber","params":[]}' \
            "$ETH_RPC" >/dev/null 2>&1; then
            probe=1
            break
        fi
        [ "$attempt" = 3 ] || sleep 2
    done
    if [ "$probe" = 0 ]; then
        log "ERROR: enclave '$ENCLAVE' publishes $ETH_RPC but it does not answer eth_blockNumber."
        log "Its containers are present but not serving. Re-run with FORCE_RECREATE=1 to rebuild."
        exit 1
    fi
fi

# Endpoint handoff for deploy_eth_contracts.sh / run_optimism_node.sh. Rewritten on
# the reuse path too — callers source it unconditionally, and a stale file from an
# earlier enclave points at ports that are no longer mapped.
ENV_FILE=$RUN_DIR/eth.env
{
    printf 'export ETH_RPC=%s\n' "$ETH_RPC"
    printf 'export ETH_WS=%s\n' "$ETH_WS"
    printf 'export ETH_BEACON_API=%s\n' "$ETH_BEACON_API"
} >"$ENV_FILE"

if [ "$REUSE" = 1 ]; then
    echo "Ethereum L1 (enclave $ENCLAVE) REUSED. Endpoints written to $ENV_FILE"
else
    echo "Ethereum L1 (enclave $ENCLAVE) ready. Endpoints written to $ENV_FILE"
fi
echo "Deploy the IBC contracts + patch the relayer config with:"
echo "  scripts/local/deploy_eth_contracts.sh"
