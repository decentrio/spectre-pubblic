#!/usr/bin/env bash

# Checks that the Groth16 proving key the prover will use belongs to the
# Groth16Verifier_N{N}.sol that is (or will be) deployed. A pk from one setup
# against a verifier from another does not fail loudly: every Cosmos -> EVM
# updateApplicationState / updateConsensusState reverts with a bare
# ProofVerificationFailed(), and the packet is re-queued forever.
#
# Two modes:
#
#   check_prover_pairing.sh
#       BEFORE DEPLOY. Local files only: r1cs/pk/vk/verifier all exist and their
#       sha256 equals the set that scripts/solidity/build-prover-artifacts.sh
#       published together (.artifacts/solidity/prover/provenance.json). The
#       r1cs/pk/vk are read from the directory proverd will actually load them
#       from, not from the path recorded in provenance: PROVER_BIN_DIR from the
#       environment, else ./bin in the spectre-prover checkout (PROVER_REPO,
#       default ../prover next to this repository), the same order proverd
#       resolves it when started from that checkout. A relative PROVER_BIN_DIR is
#       taken against that checkout.
#
#   check_prover_pairing.sh --rpc <evm-rpc> [--signature-verifier <addr>]
#       BEFORE SENDING PACKETS (and before starting the prover and the relayer). Everything above, plus
#       the verifier registered in SignatureVerifier.buckets(N) on that chain
#       must carry exactly the runtime bytecode forge compiled from the local
#       Groth16Verifier_N{N}.sol (out/ is matched to the source by content hash,
#       so an old file timestamp is not mistaken for a stale build). The vk is baked into that bytecode as constants,
#       so equal bytecode means the on-chain vk is the one this pk was set up with.
#       --signature-verifier defaults to .l2-deployment.json (.signatureVerifier),
#       which deploy_l2_contracts.sh writes; pass it explicitly for an L1 deploy.
#
# Exit 1 on any mismatch. PROVER_PAIRING=warn prints the same banner but exits 0.
# Fix for a mismatch: regenerate the set with scripts/solidity/build-prover-artifacts.sh
# --force, then REDEPLOY the verifier (deploy_*_contracts.sh) and create the
# clients again; or restore the matching pk.bin.

set -euo pipefail

cd "$(dirname "$0")/../.."
REPO_ROOT=$PWD
PROVENANCE=$REPO_ROOT/.artifacts/solidity/prover/provenance.json

RPC=""
SIG_VERIFIER=""
while [ $# -gt 0 ]; do
    case "$1" in
        --rpc) RPC=${2:?--rpc needs a URL}; shift 2 ;;
        --signature-verifier) SIG_VERIFIER=${2:?--signature-verifier needs an address}; shift 2 ;;
        -h | --help) sed -n '3,/^$/p' "$0" | sed -e '/^$/d' -e 's/^# \{0,1\}//'; exit 0 ;;
        *) echo "unknown argument: $1" >&2; exit 2 ;;
    esac
done

for tool in jq; do
    command -v "$tool" >/dev/null 2>&1 || { echo "check_prover_pairing: $tool is required" >&2; exit 2; }
done
if [ -n "$RPC" ]; then
    command -v cast >/dev/null 2>&1 || { echo "check_prover_pairing: cast is required for --rpc" >&2; exit 2; }
fi

sha256_of() {
    if command -v sha256sum >/dev/null 2>&1; then sha256sum "$1" | cut -d' ' -f1; else shasum -a 256 "$1" | cut -d' ' -f1; fi
}

PROBLEMS=()
problem() { PROBLEMS+=("$*"); }

if [ ! -f "$PROVENANCE" ]; then
    problem "no provenance at ${PROVENANCE#"$REPO_ROOT/"}: nothing records which pk was set up with which verifier. Run scripts/solidity/build-prover-artifacts.sh --force"
    BUCKETS=""
else
    BUCKETS=$(jq -r '.supported_buckets[]' "$PROVENANCE")
fi

# The directory proverd loads keys from. The relayer no longer loads any: the circuit
# artifacts belong to the spectre-prover checkout, whose proverd reads PROVER_BIN_DIR and
# defaults to ./bin (an empty value counts as unset, as there).
PROVER_REPO=${PROVER_REPO:-$REPO_ROOT/../prover}
BIN_DIR=${PROVER_BIN_DIR:-}
if [ -n "$BIN_DIR" ]; then
    BIN_DIR_SRC="PROVER_BIN_DIR in the environment"
else
    BIN_DIR=./bin
    BIN_DIR_SRC="the proverd default ./bin in the checkout at $PROVER_REPO (PROVER_REPO)"
