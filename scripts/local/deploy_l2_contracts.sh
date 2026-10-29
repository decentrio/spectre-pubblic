#!/bin/sh

set -euxo pipefail

# Deploys the L2-side receive infrastructure (E2ETestDeployL2) to a running local
# rollup's exec layer and patches the relayer's cosmos_to_l2 module with the
# resulting addresses + L2 endpoint. This is the L2 twin of deploy_eth_contracts.sh:
# same deploy-then-patch shape, but it targets the L2 exec RPC and the cosmos_to_l2
# module (a SpectreClient on the L2), and it runs E2ETestDeployL2 (no ERC20 faucet).
#
# Like the L1 flow it is DEPLOY-ONLY: the SpectreClient itself is created afterwards
# by `relayer create-clients-eth --config <that-path>.json`, which reads a fresh
# Cosmos genesis and writes spectre_client back into the module.
#
# Endpoint source (in priority order):
#   1. L2_RPC in the environment (explicit deploy override)
#   2. L2_ENV_FILE, if set, is sourced and its L2_RPC_URL used
#   3. the stack handoff for DST_CHAIN is sourced automatically:
#        opstack   -> .op-devnet-run/attestor.env       (L2_RPC_URL)
#        arbitrum  -> .arbitrum-devnet-run/attestor.env  (L2_RPC_URL)
#        base      -> .base-devnet-run/attestor.env      (L2_RPC_URL)
#
# If L2_ENV_FILE is set explicitly, it is sourced even when L2_RPC is already set.
# L2_RPC/L2_WS still win for deployment; the unconditional source lets the relayer
# consume extra handoff variables such as Base's follower RPC/WS pair.
#
# The relayer config can use a different execution endpoint from deployment.
# Local Base writes L2_FOLLOWER_RPC_URL/L2_FOLLOWER_WS_URL for base-client, which
# serves historical eth_getProof. The sequencer RPC is still correct for sending
# the deployment transactions, but not for packet membership proofs.
#
# Env:
#   DST_CHAIN (opstack)                 which cosmos_to_l2 module to patch
#                                       (matched on src_chain=cosmos + dst_chain)
#   MODULE_NAME                         patch the module with this `name` instead.
#                                       Needed for Base: it runs the OP Stack, so
#                                       its modules carry dst_chain=opstack and are
#                                       indistinguishable from Optimism's by chain
#                                       alone. Set MODULE_NAME=cosmos-to-base.
#   RELAYER_CONFIG                      relayer config to patch
#                                       (default: relayer/config.json — examples are
#                                       never written to)
#   L2_RPC / L2_ENV_FILE                see endpoint source above
#   L2_WS                               optional deploy L2 exec WS
#   L2_RELAYER_RPC / L2_RELAYER_WS      optional endpoint patched into relayer
#                                       config; defaults to Base follower vars
#                                       when present, otherwise L2_RPC/L2_WS
#   L2_DEPLOYER_PRIVATE_KEY / _ADDRESS  deployer funded on the L2 (defaults to the
#                                       well-known devnet key, prefunded on Kurtosis
#                                       OP + Arbitrum local stacks)
#   PERMIT2                             optional Permit2 address (default: unset)
#
# All internal paths (scripts/E2ETestDeployL2.s.sol, relayer/) are repo-root relative.
cd "$(dirname "$0")/../.."
REPO_ROOT=$PWD

