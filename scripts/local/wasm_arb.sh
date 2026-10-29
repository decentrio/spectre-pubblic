#!/bin/sh
set -eu

cd "$(dirname "$0")/../.."
exec ./scripts/local/store_wasm_artifact.sh \
  artifacts/cw_ics08_wasm_arbitrum.wasm \
  arbitrum-light-client \
  "cw-ics08-wasm Arbitrum signed light client"
