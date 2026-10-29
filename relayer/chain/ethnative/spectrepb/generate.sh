#!/usr/bin/env bash
# Regenerates spectre.pb.go from the vendored proto. Needs protoc and
# protoc-gen-gocosmos on PATH:
#   go install github.com/cosmos/gogoproto/protoc-gen-gocosmos@v1.7.0
# protoc-gen-gocosmos requires the proto file path to match its package
# (ibc/lightclients/spectre/v1/), hence the proto/ include root, and it writes
# under the go_package path, so the output goes through a scratch directory.
set -euo pipefail

cd "$(dirname "$0")"
gogoproto=$(go list -m -f '{{.Dir}}' github.com/cosmos/gogoproto)
out=$(mktemp -d)
trap 'rm -rf "$out"' EXIT

protoc -I proto -I "$gogoproto" -I /opt/homebrew/include \
  --gocosmos_out=plugins=grpc:"$out" \
  proto/ibc/lightclients/spectre/v1/spectre.proto

cp "$out"/relayer/chain/ethnative/spectrepb/spectre.pb.go ./spectre.pb.go
