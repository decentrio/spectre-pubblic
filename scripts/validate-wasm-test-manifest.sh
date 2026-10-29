#!/usr/bin/env bash
set -euo pipefail
export LC_ALL=C

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$root"

manifest="test/fixtures/wasm-contracts/test-manifest.txt"
generated="$(mktemp)"
trap 'rm -f "$generated"' EXIT
go_cache="${GOCACHE:-/tmp/spectre-wasm-manifest-go-cache}"
go_library_path="$root/.artifacts/native${LD_LIBRARY_PATH:+:$LD_LIBRARY_PATH}"
export CGO_LDFLAGS="-L$root/.artifacts/native${CGO_LDFLAGS:+ $CGO_LDFLAGS}"

sha256_files() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$@"
  elif command -v shasum >/dev/null 2>&1; then
    shasum -a 256 "$@"
  else
    echo "sha256sum or shasum is required" >&2
    return 1
  fi
}

packages=(
  ibc-eureka-solidity-types
  ethereum-light-client
  ethereum-trie-db
  ethereum-types
  tree_hash
  l2-client
  l2-op-stack
  l2-arbitrum
  cw-ics08-wasm-op
  cw-ics08-wasm-base
  cw-ics08-wasm-arbitrum
)

list_go_tests() {
  local module="$1"
  shift
  (
    cd "$module"
    while IFS= read -r package; do
      env GOCACHE="$go_cache" LD_LIBRARY_PATH="$go_library_path" \
        go test "$package" -list . 2>/dev/null \
        | sed -n '/^Test/p' \
        | sort \
        | sed "s|^|go:$package:|; s|$|:pass|"
    done < <(env GOCACHE="$go_cache" go list "$@")
  )
}

{
  echo "# Expected outcomes are recorded per identifier"
  for package in "${packages[@]}"; do
    while IFS= read -r test_name; do
      outcome="pass"
      if [[ "$package" == tree_hash && "$test_name" == packages/ethereum/tree_hash/src/* ]]; then
        outcome="ignored"
      fi
      printf 'rust:%s:%s:%s\n' "$package" "$test_name" "$outcome"
    done < <(
      cargo test --locked -p "$package" -- --list --format terse 2>/dev/null \
        | sed -n 's/: test$//p' \
        | sort
    )
  done
  cargo test --locked --manifest-path programs/cw-ics08-wasm-eth/Cargo.toml \
    -- --list --format terse 2>/dev/null \
    | sed -n 's/: test$//p' \
    | sort \
    | sed 's|^|rust:cw-ics08-wasm-eth:|; s|$|:pass|'

  list_go_tests e2e/interchaintestv8 ./wasm
  echo "solidity:test/solidity-ibc/WasmStorageLayoutCompatibility.t.sol:test_shared_wasm_storage_layout_fixture:pass"

  echo "# Fixture SHA-256"
  sha256_files \
    packages/l2-op-stack/config/op-sepolia.json \
    packages/l2-op-stack/config/base-sepolia.json \
    packages/l2-arbitrum/config/arbitrum-sepolia.json \
    test/fixtures/wasm-contracts/l2-attestation-vectors.json \
    test/fixtures/wasm-contracts/l2-attestation-negative.json \
    test/fixtures/wasm-contracts/l2-client-message.json \
    test/fixtures/wasm-contracts/l2-client-message-unsigned.json \
    test/fixtures/wasm-contracts/solidity-storage-layout.json \
    test/fixtures/wasm-contracts/wasm-interface.json \
    test/fixtures/wasm-contracts/wasmvm-gas-baseline.json \
    packages/ethereum/light-client/src/test_utils/fixtures/Test_ICS20TransferERC20TokenfromEthereumToCosmosAndBack.json \
    packages/ethereum/light-client/src/test_utils/fixtures/Test_TimeoutPacketFromCosmos.json \
    | sort -k2
} > "$generated"

baseline_ref="${WASM_TEST_BASE_REF:-main}"
if ! git rev-parse --verify --quiet "${baseline_ref}^{commit}" >/dev/null; then
  echo "missing test baseline ref: $baseline_ref" >&2
  exit 1
fi
merge_base="$(git merge-base HEAD "$baseline_ref")"

if git diff --unified=0 "$merge_base" -- '*.rs' '*.go' \
  | grep -E '^\+.*(#\[ignore\]|\.Skip\(|\.Skipf\(|\.SkipNow\()' \
  | grep -Ev '^\+.*t\.Skipf\("optimized artifact %s is absent; run VALIDATE_OPTIMIZED=1 scripts/validate-l2-clients\.sh", artifact\) // wasm-conformance: allow-env-skip$' \
  | grep -q .; then
  echo "new ignored or skipped test found in changed scope" >&2
  exit 1
fi

if [[ "${WASM_TEST_MANIFEST_UPDATE:-0}" == 1 ]]; then
  cp "$generated" "$manifest"
elif [[ "${WASM_TEST_MANIFEST_PRINT:-0}" == 1 ]]; then
  cat "$generated"
else
  diff -u "$manifest" "$generated"
fi
