# Arbitrum Attestor

The attestor is a standalone gRPC backend for the relayer. Like the OP
attestor, it publishes a proposal-independent root from a configured Nitro
`unsafe`, `safe`, or `finalized` head. It also ingests BoLD v2 Arbitrum
assertions from finalized Ethereum L1 blocks by default and checks each assertion's L2
block against configured Nitro HTTP and WebSocket endpoints. It never starts,
supervises, or stops a Nitro process.

The configured Nitro endpoint performs execution and owns all chain state. The
attestor stores only its RollupCore log cursor, unresolved assertions, verified
frontier entries, and mismatch records; it does not maintain an execution
database.

## Trust boundary

For `source="assertion"` entries, the attested root comes from the assertion
read out of `RollupCore` on Ethereum L1. Nitro supplies the runtime head and
canonical-block view used to decide when that assertion can enter or advance in
the feed. The attestor re-reads assertion status at finalized L1 once that
snapshot reaches the assertion's creation block. Opt-in early ingestion also
rebuilds the complete unfinalized L1 window and withdraws orphaned assertions.
Consequently, an untrusted Nitro endpoint cannot fabricate an assertion-backed
root; it can delay, suppress, or reject an otherwise valid assertion by
misreporting its head or canonical view, which is a liveness failure rather
than a new L1 commitment.

`source="derived"` is deliberately different and mirrors OP's configured-head
feed: its root is read directly from Nitro. `attestation_head="unsafe"` trusts
the configured endpoint immediately; `safe` and `finalized` select Nitro's
corresponding L1-derived heads. Set `disable_derived_roots=true` when the feed
must contain only RollupCore-backed assertions.

The L2 client does not verify the chain-specific assertion itself. It accepts a
canonical L2 execution header only with this daemon's Ed25519 signature and
verifies the router account proof against the signed header state root. Thus
`source` and `attestation_head` govern what this daemon will sign; the
permissionless Cosmos transaction sender cannot fabricate the signed block
identity. See `docs/L2_CLIENTS.md` for the exact signature format.

## Prerequisites

Provide an Ethereum L1 RPC and Nitro endpoints for the target Arbitrum chain.
The Nitro HTTP endpoint must support `eth_getBlockByNumber` with `latest`,
`safe`, and `finalized` tags. The WebSocket endpoint must support
`eth_subscribe("newHeads")`. Both endpoints are checked against `l2_chain_id`
at startup.

### Upgrade note

Existing configurations must set `attestation_signing_key` before upgrading.
Configuration validation now rejects a blank key at load time. Prefer an
environment reference such as `"attestation_signing_key": "env:ATTESTOR_SIGNING_KEY"`
so the 32-byte Ed25519 seed is not stored in the configuration file.

Copy `config.example.json` to `config.json` and configure:

- `grpc_listen_address`: attestor gRPC bind address. The example uses loopback
  because the server does not provide TLS or authentication. Direct plaintext
  gRPC is supported only as a same-host sidecar interface.
- `metrics_listen_address`: Prometheus `/metrics` bind address. Use loopback
  and publish the container port only to a loopback host address.
- `runtime_poll_interval`: fallback reconciliation interval for Nitro's unsafe,
  safe, and finalized L2 commitments. New unsafe heads normally trigger
  immediate reconciliation through the WebSocket subscription.
- `runtime_backfill_max_blocks`: maximum L2 heights fetched per unsafe, safe,
  or finalized range in one reconciliation. When the attestor is behind by
  more, the oldest heights are skipped and the newest window is kept, so
  reconciliation stays bounded and attestation keeps pace with the chain.
  Skips are logged (`backfill capped`) and skipped heights later finalize as
  incomplete consistency results. Omitting it uses the default `2048`, roughly
  one L1 finality epoch of Arbitrum blocks.