DST_CHAIN=${DST_CHAIN:-opstack}
# An example file is documentation: it must keep working as a thing to copy, which
# it cannot do if a script rewrites it. Default to the live config instead, and say
# so when it is missing rather than silently editing a template nobody runs.
RELAYER_CONFIG=${RELAYER_CONFIG:-$REPO_ROOT/relayer/config.json}
case "$RELAYER_CONFIG" in
    /*) ;;
    *) RELAYER_CONFIG=$REPO_ROOT/$RELAYER_CONFIG ;;
esac
[ -f "$RELAYER_CONFIG" ] || {
    echo "ERROR: relayer config not found: $RELAYER_CONFIG" >&2
    echo "  Copy one of the examples and edit it, then re-run:" >&2
    echo "    cp relayer/config.<path>.example.json relayer/config.json" >&2
    echo "  where <path> is ethereum|op|arbitrum|base — each carries just that" >&2
    echo "  path's module pair (config.example.json is the catalogue of all of them)." >&2
    echo "  Or point RELAYER_CONFIG at the file you want patched." >&2
    exit 1
}

# ---------------------------------------------------------------- endpoint ---
if [ -z "${L2_RPC:-}" ] && [ -z "${L2_ENV_FILE:-}" ]; then
    case "$DST_CHAIN" in
        opstack) L2_ENV_FILE=$REPO_ROOT/.op-devnet-run/attestor.env ;;
        arbitrum) L2_ENV_FILE=$REPO_ROOT/.arbitrum-devnet-run/attestor.env ;;
        base) L2_ENV_FILE=$REPO_ROOT/.base-devnet-run/attestor.env ;;
        *) echo "ERROR: unknown DST_CHAIN=$DST_CHAIN (want opstack|arbitrum|base)" >&2; exit 1 ;;
    esac
fi

if [ -n "${L2_ENV_FILE:-}" ]; then
    [ -f "$L2_ENV_FILE" ] || {
        echo "ERROR: L2_ENV_FILE handoff not found: $L2_ENV_FILE" >&2
        echo "  Run the $DST_CHAIN stack first, or unset L2_ENV_FILE and set L2_RPC/L2_RELAYER_RPC manually." >&2
        exit 1
    }
    # shellcheck disable=SC1090
    . "$L2_ENV_FILE"
fi
L2_RPC=${L2_RPC:-${L2_RPC_URL:-}}
L2_WS=${L2_WS:-${L2_WS_URL:-}}

: "${L2_RPC:?L2_RPC is required (no L2 exec RPC discovered)}"
relayer_rpc_set=0
relayer_ws_set=0
[ -n "${L2_RELAYER_RPC:-}" ] && relayer_rpc_set=1
[ -n "${L2_RELAYER_WS:-}" ] && relayer_ws_set=1

if [ "$relayer_rpc_set" -eq 1 ]; then
    if [ "$relayer_ws_set" -eq 0 ]; then
        echo "WARNING: L2_RELAYER_RPC is set without L2_RELAYER_WS; leaving relayer WS empty instead of mixing endpoints." >&2
        L2_RELAYER_WS=
    fi
elif [ "$relayer_ws_set" -eq 1 ]; then
    echo "ERROR: L2_RELAYER_WS is set without L2_RELAYER_RPC; set both relayer endpoints together." >&2
    exit 1
elif [ -n "${L2_FOLLOWER_RPC_URL:-}" ]; then
    L2_RELAYER_RPC=$L2_FOLLOWER_RPC_URL
    L2_RELAYER_WS=${L2_FOLLOWER_WS_URL:-}
    if [ -z "${L2_FOLLOWER_WS_URL:-}" ] && [ -n "${L2_WS:-}" ]; then
        echo "WARNING: L2_FOLLOWER_RPC_URL is set without L2_FOLLOWER_WS_URL; leaving relayer WS empty instead of using the deploy WS." >&2
    fi
else
    L2_RELAYER_RPC=$L2_RPC
    L2_RELAYER_WS=${L2_WS:-}
fi
: "${L2_RELAYER_RPC:?L2_RELAYER_RPC is required (no relayer L2 exec RPC discovered)}"

# Deployer. E2ETestDeployL2 sets relayers[0] = msg.sender, so whoever deploys receives
# the ICS26Router relayer role — it MUST be the key the relayer runs with
# (relayer/.env ETH_PRIVATE_KEY). A different deployer leaves the relayer unauthorized
# and every updateApplicationState reverts with no reason string, which looks exactly
# like an unfunded signer until you run `cast run` on the tx and see canCall -> false.
# Funding the wrong address does not fix it.
#
# Default is the well-known devnet key, prefunded on both stacks; override for a real L2.
L2_DEPLOYER_ADDRESS=${L2_DEPLOYER_ADDRESS:-0x8943545177806ED17B9F23F0a21ee5948eCaa776}
L2_DEPLOYER_PRIVATE_KEY=${L2_DEPLOYER_PRIVATE_KEY:-bcdf20249abf0ed6d944c0288fad489e33f66b3960d9e6229c1cd214ed3bbe31}

# The two must be the same account. forge resolves msg.sender from --sender, so the
# script's `new SignatureVerifier(msg.sender)` records that address as owner, while
# the transaction itself is signed by --private-key. Overriding only one of them
# deploys a contract owned by somebody else and the very next call fails:
#
#   SignatureVerifier::setBucket(...) -> [Revert] NotOwner()
#
# which reads like a permissions bug in the contract rather than a mismatched flag.
# Overriding just the key is the easy mistake, since the address has a default.
if command -v cast >/dev/null 2>&1; then
    DERIVED_ADDRESS=$(cast wallet address --private-key "$L2_DEPLOYER_PRIVATE_KEY" 2>/dev/null || true)
    if [ -n "$DERIVED_ADDRESS" ] &&
        [ "$(printf '%s' "$DERIVED_ADDRESS" | tr 'A-Z' 'a-z')" != "$(printf '%s' "$L2_DEPLOYER_ADDRESS" | tr 'A-Z' 'a-z')" ]; then
        echo "ERROR: L2_DEPLOYER_ADDRESS and L2_DEPLOYER_PRIVATE_KEY are different accounts." >&2
        echo "  L2_DEPLOYER_ADDRESS: $L2_DEPLOYER_ADDRESS" >&2
        echo "  key derives to:      $DERIVED_ADDRESS" >&2
        echo "Set both to the same account (and to relayer/.env ETH_PRIVATE_KEY), or neither." >&2
        exit 1
    fi
fi

echo "DST_CHAIN: $DST_CHAIN"
echo "L2_RPC:         $L2_RPC"
echo "L2_WS:          ${L2_WS:-}"
echo "L2_RELAYER_RPC: $L2_RELAYER_RPC"
echo "L2_RELAYER_WS:  ${L2_RELAYER_WS:-}"
echo "deployer:  $L2_DEPLOYER_ADDRESS"

# Fail loud if there is no cosmos_to_l2 module to patch, BEFORE spending a deploy.
if [ -n "${MODULE_NAME:-}" ]; then
    MATCHES=$(jq --arg N "$MODULE_NAME" '[.modules[] | select(.name==$N)] | length' "$RELAYER_CONFIG")
    [ "$MATCHES" -eq 0 ] && {
        echo "ERROR: no module named \"$MODULE_NAME\" in $RELAYER_CONFIG" >&2; exit 1; }
    [ "$MATCHES" -gt 1 ] && {
        echo "ERROR: $MATCHES modules named \"$MODULE_NAME\" in $RELAYER_CONFIG; names must be unique" >&2; exit 1; }
else
    MATCHES=$(jq --arg DST "$DST_CHAIN" --arg NAME "${MODULE_NAME:-}" \
      '[.modules[] | select(.src_chain=="cosmos" and .dst_chain==$DST)] | length' \
      "$RELAYER_CONFIG")
    if [ "$MATCHES" -eq 0 ]; then
        echo "ERROR: no cosmos_to_l2 module with src_chain=cosmos dst_chain=$DST_CHAIN in $RELAYER_CONFIG; add one (see the cosmos-to-op example) before deploying" >&2
        exit 1
    fi
    if [ "$MATCHES" -gt 1 ]; then
        echo "ERROR: $MATCHES modules match src_chain=cosmos dst_chain=$DST_CHAIN — Base and Optimism both use dst_chain=opstack." >&2
        echo "  Disambiguate with MODULE_NAME=<module name>, e.g. MODULE_NAME=cosmos-to-base" >&2
        exit 1
    fi
fi

# The prover proves with its n*/pk.bin (see check_prover_pairing.sh for where); the verifier deployed below is compiled
# from contracts/verifiers/Groth16Verifier_N*.sol. A pk from another setup deploys fine
# and then fails every Cosmos -> L2 update with a bare ProofVerificationFailed().
# PROVER_PAIRING=warn downgrades a mismatch to a warning.
scripts/local/check_prover_pairing.sh

