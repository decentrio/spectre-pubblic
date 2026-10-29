#!/usr/bin/env bash
# Fails if the vendored copy of the spectre light client proto drifted from the
# atomone module it is vendored from.
#
# The relayer builds messages for a native Go light client module that lives in the
# atomone repository (proto/ibc/lightclients/spectre/v1/spectre.proto). That file is
# the source of truth; relayer/chain/ethnative/spectrepb/proto carries a copy with
# only go_package (and a header comment) changed, and spectre.pb.go is generated
# from it. A message the relayer builds from a stale copy decodes as something else
# on chain, so run this whenever either side changes.
#
#   ATOMONE_DIR=../atomone ./scripts/check-spectre-proto-sync.sh
set -euo pipefail

cd "$(dirname "$0")/.."
ATOMONE_DIR=${ATOMONE_DIR:-../atomone}

source_proto="$ATOMONE_DIR/proto/ibc/lightclients/spectre/v1/spectre.proto"
vendored_proto="relayer/chain/ethnative/spectrepb/proto/ibc/lightclients/spectre/v1/spectre.proto"

for f in "$source_proto" "$vendored_proto"; do
    [ -f "$f" ] || { echo "check-spectre-proto-sync: missing $f" >&2; exit 2; }
done

# Everything from the `syntax` line on, without go_package: the only line that may
# differ, and the vendored header comment sits above `syntax`.
normalize() {
    awk '/^syntax = /{body=1} body' "$1" | grep -v '^option go_package = '
}

if ! diff -u <(normalize "$source_proto") <(normalize "$vendored_proto"); then
    echo "check-spectre-proto-sync: the vendored proto drifted from $source_proto" >&2
    echo "copy the atomone file over it (keeping go_package and the header comment) and run" >&2
    echo "  go generate ./chain/ethnative/spectrepb   (from relayer/)" >&2
    exit 1
fi
echo "check-spectre-proto-sync: vendored proto matches $source_proto"