- `runtime_backfill_concurrency`: bound on concurrent Nitro header fetches
  during the backfill. Omitting it uses the default `8`, chosen to outpace
  Arbitrum block production at typical public-RPC latency while staying under
  free-tier concurrent-request caps (Alchemy's free tier 429s at 16). Raise it
  on a dedicated endpoint; lower it if the endpoint returns 429s — a failed
  fetch retries on the next reconciliation, with backoff, without advancing
  any state. The value must clear `3 × latency × block rate` fetches per
  second or the attestor falls behind (at 385 ms and 4 blocks/s that floor
  is ~5).
- `attestation_head`: Nitro head used for proposal-independent attestations.
  `unsafe` trusts the configured Nitro node and does not wait for an L1
  assertion; `safe` uses Nitro's L1-posted view; `finalized` uses Nitro's
  L1-finalized view. The default is `finalized` when omitted. It applies only
  to derived entries: assertion-backed entries are resolved at Nitro's
  `finalized` head, or at `safe` and served provisional until that head
  reaches the block. `docs/L2_CLIENTS.md` ("Provisional attestations")
  explains why.
- `l1_ingest_head`: Ethereum L1 head used to discover assertions: `finalized`
  (the default), `safe`, or `latest`. This does not change `attestation_head`.
  Assertions discovered before L1 finality stay provisional, even if their
  confirmation is already visible at the ingest head.
- `disable_derived_roots`: disables proposal-independent roots while retaining
  BoLD assertion verification and finalized rechecks of existing derived
  entries.
- `derived_attestation_gap_blocks`: minimum L2 block gap between derived feed
  entries. Omitting it uses the conservative default `150`.
- `max_derived_roots`: maximum confirmed derived entries retained. Provisional
  derived entries and assertion-backed entries are never pruned.
- `src_chain`: source-chain key used by relayer requests.
- `l1_rpc_url`: Ethereum execution RPC used for RollupCore logs and assertion
  status reads. The endpoint must support the `finalized` tag. Early ingestion
  additionally requires the selected tag and `eth_getStorageAt` by block hash;
  unsupported reads fail rather than silently falling back to another head.
- `l1_chain_id` and `l2_chain_id`: expected Ethereum and Arbitrum IDs. Startup
  fails if the connected L1 or RollupCore reports a different chain.
- `attestation_signing_key`: 32-byte Ed25519 seed as hex, or `env:NAME`. Keep
  it outside the config file where practical; pin the matching public key in
  the Cosmos client's immutable profile.
- `rollup_core_address`: deployed Arbitrum RollupCore proxy address.
- `assertions_mapping_slot` and `assertion_status_offset`: reviewed BoLD
  `_assertions` storage layout values. They must match the Cosmos Arbitrum
  verifier profile.
- `assertion_start_block`: first L1 block scanned on a new state database.
  Configure the RollupCore deployment block for full history, or the creation
  block of a known confirmed object when intentionally bootstrapping a frontier.
- `assertion_poll_interval` and `assertion_max_block_range`: L1 scan
  cadence and maximum inclusive block count per log query.
- `assertion_call_timeout`: deadline for each upstream call the assertion loop
  makes to L1 or Nitro, as a Go duration (default `5m`). A call that never
  returns, such as an L1 WebSocket peer that accepts a request and drops the
  response, fails the pass at this deadline instead of parking the loop. The
  failure is logged, naming the call, and counted in
  `assertion_sync_failures_total`, and the next pass retries from the durable
  state. It bounds the call, not the pass: reconciliation starts from the first
  pending assertion on every pass, so a deadline on the whole pass could be spent
  on the same prefix each time and starve the entries behind it and the log scan.
- `attestor_state_path`: durable JSON state for the assertion cursor and
  verified feed.
- `nitro_rpc_url`: Nitro HTTP/HTTPS endpoint used for chain state, block
  lookups, and unsafe/safe/finalized head reads. Point it at one Nitro node, or
  at an endpoint that keeps this client on one backend. `VerifyStateRoot` reads
  the configured head and then the requested block by number in two separate
  requests, and checks only that the block it got has the number it asked for,
  not that it descends from the head it checked. Behind a pool that answers the
  two requests from different nodes, the block it signs is not guaranteed to be
  on the chain of the head it checked. `config.arbitrum-sepolia.json` points at
  a shared public endpoint; use a node you operate for anything beyond testing.
- `nitro_ws_url`: Nitro WS/WSS endpoint used for `newHeads` notifications.

### Early L1 ingestion and recovery

The durable `next_l1_block` cursor advances only through finalized logs. With
`l1_ingest_head="safe"` or `"latest"`, every pass rereads the entire unfinalized
window in bounded log-query chunks, reads statuses at the ingest block hash,
and checks that both L1 snapshots remain canonical before committing that
window atomically. Reorg replacements and head regressions therefore do not
skip previously scanned blocks. RPC and persistence failures publish no partial
speculative snapshot; finalized backfill completed earlier in the pass remains
durable. An extended finality stall increases the window and RPC load.

Promotion still requires assertion confirmation at finalized L1 and a Nitro
`finalized` recheck. Restart durably drops persisted unfinalized assertions before serving gRPC,
without RPCs or backfill. The background loop rebuilds them on its first pass
and retries transient failures. This can briefly lower the frontier seen by
`include_provisional` consumers; storage failures still abort startup. Switching back to `finalized`
withdraws the unfinalized assertion entries. The state format remains version 1.

`include_provisional=true` with early ingestion accepts L1 reorg risk in addition
to an assertion's challenge risk. The Cosmos Wasm client trusts the pinned
attestor signatures and has no provisional flag or automatic root rollback.
Removing a root from this feed cannot revoke a consensus state or packet Cosmos
has already accepted; a conflicting signed update at the same height is rejected,
and explicit authenticated misbehaviour can freeze the client.

This option can advance the assertion-backed frontier sooner. It does not promise
a fixed end-to-end latency gain: posting cadence, Nitro heads and derived roots
also matter. OP now pins its game ingestion to finalized L1 as well; early
ingestion is an opt-in Arbitrum policy, not a missing OP parity feature.

## Shared local L1 and Nitro devnet

For local bridge testing, Arbitrum and OP settle to the same Ethereum L1 in one
Kurtosis enclave. Start the OP stack first because it owns the shared
ethereum-package deployment, then layer Arbitrum onto that enclave:

```sh
# run_optimism_node.sh is optional: include it to share one L1 between OP and
# Arbitrum; omit it and run_arbitrum_node.sh brings the L1 up itself.
./scripts/local/run_optimism_node.sh
./scripts/local/run_arbitrum_node.sh
./scripts/local/run_arbitrum_attestor.sh
```

Run all commands from the repository root. `run_optimism_node.sh` starts
ethereum-package plus OP in the `op-devnet` enclave; when it is skipped,
`run_arbitrum_node.sh` brings the same L1 up in that enclave itself. The local package under
`scripts/local/kurtosis/arbitrum/` accepts that L1's internal execution and
beacon endpoints; it deploys RollupCore and starts a simple Nitro
sequencer/batch-poster/staker without creating another L1.

`run_arbitrum_node.sh` drives that package, waits for shared-L1 finality,
validates the configured BoLD storage layout against a finalized assertion
when one is available, and writes the published endpoints, chain IDs, and
RollupCore metadata to
`.arbitrum-devnet-run/attestor.env`, including the devnet-only signing seed and
the public key that must be pinned in the matching Cosmos client profile.

`run_arbitrum_attestor.sh` automatically sources that handoff, builds the
attestor image, and connects it to the Nitro HTTP and WebSocket endpoints
published by Kurtosis. Its runtime files live in `.arbitrum-attestor-run/`;
assertion state lives in the persistent
`spectre-arbitrum-attestor-data` Docker volume.

The resulting local flow matches OP:

```text
run_*_node.sh     -> .*-devnet-run/attestor.env
run_*_attestor.sh -> external rollup-node endpoint + attestor gRPC on :3001
```

Stopping Arbitrum leaves Ethereum and OP running:

```sh
./scripts/local/run_arbitrum_node.sh --stop
./scripts/local/run_arbitrum_node.sh
```

Redeploying Arbitrum removes and recreates only the two `arb-*` Kurtosis
services. The shared L1 and OP services are preserved, although the previous
RollupCore contracts remain as unreachable history on the development L1:

```sh
./scripts/local/run_arbitrum_node.sh --reset
```

Removing the enclave removes Ethereum, OP, Arbitrum, and their local state:

```sh
kurtosis enclave rm -f op-devnet
```

The first Arbitrum run builds a RollupCreator image from the pinned
`NITRO_CONTRACTS_REF` and pulls the pinned `NITRO_IMAGE`. Override both pins
together only after checking their compatibility. The package uses public
development keys and funds them from the ethereum-package account already
used by the OP external-L1 configuration; none of those keys are suitable for
public networks.

To stop only the attestor, press Ctrl-C in its terminal. This removes its
container but preserves the assertion-state volume. To discard that state,
remove the volume explicitly after confirming it is no longer needed. The
attestor reaches Kurtosis' published L1 and Nitro endpoints
through `host.docker.internal`; it does not need to join Kurtosis' internal
Docker network.

## Run

From this directory:

```sh
cp config.example.json config.json
./scripts/start-attestor.sh ./config.json
```

For canonical Arbitrum Sepolia, start from the checked testnet profile:

```sh
./scripts/start-attestor-docker.sh ./config.arbitrum-sepolia.json
```

The tracked `config.arbitrum-sepolia.json` profile pins Sepolia L1
(`11155111`), Arbitrum Sepolia L2 (`421614`), and
the BoLD Rollup proxy (`0x042B...0Cf4`). It bootstraps at L1 block
`11379731`, which includes a complete confirmed assertion lifecycle. The
public RPC endpoints are convenient defaults but should be replaced by
operator-owned endpoints for sustained log scanning and Nitro synchronization.

The underlying Cobra command is:

```sh
./bin/attestor start --config ./config.json
```

The native launcher builds the attestor and starts it with the selected
configuration. It does not download, launch, or stop Nitro.

## Run in Docker

Keep `config.json` in a directory mounted into the container, then run:

```sh
./scripts/start-attestor-docker.sh ./config.json
```

The image contains only the attestor. `attested-roots.json` is persisted in the
`spectre-attestor-data` Docker volume. The attestor gRPC service is published
at `127.0.0.1:50051` by default. The configured L1 and Nitro endpoints must be
reachable from inside the container.

The launcher can be customized with:

- `ATTESTOR_DOCKER_IMAGE`: image name.
- `ATTESTOR_CONTAINER_NAME`: container name.
- `ATTESTOR_GRPC_PUBLISH`: host gRPC publish address.
- `ATTESTOR_STATE_VOLUME`: persistent attestor-state volume name.

## gRPC API

The protobuf contract is in
`../../proto/attestor/attestor.proto`, with generated Go client code in
`../types/attestor`. Regenerate it from the repository root with
`buf generate --template buf.gen.attestor.yaml`. The reusable Nitro runtime state is in `runtime_state.go`;
the gRPC transport implementation lives in the shared `attestor/grpc` and `attestor/host` packages. The service exposes:

```text
attestor.AttestorService/Info
attestor.AttestorService/AttestedUpTo
attestor.AttestorService/AttestedRootAtOrBelow
attestor.AttestorService/WatchAttested
attestor.AttestorService/VerifyStateRoot
```

A failed call returns its gRPC code and a classified message only (for example
`query Nitro finalized head`). The upstream error behind it is not returned,
because it quotes the Nitro or L1 URL and an RPC provider's API key can be part
of that URL; it is in the attestor's log with URLs reduced to scheme and host
(see the [runbook](../../docs/ATTESTOR_RUNBOOK.md#supported-transport-boundary)).

`AttestedUpTo(src_chain, include_provisional)` returns the highest qualifying
verified commitment. `AttestedRootAtOrBelow` applies an inclusive L2-height
bound so the relayer's header builder can resolve the assertion provenance for
a target height. A response with `found=false` means the attestor has not
accepted a qualifying assertion yet.

For Arbitrum, `root` is the Nitro L2 state root. A proposal-independent entry
has `source="derived"` and no provenance object. When `attestation_head` is
`unsafe` or `safe`, it is provisional until Nitro's finalized head reaches the
same height. The attestor then recomputes the commitment, confirms it on a
match, or replaces it with the finalized block hash/state root and emits a
`HEAD DIVERGENCE` log on a mismatch.

An assertion-backed entry has `source="assertion"` and carries its BoLD
identifier in `assertion_hash`. A pending assertion enters the feed as
`provisional=true` only after its block is canonical under Nitro's safe head.
It becomes non-provisional only after RollupCore reports it confirmed in
finalized L1 state and the same block is canonical under Nitro's finalized
head. Rejected, challenged, or locally mismatched assertions are never
returned.

The chain-specific provenance is a protobuf `oneof`: OP entries carry
`game_index`, Arbitrum entries carry `assertion_hash`, and derived entries may
carry neither.

`VerifyStateRoot` remains available as a lower-level compatibility and
diagnostic RPC. The caller supplies:

- `block_number`
- `expected_state_root` as exactly 32 bytes
- optional `expected_block_hash` as exactly 32 bytes
- `run_mode` as `RUN_MODE_UNSAFE`, `RUN_MODE_SAFE`, or `RUN_MODE_FINALIZED`

The response returns `valid` plus Nitro's canonical block hash and state root.
A block-identity mismatch is a successful gRPC response with `valid=false`; malformed requests,
missing blocks, and Nitro availability failures use gRPC status errors.

Every verification request must name the daemon's configured attestation head exactly.
The service rejects an unspecified or different `run_mode`, and rejects a requested block above
that configured head. Deploy separate daemon/key configurations for different finality tiers.

In the background, the attestor subscribes to Nitro's `newHeads` stream over
the configured WebSocket endpoint. Each unsafe-head event triggers a refresh
of the unsafe, safe, and finalized tags followed by a derived-root attestation
pass. The attestor reconnects and reconciles after a subscription failure, and
retains `runtime_poll_interval` as a fallback because safe or finalized can
advance without a new unsafe block.

The attestor records the first unsafe and safe root observed at each height
until it finalizes. When Nitro advances the finalized head, the runtime compares
the finalized root with both earlier observations and logs a consistency
result. This in-memory result is intended as the source for future alerting;
Nitro remains the persistent owner of chain state.

The standard gRPC health service is also registered for readiness checks. The
route is ready once Nitro has reported the configured `attestation_head` and the
assertion loop is making progress: it has come back around, or one of its calls
to L1 or Nitro has returned, within `assertion_call_timeout` plus two
`assertion_poll_interval`s. A loop whose passes fail still counts as running:
those failures are in the log and in `assertion_sync_failures_total`. So does a
pass that runs for a long time over a long list, because it makes progress with
every call. A loop parked on something that never returns does not, and neither
does a feed that is frozen behind it.

## Operations

Prometheus metrics use the `spectre_arbitrum_attestor_*` prefix and only the
configured `chain` label. They cover Nitro heads and refresh failures, L1
assertion backlog, provisional roots, divergence/reorg records, and durable
save failures. See the shared [attestor operations runbook](../../docs/ATTESTOR_RUNBOOK.md)
for the supported plaintext boundary and the required response to a mismatch.

The relayer builds the same header at every head kind: the canonical Nitro L2
header at the target height plus the router account proof. It never proves a
RollupCore assertion — the client verifies none — so this feed bounds the relay
on two axes instead. `AttestedUpTo` bounds how far it may advance, and
`VerifyStateRoot` answers whether the block it is about to package is the one
this daemon's Nitro connection holds at that height, at the run mode matching
the configured head kind.

## Assertion feed flow

On every assertion poll, the attestor:

1. Reads the BoLD `AssertionCreated`/`AssertionConfirmed` lifecycle only
   through Ethereum's finalized L1 head and advances a persisted cursor in
   bounded ranges.
2. Recomputes every BoLD assertion hash from its event data.
3. Resolves the asserted L2 block by hash in Nitro, then checks the canonical
   block at the recovered height. Pending assertions are checked against the
   safe head.
4. Re-reads `AssertionNode.status` at finalized L1. Confirmed assertions are
   promoted only after the Nitro finalized head covers and matches the block.
5. Serves the resulting frontier through the same oracle-shaped RPCs used by
   the OP attestor.