# ------------------------------------------------------------------ deploy ---
DEPLOY_OUT=$REPO_ROOT/.l2-deployment.json
export E2E_L2_DEPLOYMENT_OUT=$DEPLOY_OUT
[ -n "${PERMIT2:-}" ] && export PERMIT2

forge script scripts/E2ETestDeployL2.s.sol:E2ETestDeployL2 \
    --rpc-url "$L2_RPC" \
    --broadcast \
    --slow \
    --ffi \
    --sender "$L2_DEPLOYER_ADDRESS" --private-key "$L2_DEPLOYER_PRIVATE_KEY"

# E2ETestDeployL2 writes the address handoff to E2E_L2_DEPLOYMENT_OUT.
[ -f "$DEPLOY_OUT" ] || { echo "ERROR: deploy output $DEPLOY_OUT not written" >&2; exit 1; }

ICS26_ADDRESS=$(jq -r '.ics26Router' "$DEPLOY_OUT")
ICS20_ADDRESS=$(jq -r '.ics20Transfer' "$DEPLOY_OUT")
VERIFIER_ADDRESS=$(jq -r '.signatureVerifier' "$DEPLOY_OUT")
MEMBERSHIP_ADDRESS=$(jq -r '.membership' "$DEPLOY_OUT")
UPDATE_CLIENT_ADDRESS=$(jq -r '.updateClient' "$DEPLOY_OUT")
MISBEHAVIOUR_ADDRESS=$(jq -r '.misbehaviour' "$DEPLOY_OUT")

