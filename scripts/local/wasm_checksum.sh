# Helpers for local 08-wasm proposal scripts.
#
# Why these derive the checksum instead of reading one out of the chain's list:
#
# `gaiad q ibc-wasm checksums` returns a collections.KeySet, which paginates in
# ASCENDING KEY ORDER — sorted by checksum bytes, not by when each was stored. So
# neither `.checksums[0]` nor `.checksums[-1]` reliably names the wasm a script
# just stored; which one happens to be right depends on how the hashes sort
# against every other wasm already on the chain.
#
# That is not a display bug. The printed value is what the operator pastes into
# `create-clients-cosmos --wasm-checksum` / `--l2-config`, so picking the wrong
# entry creates the client with ANOTHER client's wasm code. The failure then
# lands much later and names the wrong thing:
#
#   deserializing client state failed: missing field chain_id
#
# — the Ethereum client's code rejecting an L2 profile, which sends you debugging
# the profile instead of the checksum that selected the wrong code.
#
# The checksum is just the sha256 of the UNCOMPRESSED wasm, so computing it from
# the proposal is exact and independent of what else is stored.

wasm_checksum_from_proposal() {
    proposal_file=$1
    python3 - "$proposal_file" <<'PY'
import base64
import gzip
import hashlib
import json
import sys

with open(sys.argv[1], "r", encoding="utf-8") as fh:
    proposal = json.load(fh)

for msg in proposal.get("messages", []):
    blob = msg.get("wasm_byte_code")
    if not blob:
        continue
    wasm = base64.b64decode(blob)
    if wasm[:2] == b"\x1f\x8b":
        wasm = gzip.decompress(wasm)
    print(hashlib.sha256(wasm).hexdigest())
    break
else:
    raise SystemExit("wasm_byte_code not found in proposal")
PY
}

wasm_checksum_is_stored() {
    checksum=$1
    shift
    "$GAIAD" q ibc-wasm checksums "$@" -o json \
        | jq -e --arg checksum "$checksum" '(.checksums // []) | index($checksum) != null' >/dev/null
}

assert_wasm_checksum_stored() {
    checksum=$1
    shift
    if ! wasm_checksum_is_stored "$checksum" "$@"; then
        echo "ERROR: computed wasm checksum $checksum was not stored on-chain" >&2
        echo "       Check the proposal status and execution logs before using this checksum." >&2
        return 1
    fi
}

vote_if_key_exists() {
    proposal_id=$1
    validator=$2
    home=$3

    if "$GAIAD" keys show "$validator" --home "$home" --keyring-backend "$KEYRING" >/dev/null 2>&1; then
        "$GAIAD" tx gov vote "$proposal_id" yes \
            --from "$validator" \
            --home "$home" \
            --chain-id "$CHAIN_ID" \
            --keyring-backend "$KEYRING" \
            --gas-prices 1stake \
            -y
        WASM_VOTES_CAST=$((WASM_VOTES_CAST + 1))
    else
        echo "Skipping $validator vote; key not found in $home"
    fi
}

vote_default_validators() {
    proposal_id=$1

    WASM_VOTES_CAST=0
    vote_if_key_exists "$proposal_id" val1 "$HOME/.gaia"
    vote_if_key_exists "$proposal_id" val2 "$HOME/.gaia-val2"
    vote_if_key_exists "$proposal_id" val3 "$HOME/.gaia-val3"

    if [ "$WASM_VOTES_CAST" -eq 0 ]; then
        echo "ERROR: no validator keys found for proposal $proposal_id; cannot vote it through" >&2
        return 1
    fi
}
