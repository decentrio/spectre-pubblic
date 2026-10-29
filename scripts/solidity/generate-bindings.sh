#!/usr/bin/env bash
set -euo pipefail

repo_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
manifest="$repo_root/scripts/solidity/contracts.json"
mode=${1:-all}
cd "$repo_root"

require_abigen() {
  local expected actual
  expected=$(jq -r '.abigen_version' "$manifest")
  actual=$(abigen --version | sed 's/^abigen version //')
  if [[ "$actual" != "$expected" ]]; then
    echo "abigen version mismatch: expected $expected, got $actual" >&2
    return 1
  fi
}

artifact_for() {
  local name=$1 source=$2 artifact
  artifact="$repo_root/out/${name}.sol/${name}.json"
  [[ -f "$artifact" ]] || { echo "missing artifact: $artifact" >&2; return 1; }
  jq -e --arg source "$source" '.metadata.sources | has($source)' "$artifact" >/dev/null || {
    echo "artifact source mismatch for $name; expected $source" >&2
    return 1
  }
  printf "%s\n" "$artifact"
}

generate_shared() {
  require_abigen
  while IFS= read -r item; do
    local name source abi_path binding_path package artifact
    name=$(jq -r '.name' <<<"$item")
    source=$(jq -r '.source' <<<"$item")
    abi_path=$(jq -r '.abi' <<<"$item")
    binding_path=$(jq -r '.shared_binding' <<<"$item")
    artifact=$(artifact_for "$name" "$source")
    mkdir -p "$(dirname "$abi_path")" "$(dirname "$binding_path")"
    jq '.abi' "$artifact" > "$abi_path"
    package=$(basename "$(dirname "$binding_path")")
    abigen --abi "$abi_path" --pkg "$package" --type Contract --out "$binding_path"
  done < <(jq -c '.contracts[] | select(.abi != null and .shared_binding != null)' "$manifest")
}

generate_relayer() {
  require_abigen
  local staging
  staging=$(mktemp -d)
  trap 'rm -rf "$staging"' RETURN
  while IFS= read -r item; do
    local name source binding_path artifact abi_file bin_file bytecode
    name=$(jq -r '.name' <<<"$item")
    source=$(jq -r '.source' <<<"$item")
    binding_path=$(jq -r '.relayer_binding' <<<"$item")
    artifact=$(artifact_for "$name" "$source")
    abi_file="$staging/${name}.abi"
    bin_file="$staging/${name}.bin"
    jq '.abi' "$artifact" > "$abi_file"
    bytecode=$(jq -r '.bytecode.object' "$artifact")
    if [[ "$bytecode" == *"__$"* ]]; then
      echo "unlinked library placeholder remains in $name" >&2
      return 1
    fi
    printf "%s\n" "$bytecode" > "$bin_file"
    mkdir -p "$(dirname "$binding_path")"
    abigen --bin "$bin_file" --abi "$abi_file" --pkg "contract${name}" --out "$binding_path"
  done < <(jq -c '.contracts[] | select(.relayer_binding != null)' "$manifest")
}

generate_bytecode() {
  local artifact
  artifact=$(artifact_for SpectreClient contracts/light-clients/spectre/SpectreClient.sol)
  mkdir -p abi/bytecode
  cp "$artifact" abi/bytecode/SpectreClient.json
}

forge build
case "$mode" in
  shared) generate_shared ;;
  relayer) generate_relayer ;;
  bytecode) generate_bytecode ;;
  all) generate_shared; generate_relayer; generate_bytecode ;;
  *) echo "usage: $0 {shared|relayer|bytecode|all}" >&2; exit 2 ;;
esac