echo "ICS26_ADDRESS:      $ICS26_ADDRESS"
echo "ICS20_ADDRESS:      $ICS20_ADDRESS"
echo "VERIFIER_ADDRESS:   $VERIFIER_ADDRESS"
echo "MEMBERSHIP_ADDRESS: $MEMBERSHIP_ADDRESS"
echo "UPDATE_CLIENT:      $UPDATE_CLIENT_ADDRESS"
echo "MISBEHAVIOUR:       $MISBEHAVIOUR_ADDRESS"

# ------------------------------------------------------------------- patch ---
# Patch the matching cosmos_to_l2 module and its return-direction pair (leave the
# L1 cosmos_to_eth module and any other L2 family untouched). spectre_client stays
# empty — create-clients-eth fills it once it deploys the SpectreClient against a
# fresh Cosmos genesis.
#
# The return module needs the SAME router address in a different place:
# rollup_profile.common.l2_router. Patching only the forward module left the
# example placeholder (0x7777…) in place — a valid-looking address, so it failed
# far downstream as a membership-proof error against an account that does not
# exist, rather than as "you did not set the router". The two modules are joined
# by client id: cosmos_to_l2.ics26_client_id == l2_to_cosmos.l2_ics26_client_id.
jq \
  --arg DST "$DST_CHAIN" --arg NAME "${MODULE_NAME:-}" \
  --arg RPC "$L2_RELAYER_RPC" \
  --arg WS "${L2_RELAYER_WS:-}" \
  --arg ICS26 "$ICS26_ADDRESS" \
  --arg VERIF "$VERIFIER_ADDRESS" \
  --arg MEMB "$MEMBERSHIP_ADDRESS" \
  --arg UPCL "$UPDATE_CLIENT_ADDRESS" \
  --arg MIS "$MISBEHAVIOUR_ADDRESS" '
    def is_forward:
      if $NAME != "" then .name == $NAME
      else .src_chain == "cosmos" and .dst_chain == $DST end;

    (.modules | map(select(is_forward)) | first | .config.ics26_client_id // "") as $cid
    | .modules |= map(
      if is_forward then
          .config.eth_rpc_url = $RPC
        | .config.eth_ws_url = $WS
        | .config.ics26_address = $ICS26
        | .config.signature_verifier = $VERIF
        | .config.membership = $MEMB
        | .config.update_client = $UPCL
        | .config.misbehaviour = $MIS
      elif ($cid != "" and .config.l2_ics26_client_id == $cid) then
          .config.l2_rpc_url = $RPC
        | .config.rollup_profile.common.l2_router = $ICS26
      else . end
    )
  ' "$RELAYER_CONFIG" > "$RELAYER_CONFIG.tmp" && mv "$RELAYER_CONFIG.tmp" "$RELAYER_CONFIG"

echo "Patched $RELAYER_CONFIG cosmos_to_l2 (dst_chain=$DST_CHAIN) with the deployed L2 addresses."

# Report the return module by name, or say plainly that none was found — a config
# with no matching l2_to_cosmos relays one direction only, which is a legitimate
# shape but never what someone running the E2E wants.
RETURN_MODULE=$(jq -r \
  --arg DST "$DST_CHAIN" --arg NAME "${MODULE_NAME:-}" '
    def is_forward:
      if $NAME != "" then .name == $NAME
      else .src_chain == "cosmos" and .dst_chain == $DST end;
    (.modules | map(select(is_forward)) | first | .config.ics26_client_id // "") as $cid
    | [.modules[] | select($cid != "" and .config.l2_ics26_client_id == $cid) | .name]
    | first // ""
  ' "$RELAYER_CONFIG")

if [ -n "$RETURN_MODULE" ]; then
    echo "Patched the return module \"$RETURN_MODULE\" l2_router + l2_rpc_url to match."
else
    echo "WARNING: no l2_to_cosmos module pairs with this one (no matching l2_ics26_client_id)." >&2
    echo "         The return direction will not relay until you add one." >&2
fi

echo "Next: relayer create-clients-eth --config <that-path>.json  (deploys the SpectreClient on the L2)."
