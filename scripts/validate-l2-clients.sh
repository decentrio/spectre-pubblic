#!/usr/bin/env bash
set -euo pipefail
export LC_ALL=C

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$root"

packages=(
  l2-client
  l2-op-stack
  l2-arbitrum
  cw-ics08-wasm-arbitrum
  cw-ics08-wasm-base
  cw-ics08-wasm-op
)
programs=(
  cw-ics08-wasm-eth
  cw-ics08-wasm-arbitrum
  cw-ics08-wasm-base
  cw-ics08-wasm-op
)

optimizer_image="cosmwasm/optimizer:0.17.0@sha256:7e0b9229c1a4118d0c9a2af2e7f5d95a91f264c26a2ce5681c779926e74d7f85"
optimizer_rust_toolchain="1.86.0-x86_64-unknown-linux-musl"
interface_fixture="test/fixtures/wasm-contracts/wasm-interface.json"
ibc_go_max_wasm_size=$((3 * 1024 * 1024))
approved_upload_limit=$((ibc_go_max_wasm_size * 9 / 10))
validate_optimized="${VALIDATE_OPTIMIZED:-0}"
go_cache="${GOCACHE:-/tmp/spectre-wasm-go-cache}"
go_library_path="$root/.artifacts/native${LD_LIBRARY_PATH:+:$LD_LIBRARY_PATH}"
export CGO_LDFLAGS="-L$root/.artifacts/native${CGO_LDFLAGS:+ $CGO_LDFLAGS}"
optimizer_cargo_cache="${OPTIMIZER_CARGO_CACHE:-${CARGO_HOME:-${HOME}/.cargo}}"

case "$validate_optimized" in
  0 | 1) ;;
  *)
    printf 'VALIDATE_OPTIMIZED must be 0 or 1, found %s\n' "$validate_optimized" >&2
    exit 2
    ;;
esac

validate_interface() {
  local kind="$1"
  local family="$2"
  local artifact_path="$3"
  local imports_expected imports_actual exports_expected exports_actual
  imports_expected="$(mktemp)"
  imports_actual="$(mktemp)"
  exports_expected="$(mktemp)"
  exports_actual="$(mktemp)"

  jq -r --arg kind "$kind" --arg family "$family" \
    '.[$kind][$family].imports[]' "$interface_fixture" | sort > "$imports_expected"
  node scripts/inspect-wasm.mjs imports "$artifact_path" | sort > "$imports_actual"
  jq -r --arg kind "$kind" --arg family "$family" \
    '.[$kind][$family].exports[]' "$interface_fixture" | sort > "$exports_expected"
  node scripts/inspect-wasm.mjs exports "$artifact_path" | sort > "$exports_actual"

  diff -u "$imports_expected" "$imports_actual"
  diff -u "$exports_expected" "$exports_actual"
  rm -f "$imports_expected" "$imports_actual" "$exports_expected" "$exports_actual"
}

validate_repository_ownership() {
  local stale_pattern='verify_attested_header|ATTESTATIONS_ARE_AUTHENTICATED'
  local legacy_name_pattern='Fast''-IBC|fast''-ibc|fast''_ibc'
  if git diff --unified=0 main -- . \
    ':(exclude)specs/001-refactor-wasm-contracts/compatibility-manifest.md' \
    | rg "^\+.*(${legacy_name_pattern})"; then
    echo 'new unversioned legacy project terminology found in the changed scope' >&2
    return 1
  fi
  if rg -n "$stale_pattern" packages/l2-client packages/l2-op-stack packages/l2-arbitrum \
    programs/cw-ics08-wasm-op programs/cw-ics08-wasm-base programs/cw-ics08-wasm-arbitrum \
    --glob '!**/*_test.*'; then
    echo 'stale unsigned verifier/authentication-bypass name found in active source' >&2
    return 1
  fi
  if [[ "$(rg -l 'pub fn verify_authenticated_header' packages/l2-client/src | wc -l)" != 1 ]]; then
    echo 'the authenticated L2 verifier must have exactly one owner' >&2
    return 1
  fi
  if rg -n '\bAttestedL2Header\b' programs/cw-ics08-wasm-{op,base,arbitrum}; then
    echo 'an L2 program composes an unsigned header type' >&2
    return 1
  fi
  if rg -n 'todo!\(|unimplemented!\(' packages/l2-client/src packages/ethereum \
    programs/cw-ics08-wasm-{eth,op,base,arbitrum}/src; then
    echo 'an exported Wasm source tree still contains a placeholder implementation' >&2
    return 1
  fi
}

