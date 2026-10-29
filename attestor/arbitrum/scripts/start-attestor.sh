#!/usr/bin/env bash

set -euo pipefail

usage() {
	cat >&2 <<'EOF'
Usage: start-attestor.sh [CONFIG_PATH]

Builds the attestor and starts it against the Nitro HTTP and WebSocket
endpoints configured in CONFIG_PATH.

Environment:
  ATTESTOR_BINARY       Output/path of the attestor binary.
  ATTESTOR_SKIP_BUILD   Set to 1 to run an existing binary without rebuilding.
EOF
}

fail() {
	printf 'start-attestor: %s\n' "$*" >&2
	exit 1
}

if [[ "${1:-}" == "-h" || "${1:-}" == "--help" ]]; then
	usage
	exit 0
fi
if [[ "$#" -gt 1 ]]; then
	usage
	exit 2
fi

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
attestor_dir="$(cd "$script_dir/.." && pwd)"
requested_config="${1:-$attestor_dir/config.json}"
[[ -f "$requested_config" ]] || fail "config file not found: $requested_config"

config_dir="$(cd "$(dirname "$requested_config")" && pwd)"
config_path="$config_dir/$(basename "$requested_config")"

attestor_binary="${ATTESTOR_BINARY:-$attestor_dir/bin/attestor}"
if [[ "$attestor_binary" != /* ]]; then
	attestor_binary="$attestor_dir/$attestor_binary"
fi

if [[ "${ATTESTOR_SKIP_BUILD:-0}" != "1" ]]; then
	command -v go >/dev/null 2>&1 || fail "go is required to build the attestor"
	mkdir -p "$(dirname "$attestor_binary")"
	printf 'Building attestor at %s\n' "$attestor_binary"
	(
		cd "$attestor_dir"
		go build -o "$attestor_binary" ./cmd
	)
fi

[[ -x "$attestor_binary" ]] || fail "attestor binary is not executable: $attestor_binary"
printf 'Starting attestor with %s\n' "$config_path"
exec "$attestor_binary" start --config "$config_path"