fi
case "$BIN_DIR" in /*) ;; *) BIN_DIR=$PROVER_REPO/${BIN_DIR#./} ;; esac

# Local files vs the set that was published together.
for N in $BUCKETS; do
    for key in r1cs proving_key verifying_key solidity_verifier; do
        want=$(jq -r --arg k "n${N}_${key}" '.artifacts[$k].sha256 // empty' "$PROVENANCE")
        case "$key" in
            r1cs) file=$BIN_DIR/n${N}/r1cs.bin ;;
            proving_key) file=$BIN_DIR/n${N}/pk.bin ;;
            verifying_key) file=$BIN_DIR/n${N}/vk.bin ;;
            solidity_verifier) file=$REPO_ROOT/contracts/verifiers/Groth16Verifier_N${N}.sol ;;
        esac
        if [ -z "$want" ]; then
            problem "n${N}_${key}: not recorded in provenance"
        elif [ ! -s "$file" ]; then
            problem "$file is missing or empty (proverd reads keys from $BIN_DIR, set by $BIN_DIR_SRC; pk.bin is git-ignored and cannot be recovered, so the set must be regenerated together)"
        else
            got=$(sha256_of "$file")
            [ "$got" = "$want" ] || problem "$file sha256 ${got:0:16}... differs from the published set (${want:0:16}...): it did not come from the same build-prover-artifacts.sh run as the other files"
        fi
    done
done

# On-chain verifier vs what forge compiled from the local .sol.
if [ -n "$RPC" ] && [ -n "$BUCKETS" ]; then
    if [ -z "$SIG_VERIFIER" ] && [ -f "$REPO_ROOT/.l2-deployment.json" ]; then
        SIG_VERIFIER=$(jq -r '.signatureVerifier // empty' "$REPO_ROOT/.l2-deployment.json")
    fi
    if [ -z "$SIG_VERIFIER" ]; then
        problem "no SignatureVerifier address: pass --signature-verifier (no .l2-deployment.json to read it from)"
    else
        for N in $BUCKETS; do
            artifact=$REPO_ROOT/out/Groth16Verifier_N${N}.sol/Groth16Verifier_N${N}.json
            sol=$REPO_ROOT/contracts/verifiers/Groth16Verifier_N${N}.sol
            # out/ must have been compiled from THIS source: forge records the keccak256 of
            # every source in the artifact metadata. Timestamps say nothing (a restored or
            # touched file is newer than an up-to-date build that forge will not redo).
            if [ ! -f "$artifact" ]; then
                problem "n${N}: no forge artifact at ${artifact#"$REPO_ROOT/"}. Run: forge build"
                continue
            fi
            src_key="contracts/verifiers/Groth16Verifier_N${N}.sol"
            built=$(jq -r --arg k "$src_key" '.metadata.sources[$k].keccak256 // empty' "$artifact")
            have=$(cast keccak < "$sol")
            if [ -z "$built" ] || [ "$built" != "$have" ]; then
                problem "n${N}: out/ was not compiled from the current $src_key (source keccak256 ${have:0:12}... vs recorded ${built:-none}), so the bytecode comparison would be against another build. Run: forge build"
                continue
            fi
            verifier=$(cast call "$SIG_VERIFIER" "buckets(uint16)(address,bytes4)" "$N" --rpc-url "$RPC" 2>/dev/null | head -1 || true)
            if [ -z "$verifier" ] || [ "$verifier" = 0x0000000000000000000000000000000000000000 ]; then
                problem "n${N}: SignatureVerifier $SIG_VERIFIER has no bucket $N registered on $RPC"
                continue
            fi
            onchain=$(cast code "$verifier" --rpc-url "$RPC" 2>/dev/null || true)
            local_code=$(jq -r '.deployedBytecode.object' "$artifact")
            if [ "$onchain" != "$local_code" ]; then
                problem "n${N}: verifier $verifier registered on-chain is NOT the local Groth16Verifier_N${N}.sol (code differs); proofs from the local pk will revert with ProofVerificationFailed(). Redeploy the contracts and recreate the clients"
            else
                echo "check_prover_pairing: n${N} on-chain verifier $verifier == local Groth16Verifier_N${N}.sol"
            fi
        done
    fi
fi

if [ ${#PROBLEMS[@]} -eq 0 ]; then
    echo "check_prover_pairing: OK (buckets: $(echo $BUCKETS | tr '\n' ' '); keys from $BIN_DIR)"
    exit 0
fi

{
    echo
    echo "=================================================================="
    echo " WARNING: proving key / Groth16 verifier pairing is NOT established"
    echo "=================================================================="
    for p in "${PROBLEMS[@]}"; do echo " - $p"; done
    echo
    echo " Consequence: Cosmos -> EVM packets will not be delivered; the relayer"
    echo " logs ProofVerificationFailed() and re-queues them indefinitely."
    echo "=================================================================="
} >&2

if [ "${PROVER_PAIRING:-}" = warn ]; then
    echo "check_prover_pairing: PROVER_PAIRING=warn, continuing anyway" >&2
    exit 0
fi
exit 1