for json_file in \
  packages/l2-op-stack/config/op-sepolia.json \
  packages/l2-op-stack/config/base-sepolia.json \
  packages/l2-arbitrum/config/arbitrum-sepolia.json \
  test/fixtures/wasm-contracts/l2-attestation-vectors.json \
  test/fixtures/wasm-contracts/l2-attestation-negative.json \
  "$interface_fixture"; do
  jq empty "$json_file"
done
validate_repository_ownership
generated_vectors="$(mktemp)"
generated_message="$(mktemp)"
generated_unsigned_message="$(mktemp)"
(
  cd relayer
  go run ../scripts/generate-l2-attestation-fixtures.go \
    -out "$generated_vectors" \
    -client-message-out "$generated_message" \
    -unsigned-client-message-out "$generated_unsigned_message"
)
diff -u test/fixtures/wasm-contracts/l2-attestation-vectors.json "$generated_vectors"
diff -u test/fixtures/wasm-contracts/l2-client-message.json "$generated_message"
diff -u test/fixtures/wasm-contracts/l2-client-message-unsigned.json "$generated_unsigned_message"
rm -f "$generated_vectors" "$generated_message" "$generated_unsigned_message"
cargo fmt --all -- --check
cargo clippy --workspace --all-targets --locked -- -D warnings
cargo test --locked --workspace --no-fail-fast
cargo test --locked --manifest-path programs/cw-ics08-wasm-eth/Cargo.toml

(
  cd e2e/interchaintestv8
  env GOCACHE="$go_cache" LD_LIBRARY_PATH="$go_library_path" \
    go test ./wasm -run '^$' -count=1
)
forge test --match-contract WasmStorageLayoutCompatibilityTest

test_args=()
for package in "${packages[@]}"; do
  test_args+=(-p "$package")
done
cargo test --locked "${test_args[@]}"

cargo build --target wasm32-unknown-unknown --release --locked \
  -p cw-ics08-wasm-arbitrum -p cw-ics08-wasm-base -p cw-ics08-wasm-op
cargo build --target wasm32-unknown-unknown --release --locked \
  --manifest-path programs/cw-ics08-wasm-eth/Cargo.toml

for program in "${programs[@]}"; do
  artifact="${program//-/_}.wasm"
  family="l2"
  if [[ "$program" == cw-ics08-wasm-eth ]]; then
    family="ethereum"
  fi
  validate_interface raw "$family" "target/wasm32-unknown-unknown/release/$artifact"
done

reproducibility="not checked; set VALIDATE_OPTIMIZED=1 to build twice with $optimizer_image"
host_conformance="not checked; set VALIDATE_OPTIMIZED=1 to run pinned wasmvm v2.2.4 fixtures"
declare -A first_pass_hashes=()
declare -A second_pass_hashes=()
optimizer_volumes=()

