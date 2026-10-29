#!/usr/bin/env bash
# Fails if the vendored copy of the prover service proto drifted from the
# spectre-prover repository it is vendored from.
#
# The relayer calls the prover over gRPC. That service is defined in the spectre-prover
# repository (proto/prover/v1/prover.proto), which is the source of truth;
# relayer/prover/proverpb/proto carries a copy with only go_package (and a header
# comment) changed, and prover.pb.go / prover_grpc.pb.go are generated from it. A
# message the relayer builds from a stale copy decodes as something else in the prover,
# so run this whenever either side changes.
#
#   PROVER_DIR=../prover ./scripts/check-prover-proto-sync.sh
set -euo pipefail

cd "$(dirname "$0")/.."
PROVER_DIR=${PROVER_DIR:-../prover}

source_proto="$PROVER_DIR/proto/prover/v1/prover.proto"
vendored_proto="relayer/prover/proverpb/proto/prover/v1/prover.proto"

for f in "$source_proto" "$vendored_proto"; do
    [ -f "$f" ] || { echo "check-prover-proto-sync: missing $f" >&2; exit 2; }
done

# Everything from the `syntax` line on, without go_package: the only line that may
# differ, and the vendored header comment sits above `syntax`.
normalize() {
    awk '/^syntax = /{body=1} body' "$1" | grep -v '^option go_package = '
}

if ! diff -u <(normalize "$source_proto") <(normalize "$vendored_proto"); then
    echo "check-prover-proto-sync: the vendored proto drifted from $source_proto" >&2
    echo "copy the spectre-prover file over it (keeping go_package and the header comment) and run" >&2
    echo "  go generate ./prover/proverpb   (from relayer/)" >&2
    exit 1
fi
echo "check-prover-proto-sync: vendored proto matches $source_proto"
