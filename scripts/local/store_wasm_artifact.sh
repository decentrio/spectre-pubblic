#!/bin/sh
set -eu

if [ "$#" -ne 3 ]; then
  echo "usage: $0 <optimized.wasm> <proposal-title> <summary>" >&2
  exit 2
fi

artifact_path="$1"
proposal_title="$2"
proposal_summary="$3"
if [ ! -f "$artifact_path" ]; then
  echo "missing optimized artifact: $artifact_path" >&2
  echo "run VALIDATE_OPTIMIZED=1 scripts/validate-l2-clients.sh first" >&2
  exit 1
fi

. ./scripts/local/gaiad_binary.sh
. ./scripts/local/wasm_checksum.sh

chain_id="test-ibc-eth"
keyring="test"
# The shared voting helpers use uppercase shell variables. Keep the local names
# above for this script's own CLI calls and initialise the helpers explicitly.
CHAIN_ID=$chain_id
KEYRING=$keyring
signer="cosmos10d07y265gmmuvt4z0w9aw880jnsr700j6zn9kn"
# GNU base64 spells this `-w 0`, whereas macOS/BSD base64 uses `-b`.  Remove
# wrapping explicitly so the proposal payload is portable across both hosts.
#
# The payload goes to jq through a file (--rawfile), never through --arg. The gzipped, base64 wasm is
# about 300 KB, and Linux caps a single command-line argument at 128 KiB (MAX_ARG_STRLEN), so
# `jq --arg` fails with "Argument list too long" there even though it happens to work on macOS.
wasm_byte_code_file="$(mktemp)"
trap 'rm -f "$wasm_byte_code_file"' EXIT
gzip -n -c "$artifact_path" | base64 | tr -d '\n' > "$wasm_byte_code_file"

jq -n \
  --arg signer "$signer" \
  --rawfile wasm_byte_code "$wasm_byte_code_file" \
  --arg title "$proposal_title" \
  --arg summary "$proposal_summary" \
  '{
    messages: [{
      "@type": "/ibc.lightclients.wasm.v1.MsgStoreCode",
      signer: $signer,
      wasm_byte_code: $wasm_byte_code
    }],
    title: $title,
    deposit: "10000000stake",
    summary: $summary,
    expedited: false
  }' > proposal.json

"$GAIAD" tx gov submit-proposal proposal.json \
  --from val1 \
  --home "$HOME/.gaia" \
  --chain-id "$chain_id" \
  --keyring-backend "$keyring" \
  --gas 200000000 \
  --gas-prices 1stake \
  -y

sleep 5
proposal_id="$(
  "$GAIAD" q gov proposals -o json \
    | jq -r '.proposals | sort_by(.id | tonumber) | last | .id'
)"
sleep 5
vote_default_validators "$proposal_id"

checksum="$(wasm_checksum_from_proposal proposal.json)"
# The local Cosmos network produces a block roughly every 30 seconds and its
# governance voting period is one minute.  Poll the chain rather than assuming
# a fixed delay, so this works reliably on slower developer machines too.
for _ in $(seq 1 45); do
  if wasm_checksum_is_stored "$checksum"; then
    echo "Checksum: 0x$checksum"
    exit 0
  fi
  sleep 2
done

assert_wasm_checksum_stored "$checksum"
echo "Checksum: 0x$checksum"
