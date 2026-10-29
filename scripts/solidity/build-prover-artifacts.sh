#!/usr/bin/env bash
# Generates the paired prover artifacts: r1cs/pk/vk for the proving side and
# Groth16Verifier_N{N}.sol for the contracts. The circuit and its setup tool live in the
# spectre-prover repository (cmd/setup-circuits); this script runs it, then publishes the
# two halves where each side reads them:
#
#   r1cs/pk/vk              -> $PROVER_BIN_DIR (default $PROVER_REPO/bin), loaded by proverd
#   Groth16Verifier_N{N}.sol -> contracts/verifiers/
#
# and records their sha256 together in .artifacts/solidity/prover/provenance.json, which
# scripts/local/check_prover_pairing.sh checks before a deploy.
#
#   PROVER_REPO   the spectre-prover checkout (default ../prover next to this repository)
#   PROVER_BIN_DIR where proverd loads the keys from (default $PROVER_REPO/bin)
set -euo pipefail

repo_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
prover_repo=${PROVER_REPO:-$repo_root/../prover}
if [[ ! -f "$prover_repo/cmd/setup-circuits/main.go" ]]; then
  echo "build-prover-artifacts: no spectre-prover checkout at $prover_repo (set PROVER_REPO)" >&2
  exit 2
fi
prover_repo=$(cd "$prover_repo" && pwd)
bin_dir=${PROVER_BIN_DIR:-$prover_repo/bin}
staging="$repo_root/.artifacts/solidity/prover"

# The setup tool links ecip-gnark's Rust FFI; scripts/build-garaga-ffi.sh in the prover
# repository puts it in its own .artifacts/native.
native_lib="$prover_repo/.artifacts/native"
export CGO_LDFLAGS="-L$native_lib${CGO_LDFLAGS:+ $CGO_LDFLAGS}"
export LD_LIBRARY_PATH="$native_lib${LD_LIBRARY_PATH:+:$LD_LIBRARY_PATH}"
export DYLD_LIBRARY_PATH="$native_lib${DYLD_LIBRARY_PATH:+:$DYLD_LIBRARY_PATH}"
force=0
case "${1:-}" in
  "") ;;
  --force) force=1 ;;
  *) echo "usage: $0 [--force]" >&2; exit 2 ;;
esac

buckets=$(
  cd "$prover_repo"
  go run ./cmd/setup-circuits -list-buckets
)
if [[ -z "$buckets" ]]; then
  echo "prover generator reported no configured buckets" >&2
  exit 1
fi

complete=1
for bucket in $buckets; do
  for artifact in r1cs.bin pk.bin vk.bin; do
    [[ -s "$bin_dir/n$bucket/$artifact" ]] || complete=0
  done
  [[ -s "$repo_root/contracts/verifiers/Groth16Verifier_N$bucket.sol" ]] || complete=0
done
if ((complete == 1 && force == 0)); then
  echo "Prover artifacts already present for all configured buckets; skipping randomized setup."
  echo "Use $0 --force to regenerate them intentionally."
  exit 0
fi

mkdir -p "$staging"
run_staging=$(mktemp -d "$staging/run.XXXXXX")
generated="$run_staging/generated"
trap 'rm -rf "$run_staging"' EXIT

mkdir -p "$generated/bin" "$generated/verifiers"

(
  cd "$prover_repo"
  go run ./cmd/setup-circuits "$generated/bin" "$generated/verifiers"
)

for bucket in $buckets; do
  for artifact in r1cs.bin pk.bin vk.bin; do
    [[ -s "$generated/bin/n$bucket/$artifact" ]] || {
      echo "missing staged n$bucket/$artifact" >&2
      exit 1
    }
  done
  [[ -s "$generated/verifiers/Groth16Verifier_N$bucket.sol" ]] || {
    echo "missing staged Groth16Verifier_N$bucket.sol" >&2
    exit 1
  }
done

mkdir -p "$repo_root/contracts/verifiers"
for bucket in $buckets; do
  mkdir -p "$bin_dir/n$bucket"
  cp "$generated/bin/n$bucket/r1cs.bin" "$bin_dir/n$bucket/r1cs.bin"
  cp "$generated/bin/n$bucket/pk.bin" "$bin_dir/n$bucket/pk.bin"
  cp "$generated/bin/n$bucket/vk.bin" "$bin_dir/n$bucket/vk.bin"
  cp "$generated/verifiers/Groth16Verifier_N$bucket.sol" \
    "$repo_root/contracts/verifiers/Groth16Verifier_N$bucket.sol"
done

python3 - "$repo_root" "$bin_dir" "$staging/provenance.json" $buckets <<'PY'
import hashlib
import json
import sys
from pathlib import Path

root = Path(sys.argv[1])
bin_dir = Path(sys.argv[2])
output = Path(sys.argv[3])
buckets = [int(value) for value in sys.argv[4:]]
# The keys live in the prover's directory, outside this repository, so their paths are
# absolute; the verifier sources are inside it and stay relative.
paths = {}
for bucket in buckets:
    prefix = f'n{bucket}'
    paths[f'{prefix}_r1cs'] = str(bin_dir / prefix / 'r1cs.bin')
    paths[f'{prefix}_proving_key'] = str(bin_dir / prefix / 'pk.bin')
    paths[f'{prefix}_verifying_key'] = str(bin_dir / prefix / 'vk.bin')
    paths[f'{prefix}_solidity_verifier'] = f'contracts/verifiers/Groth16Verifier_N{bucket}.sol'
data = {
    'schema_version': 1,
    'scope': 'checked-local-generator',
    'supported_buckets': buckets,
    'generator': 'cd $PROVER_REPO && go run ./cmd/setup-circuits <staging-bin> <staging-verifiers>',
    'artifacts': {
        key: {'path': value, 'sha256': hashlib.sha256((root / value).read_bytes()).hexdigest()}
        for key, value in paths.items()
    },
}
output.write_text(json.dumps(data, indent=2) + '\n')
PY

echo "Published paired artifacts for configured buckets: $(printf '%s ' $buckets)"
echo "Keys:       $bin_dir"
echo "Verifiers:  contracts/verifiers"
echo "Provenance: ${staging#"$repo_root/"}"/provenance.json
