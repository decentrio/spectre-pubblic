#!/usr/bin/env bash
# Regenerates prover.pb.go and prover_grpc.pb.go from the vendored proto. Needs protoc and
# the plugin versions the attestor stubs were generated with on PATH:
#   go install google.golang.org/protobuf/cmd/protoc-gen-go@v1.36.11
#   go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@v1.5.1
# (a protoc-gen-go from golang/protobuf 1.5.x is older and writes different code).
# The include root is proto/, so the file registers as prover/v1/prover.proto, the same
# name the spectre-prover repository registers. Output goes through a scratch directory
# because the plugins write under the go_package path.
set -euo pipefail

cd "$(dirname "$0")"
for tool in protoc protoc-gen-go protoc-gen-go-grpc; do
  command -v "$tool" >/dev/null 2>&1 || { echo "generate: $tool not found on PATH" >&2; exit 2; }
done

out=$(mktemp -d)
trap 'rm -rf "$out"' EXIT

protoc -I proto \
  --go_out="$out" --go_opt=module=relayer \
  --go-grpc_out="$out" --go-grpc_opt=module=relayer \
  prover/v1/prover.proto

cp "$out"/prover/proverpb/prover.pb.go ./prover.pb.go
cp "$out"/prover/proverpb/prover_grpc.pb.go ./prover_grpc.pb.go
