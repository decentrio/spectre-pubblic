#!/bin/sh
set -eu

cd "$(dirname "$0")/../.."
exec ./scripts/local/store_wasm_artifact.sh \
  artifacts/cw_ics08_wasm_base.wasm \
  base-light-client \
  "cw-ics08-wasm Base signed light client"
