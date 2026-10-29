#!/usr/bin/env bash
# Builds and runs proverd, the Groth16 prover service, from the spectre-prover checkout.
# The relayer proves through it over gRPC (PROVER_ADDR), so it has to be up before
# `relayer start`, `update-client` or `submit-misbehaviour`.
#
#   scripts/local/run_prover.sh
#       Foreground, for a devnet: Ctrl-C stops it.
#
#   scripts/local/run_prover.sh -- <command> [args...]
#       Starts proverd in the background, waits until it answers, runs the command with
#       PROVER_ADDR set, then stops proverd and exits with the command's status. Used by
#       `just test-e2e`.
#
#   PROVER_REPO       the spectre-prover checkout (default ../prover next to this repository)
#   PROVER_BIN_DIR    where proverd loads n{N}/{r1cs,pk,vk}.bin from (default $PROVER_REPO/bin;
#                     proverd's own default is .artifacts/default, this script pins it so the
#                     keys are the ones build-prover-artifacts.sh published)
#   PROVER_AUTO_SETUP default 0 here. proverd generates NEW keys when the directory holds no
#                     artifacts (its own default is on). Those keys match no verifier deployed
#                     from contracts/verifiers, so a wrong PROVER_BIN_DIR must fail, not mint a key.
#   PROVER_LISTEN     listen address (default 127.0.0.1:50060). Keep it on loopback: the API
#                     has no authentication or TLS and every submit starts a prove.
#   PROVER_WAIT_SECS  how long to wait for proverd to start in `--` mode (default 180; it
#                     loads each bucket's proving key, which takes tens of seconds)
#
# libgaraga_rs must exist in the prover checkout: $PROVER_REPO/scripts/build-garaga-ffi.sh.
#
# A prover that already runs elsewhere (a server) needs none of this: skip the script and
# set PROVER_ADDR=host:port for the relayer.
set -euo pipefail

repo_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
prover_repo=${PROVER_REPO:-$repo_root/../prover}
if [[ ! -f "$prover_repo/cmd/proverd/main.go" ]]; then
  echo "run_prover: no spectre-prover checkout at $prover_repo (set PROVER_REPO)" >&2
  exit 2
fi
prover_repo=$(cd "$prover_repo" && pwd)

native_lib="$prover_repo/.artifacts/native"
if [[ ! -d "$native_lib" ]]; then
  echo "run_prover: libgaraga_rs not built; run $prover_repo/scripts/build-garaga-ffi.sh" >&2
  exit 2
fi
export CGO_LDFLAGS="-L$native_lib${CGO_LDFLAGS:+ $CGO_LDFLAGS}"
export LD_LIBRARY_PATH="$native_lib${LD_LIBRARY_PATH:+:$LD_LIBRARY_PATH}"
export DYLD_LIBRARY_PATH="$native_lib${DYLD_LIBRARY_PATH:+:$DYLD_LIBRARY_PATH}"

export PROVER_BIN_DIR=${PROVER_BIN_DIR:-$prover_repo/bin}
export PROVER_AUTO_SETUP=${PROVER_AUTO_SETUP:-0}
export PROVER_LISTEN=${PROVER_LISTEN:-127.0.0.1:50060}

out="$repo_root/.artifacts/bin"
mkdir -p "$out"
(cd "$prover_repo" && go build -o "$out/proverd" ./cmd/proverd)

if [[ "${1:-}" != "--" ]]; then
  if (($# > 0)); then
    echo "usage: $0 [-- command [args...]]" >&2
    exit 2
  fi
  exec "$out/proverd"
fi
shift
if (($# == 0)); then
  echo "usage: $0 [-- command [args...]]" >&2
  exit 2
fi

"$out/proverd" &
prover_pid=$!
stop_prover() {
  kill "$prover_pid" 2>/dev/null || true
  wait "$prover_pid" 2>/dev/null || true
}
trap stop_prover EXIT

host=${PROVER_LISTEN%:*}
port=${PROVER_LISTEN##*:}
deadline=$((SECONDS + ${PROVER_WAIT_SECS:-180}))
until (exec 3<>"/dev/tcp/$host/$port") 2>/dev/null; do
  if ! kill -0 "$prover_pid" 2>/dev/null; then
    echo "run_prover: proverd exited before it started listening on $PROVER_LISTEN" >&2
    exit 1
  fi
  if ((SECONDS >= deadline)); then
    echo "run_prover: proverd is not listening on $PROVER_LISTEN after ${PROVER_WAIT_SECS:-180}s" >&2
    exit 1
  fi
  sleep 1
done

# Not exec: the trap above has to run when the command ends.
status=0
PROVER_ADDR="$PROVER_LISTEN" "$@" || status=$?
exit "$status"
