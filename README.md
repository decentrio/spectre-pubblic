# Spectre

A ZK light client that lets Ethereum follow Tendermint consensus, and the Go
relayer and prover that feed it. A purpose-built gnark Groth16 circuit
batch-verifies the validator Ed25519 signatures behind one CometBFT commit — a
2/3+ voting-power quorum, with each `CanonicalVote` reconstructed in-circuit —
so a client update settles on-chain as a single proof carrying ~32 bytes of
public input. Signers are padded up to the nearest fixed circuit size, one
`Groth16Verifier_N{N}` per bucket, for validator sets up to 180 active members.

The client plugs into an [IBC v2](https://github.com/cosmos/ibc/tree/main/spec/IBC_V2)
Solidity stack for packet routing and ICS-20 transfers, and the same relayer
carries the reverse direction against a CosmWasm Ethereum light client on the
Cosmos side (`programs/cw-ics08-wasm-eth`), which follows the beacon sync
committee rather than a Groth16 proof. CosmWasm ICS-08 clients extend the path
to OP, Base and Arbitrum, backed by attestor sidecars.

## Architecture

```
Cosmos                  Go relayer                  Ethereum
──────                  ──────────                  ────────

commit @ H         ──▶  extract top-N Ed25519  ──▶  ICS26Router.updateApplicationState
(validator sigs)        sigs holding ≥ 2/3          └──▶ SpectreClient.updateApplicationState
                        voting power                     └──▶ UpdateClient.sol   (delegatecall)
                        gnark: one Groth16                    └──▶ SignatureVerifier
                        proof, ~32 bytes                           └──▶ Groth16Verifier_N{N}
                        of public input             ⇒ keccak(consensusState) stored per height

send_packet        ──▶  packet + ICS23 proof   ──▶  ICS26Router.recvPacket
                                                    ├──▶ SpectreClient.verifyMembership
                                                    │    └──▶ Membership.sol      (staticcall)
                                                    │         ICS23 against the commitment root
                                                    └──▶ ICS20Transfer.onRecvPacket
                                                         └──▶ Escrow / IBCERC20
```

The two flows are independent, and that is the point of the design: a Groth16
proof is paid once per client update, and every packet relayed against that
update costs only an ICS23 membership check. Nothing on the packet path touches
the verifier contracts.

The Tendermint commit is verified by batch-proving N Ed25519 signatures whose
voting power sums to ≥ 2/3 of the validator set. To keep Groth16 circuits
fixed-size, production retains the bucket topology N ∈ {4, 8, 16, 32, 64,
128}. Each bucket has one inseparable `(r1cs, pk, vk)` set and matching
`Groth16Verifier_N{N}.sol`. The prover lives in its own repository
(spectre-prover, a sibling checkout at `../prover`); its engine discovers which
buckets are servable by scanning its `bin/` for complete artifact triples at
startup — no code change is needed once a bucket's artifacts exist. No artifact
triples are checked in, so a fresh clone must generate or provision them before
the prover can serve any bucket. By default the generator (`cmd/setup-circuits`
in that repository, `-buckets` flag) builds N=4; serving a larger quorum
requires generating that bucket's artifacts, then publishing, deploying, and
registering its `Groth16Verifier_N{N}.sol`.

The store keeps one 32-byte `keccak256(abi.encode(consensusState))` per height,
not the root itself. A packet carries the consensus state and its commitment
root, and `SpectreClient._validateMembershipInput` rehashes what it was given,
compares that against the stored hash, checks the root matches the state, and
checks the trusting period — all before the ICS23 proof is looked at.

The update above is drawn router-managed, which is the access-controlled path
(`ICS02ClientUpgradeable.updateApplicationState` is `restricted` and forwards to
the client). A deployment that is not router-managed has the relayer call
`SpectreClient` directly instead — `relayer/transaction/ethereum.go:670-694`
picks between the two. The packet path always goes through the router.

## Building

### Requirements

- [Go](https://golang.org/) >= 1.26 (`relayer/go.mod` pins 1.26.0, toolchain 1.26.9)
- [Foundry](https://getfoundry.sh/)
- [Bun](https://bun.sh/)
- [Just](https://github.com/casey/just)
- A checkout of spectre-prover next to this repository (`../prover`, or set
  `PROVER_REPO`): the relayer proves through it. Its README lists what it needs
  (the private gnark forks, a Rust toolchain for the Garaga FFI, optionally ICICLE
  for GPU proving)

### The prover

The Groth16 prover is a separate repository and process, spectre-prover (a sibling
checkout at `../prover`). This repository links no gnark and no native library: the
relayer calls the prover's `proverd` over gRPC at `PROVER_ADDR` (or `--prover-addr`),
and `start` refuses to run a Cosmos-sourced path without one. The gnark forks, the
Garaga FFI, the circuit setup tool and GPU proving are documented in that repository.

```bash
scripts/local/run_prover.sh      # build and run proverd on 127.0.0.1:50060 (foreground)
```

The API is defined in the prover repository (`proto/prover/v1/prover.proto`);
`relayer/prover/proverpb` vendors a copy and generates its Go code. After changing
either side, check for drift:

```bash
PROVER_DIR=../prover scripts/check-prover-proto-sync.sh
```

### Build and test

```bash
bun install                  # Solidity dependencies (never npm/yarn)
just build-go-relayer        # go build ./... in relayer/
just build-prover-artifacts  # run spectre-prover's circuit setup (../prover) and emit contracts/verifiers/
just build-contracts         # forge build — needs the generated verifier (see below)

just test-foundry            # all Solidity tests
just test-go-relayer         # all Go relayer tests
just lint                    # solidity + go + buf (`just lint-rust` is separate)
```

The order matters on a fresh clone. `contracts/verifiers/Groth16Verifier_N4.sol`
is produced by `just build-prover-artifacts` and is not checked in, while the
deploy scripts (`scripts/E2ETestDeploy.s.sol`, `scripts/E2ETestDeployL2.s.sol`)
and two integration tests import it. Until it exists, `just build-contracts`
stops with `Source "contracts/verifiers/Groth16Verifier_N4.sol" not found`.

`just --list` shows every recipe, including the per-L2 wasm builds, the
attestor sidecars, and Slither.

## Running it end to end

Bring-up runbooks live in **[docs/E2E.md](docs/E2E.md)** — Cosmos↔Ethereum,
Cosmos↔OP, Cosmos↔Arbitrum, Cosmos↔Base, running against an L2 you did not
deploy, sending test packets in both directions, and the failure modes each
step produces.

## L2 ICS-08 clients

The repository also contains independent CosmWasm clients for OP Sepolia, Base
Sepolia, and Arbitrum Sepolia. Their deployment evidence requirements,
host-query wiring, and verification commands are documented in
[docs/L2_CLIENTS.md](docs/L2_CLIENTS.md).

## Production deployment

See [docs/PRODUCTION_DEPLOYMENT.md](docs/PRODUCTION_DEPLOYMENT.md) before using
the production AccessManager deployment scripts. It documents the governance
timelock requirement, the escrow launch gate, and the scheduled light-client
provisioning flow.

## One relay path per process

`start` serves exactly one source→destination path and refuses a config declaring
more than one, so any real deployment runs several processes side by side.

**Give each `start` process its own `COSMOS_PRIVATE_KEY`.** Every path writes to
Cosmos — even a Cosmos→L2 path that appears to write only to the L2, because its
timeout scanner refunds expired Cosmos-origin packets on Cosmos. Two processes
sharing a key share an account sequence with nothing coordinating them, and the
loser of that race can have a single-packet transaction dropped, stranding the
transfer in escrow until it times out.

`ETH_PRIVATE_KEY` is a weaker rule: nonces are tracked per `{chain id, address}`,
so a Cosmos↔Ethereum process (L1) and a Cosmos↔L2 process (L2) can share one key.
Only processes writing the **same** EVM chain need separate ones.

Set them per process rather than editing `relayer/.env` — `godotenv` does not
override an already-set variable:

```bash
COSMOS_PRIVATE_KEY=$KEY_ETH_PATH ./relayer start --config config.json &
COSMOS_PRIVATE_KEY=$KEY_OP_PATH  ./relayer start --config config.op.json &
```

Full reasoning, including the one other command that shares the Cosmos key:
[docs/E2E.md](docs/E2E.md#signing-keys-when-several-processes-share-a-chain).

## Prover artifacts and GPU proving

Both belong to the prover now. `just build-prover-artifacts`
(`scripts/solidity/build-prover-artifacts.sh`) runs spectre-prover's `cmd/setup-circuits`,
publishes the keys into that checkout's `bin/` (or `PROVER_BIN_DIR`) and only
`Groth16Verifier_N{N}.sol` into `contracts/verifiers/`, and records the pair in
`.artifacts/solidity/prover/provenance.json`. It does nothing when every artifact already
exists unless you pass `--force`, and it cannot use the GPU backend.

**Regenerating prover artifacts changes the verifying key.** However you regenerate, every
deployed `Groth16Verifier_N{N}` must be redeployed and re-registered with
`SignatureVerifier.setBucket(...)`, or every proof fails on-chain. Do not regenerate for a
live deployment without agreeing that redeploy first. `scripts/local/check_prover_pairing.sh`
checks that the keys `proverd` will load match the verifier.

GPU proving (ICICLE, `-tags=icicle`, `GPU_PROVE=1`) is a `proverd` setting; see the
spectre-prover README.

## Logging

Every log line the relayer emits carries a level, so a log viewer can filter and alert
on it. Both settings are global flags, accepted by every subcommand, with an
environment variable each; the flag wins:

```bash
./relayer start --config config.json --log-level warn --log-format json

# Env (equivalent)
RELAYER_LOG_LEVEL=debug RELAYER_LOG_FORMAT=console ./relayer start --config config.json
```

| Flag / env | Values | Default |
|---|---|---|
| `--log-level` / `RELAYER_LOG_LEVEL` | `debug`, `info`, `warn`, `error` | `info` |
| `--log-format` / `RELAYER_LOG_FORMAT` | `json`, `console`, `auto` | `auto`: `console` on a terminal, `json` otherwise |

The variables are read from the process environment, not from `relayer/.env`: that
file is loaded inside each command, after the logger exists. An invalid value never
stops the relayer; it is ignored, reported in the log, and the default is used.

`json` is what a log viewer reads. Each line has `level`, `ts` (epoch seconds),
`caller`, `msg` and, under `start`, `relay_path`. If the relayer runs with a terminal
attached (`docker run -t`), pass `--log-format json` explicitly, or `auto` will pick
`console`.

When a command fails, cobra also prints its own `Error: ...` line and the usage text
as plain lines. The same error is then logged once at `fatal`; alert on that line, not
on the plain text.

gnark logs through a logger of its own, which would otherwise print three console lines
on stdout for every proof. It goes through the same stream: its lines read
`[gnark] <message>`, carry `"component":"gnark"` and no `caller`, and keep gnark's own
fields (`took` is in milliseconds) under a `gnark` key so they cannot shadow `level` or
`msg`. The per-proof lines are `debug`; a warning or error from gnark keeps its level.

A line's level says what it asks of the operator, not how chatty it is:

| Level | Meaning |
|---|---|
| `error` | a person has to act or look: a packet dropped that nothing else refunds (an ack or a timeout), state that could not be made durable and that no later pass re-derives from the chain (an owed acknowledgement, a settlement tombstone, retry state), an operation abandoned, a recovered panic |
| `warn` | something went wrong or is degraded and the relayer is handling it: a retry, a fallback, a skipped check, a resubscribe, a dropped send the timeout scanner refunds, a failed write that the next pass or a restart scan repeats (a recovery cursor, a packet tracked before it is relayed) |
| `info` | lifecycle, progress, and every state change a packet goes through |
| `debug` | output that repeats with nothing new to say, such as the batch builder's `re-queued ... (waiting)` echo and gnark's three lines per proof |

For contributors: the engine logs with `log.Printf`. Start the message with `WARN`,
`ERROR` or `DEBUG` and one space to give it a level, ahead of the `[Label]`; with no
token it is `info`. The token is stripped, so the logged text is unchanged.
`relayer/logbridge` documents the convention, and its test rejects a misspelt token
and an `[ATTENTION]` line that has none.

## Benchmark mode

Detailed per-step gas + timing logs are off by default (production noise) and
opt-in via flag or env:

```bash
# CLI flag
./relayer start --config config.json --benchmark

# Env (equivalent)
RELAYER_BENCHMARK=1 ./relayer start --config config.json
```

Either source turns it on; flag is the override. The relayer logs
`[bench] enabled: detailed gas/timing logs are active` at startup so it's
obvious which mode you're in.

When enabled, the relayer emits the following lines so a single E2E run can be
diffed for performance regressions without rebuilding:

| Prefix | Where | Fields |
|---|---|---|
| `[prover]` | per proof job, written by `proverd` (spectre-prover) in its own log; the relayer adds `connected`, `unreachable while polling` / `reachable again`, and `not found ... resubmitting` lines for the same `proof_id` | `proof_id`, `bucket`, `witness` (queued) / `done in <duration>` |
| `[bench] eth` | per `SendEthTx` / `SendEthTxBatch` (`relayer/transaction/ethereum.go`) | label or `multicall labels=...`, `gasUsed`, `submit`, `wait`, `total`, `tx` |
| `[bench] eth inner[i]` | per multicall inner | `label`, `gas` from `debug_traceTransaction` callTracer |
| `[bench] cosmos` | per `SendCosmosTx` / `SendCosmosTxBatch` (`relayer/transaction/cosmos.go`) | msg type or `batch msgs=N`, `gasWanted`, `gasUsed`, `broadcast`, `total`, `height`, `hash` |

`SendEthTxBatch` packs N inner calls but the receipt only reports the total.
For each multicall, after the tx confirms the relayer calls
`debug_traceTransaction` with the `callTracer`, descends past the UUPS
proxy → impl wrapper frame, and logs one `inner[i]` line per direct child of
the multicall body. The lines look like this (values elided):

```
[bench] eth multicall labels=updateApplicationState,recvPacket:1 gasUsed=<total> ...
[bench] eth inner[0] updateApplicationState gas=<n> (from trace)
[bench] eth inner[1] recvPacket:1 gas=<n> (from trace)
```

`gas=... (from trace)` is the **real on-chain gasUsed** of that inner CALL,
not an estimate.

The inner-gas trace requires the RPC endpoint to expose the `debug_` namespace
(`--http.api=...,debug` on geth). Verify with:

```bash
curl -X POST <rpc> -d '{"jsonrpc":"2.0","method":"rpc_modules","params":[],"id":1}'
# look for "debug": "1.0" in the response
```

Kurtosis ethereum-package presets typically include it; production endpoints
typically do not. If unavailable, the relayer logs one error line and
continues — the rest of the benchmark output is unaffected.

`utils.SetBenchEnabled(true|false)` lets tests force the flag without touching
env. Definitions live in `relayer/utils/bench.go`.