cleanup_optimizer_volumes() {
  if ((${#optimizer_volumes[@]} > 0)); then
    docker volume rm -f "${optimizer_volumes[@]}" >/dev/null 2>&1 || true
  fi
}
trap cleanup_optimizer_volumes EXIT

if [[ "$validate_optimized" == 1 ]]; then
  # Programs within one pass share only that pass's disposable target cache. Pass 2 uses a wholly
  # separate target volume so it cannot reuse any pass-1 build product. The source cache is mounted
  # read-only and Cargo remains offline; Cargo.lock still authenticates every registry/git input.
  for source_cache in "$optimizer_cargo_cache/registry" "$optimizer_cargo_cache/git"; do
    if [[ ! -d "$source_cache" ]]; then
      printf 'offline optimizer source cache is missing: %s\n' "$source_cache" >&2
      exit 1
    fi
  done
  cache_prefix="spectre_wasm_validation_$$"
  for pass in 1 2; do
    target_cache="${cache_prefix}_target_${pass}"
    optimizer_volumes+=("$target_cache")
    for program in "${programs[@]}"; do
      docker run --rm \
        -v "$root:/code" \
        -e CARGO_NET_OFFLINE=true \
        -e RUSTUP_TOOLCHAIN="$optimizer_rust_toolchain" \
        --mount "type=bind,source=$optimizer_cargo_cache/registry,target=/usr/local/cargo/registry,readonly" \
        --mount "type=bind,source=$optimizer_cargo_cache/git,target=/usr/local/cargo/git,readonly" \
        --mount "type=volume,source=$target_cache,target=/target" \
        "$optimizer_image" "./programs/$program"
      artifact="${program//-/_}.wasm"
      optimized="artifacts/$artifact"
      if [[ ! -f "$optimized" ]]; then
        printf 'optimizer did not produce %s\n' "$optimized" >&2
        exit 1
      fi
      checksum="$(sha256sum "$optimized" | cut -d' ' -f1)"
      bytes="$(stat -c %s "$optimized")"
      if ((bytes > approved_upload_limit)); then
        printf 'optimized artifact exceeds 90%% of ibc-go MaxWasmSize (%d): %s is %d bytes\n' \
          "$approved_upload_limit" "$artifact" "$bytes" >&2
        exit 1
      fi
      if [[ "$pass" == 1 ]]; then
        first_pass_hashes["$artifact"]="$checksum"
      else
        second_pass_hashes["$artifact"]="$checksum"
        if [[ "${first_pass_hashes[$artifact]}" != "$checksum" ]]; then
          printf 'non-reproducible optimized artifact: %s\n' "$artifact" >&2
          exit 1
        fi
      fi
    done
  done
  reproducibility="confirmed by two optimizer passes with independent ephemeral caches"

  for program in "${programs[@]}"; do
    artifact="${program//-/_}.wasm"
    family="l2"
    if [[ "$program" == cw-ics08-wasm-eth ]]; then
      family="ethereum"
    fi
    validate_interface optimized "$family" "artifacts/$artifact"
  done

  gas_report="target/wasmvm-gas-report.json"
  gas_baseline="test/fixtures/wasm-contracts/wasmvm-gas-baseline.json"
  if [[ ! -f "$gas_baseline" ]]; then
    printf 'required wasmvm gas baseline is missing: %s\n' "$gas_baseline" >&2
    exit 1
  fi
  rm -f "$gas_report"
  (
    cd "$root/e2e/interchaintestv8"
    env \
      GOCACHE="$go_cache" \
      LD_LIBRARY_PATH="$go_library_path" \
      WASM_CONFORMANCE_REPO_ROOT="$root" \
      WASM_CONFORMANCE_ARTIFACT_DIR="$root/artifacts" \
      WASM_CONFORMANCE_REPORT="$root/$gas_report" \
      WASM_CONFORMANCE_GAS_BASELINE="$root/$gas_baseline" \
      go test ./wasm -run 'TestOptimizedArtifactsConformToPinnedWasmVM|TestPinnedHostVersions' \
        -count=1 -v
  )
  if ! jq -e '
    keys == [
      "cw_ics08_wasm_arbitrum.wasm",
      "cw_ics08_wasm_base.wasm",
      "cw_ics08_wasm_eth.wasm",
      "cw_ics08_wasm_op.wasm"
    ]
  ' "$gas_report" >/dev/null; then
    printf 'optimized wasmvm conformance did not produce a complete four-artifact gas report\n' >&2
    exit 1
  fi
  host_conformance="passed pinned wasmvm v2.2.4 signed-update, state-write, response, and gas fixtures"
fi

scripts/validate-wasm-test-manifest.sh

if [[ -n "${ARTIFACT_MANIFEST:-}" ]]; then
  manifest="$ARTIFACT_MANIFEST"
else
  manifest="target/wasm-artifact-validation.md"
fi
mkdir -p "$(dirname "$manifest")"
source_revision="$(git rev-parse HEAD)"
source_tree_state="clean"
if [[ -n "$(git status --porcelain)" ]]; then
  source_tree_state="contains uncommitted implementation changes"
fi
{
  echo "# Wasm artifact validation"
  echo
  echo "Pinned optimizer: \`$optimizer_image\` with Rust \`$optimizer_rust_toolchain\`"
  echo
  echo "Generated: \`$(date -u +'%Y-%m-%dT%H:%M:%SZ')\` from revision \`$source_revision\`; source tree **$source_tree_state**."
  echo
  echo "Validation command: \`VALIDATE_OPTIMIZED=$validate_optimized scripts/validate-l2-clients.sh\`."
  echo
  echo "Environment: \`$(rustc --version)\`; \`$(cargo --version)\`; \`$(go version)\`; \`node $(node --version)\`; wasmvm \`v2.2.4\`; ibc-go 08-wasm \`v10.3.0\`."
  echo
  echo "Reproducibility: **$reproducibility**"
  echo
  echo "Runtime boundary: **Wasm-contract artifacts validated; end-to-end deployment still requires a compatible signed message producer and operational review**."
  echo
  echo "Optimized host-runtime conformance: **$host_conformance**"
  echo
  echo "Upload-size gate: **90% of ibc-go v10.3.0 MaxWasmSize = $approved_upload_limit bytes**."
  echo
  echo "Exact tests and fixture digests: [\`test/fixtures/wasm-contracts/test-manifest.txt\`](../test/fixtures/wasm-contracts/test-manifest.txt)."
  echo
  echo "Gas baseline: [current baseline](../test/fixtures/wasm-contracts/wasmvm-gas-baseline.json); regressions above 10% fail validation. Reports are generated under \`target/\`."
  if [[ "$validate_optimized" == 1 ]]; then
    echo
    echo "| Artifact | Independent pass 1 | Independent pass 2 |"
    echo "|---|---:|---:|"
    for program in "${programs[@]}"; do
      artifact="${program//-/_}.wasm"
      echo "| \`$artifact\` | \`${first_pass_hashes[$artifact]}\` | \`${second_pass_hashes[$artifact]}\` |"
    done
  fi
  echo
  echo "| Artifact | Kind | SHA-256 | Bytes | Imports | Exports |"
  echo "|---|---|---:|---:|---|---|"
  for program in "${programs[@]}"; do
    artifact="${program//-/_}.wasm"
    raw="target/wasm32-unknown-unknown/release/$artifact"
    artifact_paths=("raw:$raw")
    if [[ "$validate_optimized" == 1 ]]; then
      artifact_paths+=("optimized:artifacts/$artifact")
    fi
    for kind_and_path in "${artifact_paths[@]}"; do
      kind="${kind_and_path%%:*}"
      path="${kind_and_path#*:}"
      if [[ ! -f "$path" ]]; then
        printf 'required artifact is missing: %s\n' "$path" >&2
        exit 1
      fi
      checksum="$(sha256sum "$path" | cut -d' ' -f1)"
      bytes="$(stat -c %s "$path")"
      if ((bytes > approved_upload_limit)); then
        printf 'artifact exceeds approved upload limit (%d): %s is %d bytes\n' \
          "$approved_upload_limit" "$path" "$bytes" >&2
        exit 1
      fi
      imports="$(node scripts/inspect-wasm.mjs imports "$path" | paste -sd, -)"
      exports="$(node scripts/inspect-wasm.mjs exports "$path" | paste -sd, -)"
      echo "| \`$artifact\` | $kind | \`$checksum\` | $bytes | \`$imports\` | \`$exports\` |"
    done
    if [[ "$validate_optimized" == 0 ]]; then
      echo "| \`$artifact\` | optimized | not generated by this invocation | - | - | - |"
    fi
  done
} > "$manifest"

printf 'Validation report: %s\n' "$manifest"
