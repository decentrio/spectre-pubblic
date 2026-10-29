#!/usr/bin/env bash
# Local AtomOne chain for the ETH <-> AtomOne devnet: one validator, atomoned from
# the atomone checkout's branch e2e/module-spectre.
#
# The mirror of run_cosmos_node.sh (Gaia), with one step fewer: AtomOne hosts the
# Ethereum light client as a native Go module, so there is no wasm to store and no
# governance proposal before `create-clients-cosmos`. IBC's default AllowedClients is
# the wildcard, so a fresh genesis already admits the "spectre" client type.
#
# AtomOne pays fees in uphoton (dynamicfee), signs with the bech32 prefix atone, and
# has the relayer account fund both denoms. The script rewrites the relayer's
# COSMOS_* settings in relayer/.env, a TRACKED file: never stage it, and restore it
# with `git checkout -- relayer/.env` at teardown.
set -euo pipefail
set -x

# Internal paths (relayer/.env, etc.) are repo-root relative.
cd "$(dirname "$0")/../.."

. ./scripts/local/atomoned_binary.sh

HOME_DIR=${ATOMONE_HOME:-$HOME/.atomone-devnet}
CHAIN_ID=${CHAIN_ID:-atomone-devnet-1}
KEYRING="test"
STAKE_DENOM="uatone"
FEE_DENOM="uphoton"
ADDRESS_PREFIX="atone"
RELAYER_KEY=test1
RELAYER_ENV_FILE="${RELAYER_ENV_FILE:-relayer/.env}"
# The relayer pays a fixed fee per transaction (COSMOS_FEE_AMOUNT). dynamicfee's
# minimum base gas price is 0.01 uphoton per gas, so 100000 uphoton covers the
# default 1.5M gas limit several times over.
RELAYER_FEE_AMOUNT=${RELAYER_FEE_AMOUNT:-100000}

killall atomoned || true
rm -rf "$HOME_DIR"

upsert_env_var() {
    local file="$1"
    local key="$2"
    local value="$3"
    local tmp

    mkdir -p "$(dirname "$file")"
    touch "$file"
    tmp="$(mktemp)"

    awk -v key="$key" -v value="$value" '
        BEGIN { updated = 0 }
        $0 ~ ("^" key "=") {
            print key "=\"" value "\""
            updated = 1
            next
        }
        { print }
        END {
            if (!updated) {
                print key "=\"" value "\""
            }
        }
    ' "$file" > "$tmp"

    mv "$tmp" "$file"
}

edit_genesis() {
    jq "$1" "$HOME_DIR/config/genesis.json" > "$HOME_DIR/config/tmp_genesis.json"
    mv "$HOME_DIR/config/tmp_genesis.json" "$HOME_DIR/config/genesis.json"
}

"$ATOMONED" init devnet --default-denom "$STAKE_DENOM" --chain-id "$CHAIN_ID" --home "$HOME_DIR"
"$ATOMONED" config set client chain-id "$CHAIN_ID" --home "$HOME_DIR"
"$ATOMONED" config set client keyring-backend "$KEYRING" --home "$HOME_DIR"

# val is the validator; test/test1/test2/test3 mirror the Gaia devnet's accounts, and
# test1 is the relayer's.
for key in val test test1 test2 test3; do
    "$ATOMONED" keys add "$key" --keyring-backend "$KEYRING" --home "$HOME_DIR"
done

"$ATOMONED" genesis add-genesis-account val "1000000000000${STAKE_DENOM},1000000000${FEE_DENOM}" \
    --keyring-backend "$KEYRING" --home "$HOME_DIR"
for key in test test1 test2 test3; do
    "$ATOMONED" genesis add-genesis-account "$key" "2000000000000${STAKE_DENOM},1000000000000${FEE_DENOM}" \
        --keyring-backend "$KEYRING" --home "$HOME_DIR"
done

# Fees go in uphoton on AtomOne (the stake denom is uatone).
edit_genesis ".app_state.dynamicfee.params.fee_denom = \"${FEE_DENOM}\""

"$ATOMONED" genesis gentx val "1000000000${STAKE_DENOM}" \
    --chain-id "$CHAIN_ID" --keyring-backend "$KEYRING" --home "$HOME_DIR"
"$ATOMONED" genesis collect-gentxs --home "$HOME_DIR"
"$ATOMONED" genesis validate-genesis --home "$HOME_DIR"

sed -i'' -e "s/^minimum-gas-prices *= .*/minimum-gas-prices = \"0.01${STAKE_DENOM},0.01${FEE_DENOM}\"/" "$HOME_DIR/config/app.toml"
"$ATOMONED" config set app api.enable true --home "$HOME_DIR"

# atomoned asks for confirmation before it prints an unarmored key and has no -y flag.
COSMOS_PRIVATE_KEY="$(printf 'y\n' | "$ATOMONED" keys export "$RELAYER_KEY" --unarmored-hex --unsafe --keyring-backend "$KEYRING" --home "$HOME_DIR" 2>/dev/null | tail -n 1)"
if ! [[ "$COSMOS_PRIVATE_KEY" =~ ^[0-9a-f]{64}$ ]]; then
    echo "could not export the relayer key $RELAYER_KEY as 64 hex characters" >&2
    exit 1
fi
upsert_env_var "$RELAYER_ENV_FILE" "COSMOS_PRIVATE_KEY" "$COSMOS_PRIVATE_KEY"
upsert_env_var "$RELAYER_ENV_FILE" "COSMOS_CHAIN_ID" "$CHAIN_ID"
upsert_env_var "$RELAYER_ENV_FILE" "COSMOS_ADDRESS_PREFIX" "$ADDRESS_PREFIX"
upsert_env_var "$RELAYER_ENV_FILE" "COSMOS_FEE_DENOM" "$FEE_DENOM"
upsert_env_var "$RELAYER_ENV_FILE" "COSMOS_FEE_AMOUNT" "$RELAYER_FEE_AMOUNT"
echo "Updated $RELAYER_ENV_FILE with the COSMOS_* settings for AtomOne (a tracked file: do not stage it)"

"$ATOMONED" start --home "$HOME_DIR" > "$HOME_DIR/atomoned.log" 2>&1 &
NODE_PID=$!

cleanup() {
    kill "$NODE_PID" 2>/dev/null || true
}
trap cleanup EXIT INT TERM

echo "Started AtomOne validator: chain-id=$CHAIN_ID home=$HOME_DIR rpc=tcp://127.0.0.1:26657 grpc=127.0.0.1:9090"
echo "Relayer account ($RELAYER_KEY): $("$ATOMONED" keys show "$RELAYER_KEY" -a --keyring-backend "$KEYRING" --home "$HOME_DIR")"
echo "Log: $HOME_DIR/atomoned.log"

wait "$NODE_PID"
