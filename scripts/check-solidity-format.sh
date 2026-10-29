#!/usr/bin/env bash
set -euo pipefail

repo_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
cd "$repo_root"

# Generated verifiers are deliberately excluded: they are regenerated from the
# verifying keys and validated by the prover artifact-pair gate.
solidity_files=()
while IFS= read -r -d '' solidity_file; do
  solidity_files+=("$solidity_file")
done < <(find contracts scripts test -type f -name '*.sol' ! -path 'contracts/verifiers/*' -print0)
if ((${#solidity_files[@]} == 0)); then
  echo "No tracked Solidity files found"
  exit 0
fi
forge fmt --check "${solidity_files[@]}"
