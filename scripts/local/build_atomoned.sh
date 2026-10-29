#!/usr/bin/env bash
# Builds atomoned from the AtomOne checkout (default ../atomone) into
# $ATOMONE_DIR/build/atomoned.
#
# AtomOne's own `make build` refuses any Go toolchain other than the exact
# version it names (check_go_version), which a newer patch release fails, so this
# runs the same go build without that gate. CGO is off, as in AtomOne's e2e
# Dockerfile.
set -euo pipefail

cd "$(dirname "$0")/../.."
ATOMONE_DIR=${ATOMONE_DIR:-../atomone}
ATOMONE_DIR=$(cd "$ATOMONE_DIR" && pwd)

branch=$(git -C "$ATOMONE_DIR" branch --show-current)
echo "[build_atomoned] $ATOMONE_DIR on branch $branch @ $(git -C "$ATOMONE_DIR" rev-parse --short HEAD)"

mkdir -p "$ATOMONE_DIR/build"
cd "$ATOMONE_DIR"
CGO_ENABLED=0 go build -mod=readonly \
    -ldflags "-X github.com/cosmos/cosmos-sdk/version.Name=atomone -X github.com/cosmos/cosmos-sdk/version.AppName=atomoned -X github.com/cosmos/cosmos-sdk/version.Version=${branch}" \
    -o build/atomoned ./cmd/atomoned
echo "[build_atomoned] built $ATOMONE_DIR/build/atomoned"
