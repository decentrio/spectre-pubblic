# OP Stack Attestor — Operator Guide

Everything needed to deploy, configure, and operate the attestor. For the
component overview, code layout, and gRPC API, see the [README](README.md);
design rationale lives in the upcoming Ethereum L2 architecture doc.

Contents: [Quickstart](#quickstart) · [Configuration](#configuration-reference)
· [Replica setup](#replica-setup-op-node--op-reth) ·
[Deployment topology](#deployment-topology-sidecar--grpc) ·
[Healthy operation](#healthy-operation) ·
[Low-latency profile](#low-latency-operation--the-fastest-safe-profile) ·
[Failure drills](#failure-drills) · [Local devnet](#local-devnet-testing) ·
[Metrics](#metrics-reference)

## Quickstart

```bash
just build-attestor                    # binary at attestor/optimism/attestor — no cgo, no LD_LIBRARY_PATH
./attestor/optimism/attestor --config attestor/optimism/config.json
```

The binary reads its own config file (template: `attestor/optimism/config.example.json` —
the attestor no longer shares the relayer's `config.json`), consuming the
`op_source` modules and the `server` block. Minimal OP Mainnet config:

```json
{
  "modules": [
    {
      "name": "op_source",
      "src_chain": "op-mainnet",
      "config": {
        "l1_rpc_url": "https://ethereum-rpc.publicnode.com",
        "op_node_rpc_url": "http://127.0.0.1:9545",
        "dispute_game_factory": "0xe5965Ab5962eDc7477C8520243A95517CD252fA9",
        "respected_game_type": 8,
        "attestation_head": "finalized",
        "l2_chain_id": 10,
        "attestation_signing_key": "env:ATTESTOR_SIGNING_KEY",
        "state_path": "./op-mainnet.attested-roots.json"
      }
    }
  ],
  "server": { "address": "127.0.0.1", "port": 3000, "grpc_port": 3001 }
}
```

**Read `dispute_game_factory` and `respected_game_type` fresh from the
chain** — governance can change them, and a wrong game type silently skips
every proposal (visible as `games_ingested_total` rising while
`pending_games` stays 0):

```bash
cast call <OptimismPortal> 'disputeGameFactory()(address)'  --rpc-url $L1
cast call <OptimismPortal> 'respectedGameType()(uint32)'    --rpc-url $L1
```

(OP Mainnet, measured 2026-07-22: portal v5.6.1, game type **8** — not legacy
CANNON=0. Type-8 root claims commit to the standard v0 single-chain output
root, so the byte-compare applies unchanged.)

Or skip all of the above with the scripted bring-up:

```bash
L1_RPC_URL=https://... OP_NODE_RPC_URL=http://127.0.0.1:9545 \
L2_CHAIN_ID=10 ATTESTOR_SIGNING_KEY=<32-byte-ed25519-seed-hex> \
  ./scripts/local/run_op_attestor.sh
```

## Configuration reference

One `op_source` module per tracked chain; several sources run in one process.
`src_chain` (module-level) is the metrics label and gRPC key — must be unique,
as must `state_path` (both enforced at config load).

| Key | Default | Meaning |
|---|---|---|
| `l1_rpc_url` | required | Ethereum L1 execution RPC (factory reads): HTTP(S), WS(S), or an absolute IPC socket path |
| `op_node_rpc_url` | required | The verify-mode replica's op-node RPC: HTTP(S), WS(S), or an absolute IPC socket path |
| `dispute_game_factory` | required | L1 `DisputeGameFactory` address |
| `respected_game_type` | 0 | Only games of this type are verified; others counted + skipped |
| `attestation_head` | `finalized` | Gating head: `finalized` \| `safe` \| `unsafe` (see below) |
| `poll_interval_seconds` | 30 | Attestation-pass cadence |
| `call_timeout_seconds` | 300 | Deadline for each upstream call the loop makes to the op-node, the dispute-game factory and the challenge hook, the first-run bootstrap included. A call that never returns (an op-node or L1 WebSocket peer that accepts a request and drops the response) fails the pass at this deadline instead of parking the loop. It bounds the call, not the pass: a pass of many calls that each return is never cut |
| `state_path` | required | JSON state file (cursor, pending, rechecks, feed) |
| `l1_bootstrap_lookback_blocks` | 7200 | First-run ingest window (~24 h of L1 blocks) |
| `l1_ws_url` | off | Optional `DisputeGameCreated` wake hint; poll loop fully correct without it |
| `disable_derived_roots` | false | Turn off self-derived attestations (feed then depends on proposer cadence) |
| `derived_attestation_gap_blocks` | 150 | Min L2-block gap between derived attestations (150 ≈ 5 min) |
| `max_derived_roots` | 1000 | Confirmed derived entries kept (oldest pruned; game + provisional entries never pruned) |
| `l2_chain_id` | required | L2 chain ID included in every signature's domain; it must equal the Cosmos client profile. |
| `attestation_signing_key` | required | 32-byte Ed25519 seed as hex, or `env:NAME` to read it from the daemon environment. Its matching public key is pinned in the Cosmos client profile. |

`l1_ws_url` is intentionally WebSocket-only. Absolute IPC socket paths are
supported for `l1_rpc_url` and `op_node_rpc_url`; use a URL transport when a
WebSocket subscription is required.

`attestation_head` is enforced by this daemon and by the relayer, not by the
Cosmos client. The daemon refuses any `VerifyStateRoot` whose `run_mode` differs
from it, and the relayer's `head_kind` must match it (`start` compares the two
through the daemon's `Info`). The tier is **not** part of the signed statement,
and the client profile carries no tier either.

Give each tier its own `attestation_signing_key`. A signature binds the L2 chain
ID, the router, the complete attestor-set hash (its threshold and every key) and
the block identity, so a signature already made for one client does not verify
at a client with a different router, key set or threshold. A shared key matters
for what the daemon signs next: it takes the router and the set hash from the
request, so any caller that can reach a weaker-tier daemon can ask it for a
signature bound to the stricter client's router and set hash, and that signature
verifies there. The stricter client accepts a header only with exactly
`threshold` valid signatures from distinct members of its set. If weaker-tier
daemons hold at least that many of its keys, they alone can authenticate a block
the stricter tier would refuse; with fewer, each shared key is still one of the
`threshold` signatures that any caller can obtain, which lowers by one the number
of other keys that would have to sign such a block. Two tiers with the same keys
and threshold on the same router have identical statements, so every signature
is valid at both. Neither the daemon nor the relayer detects any of this. See
[docs/L2_CLIENTS.md](../../docs/L2_CLIENTS.md#signed-update-contract) for the
statement layout.

`server` block: `port` serves Prometheus on `/metrics`; `grpc_port` serves the
sidecar API (0/omitted disables either).

### Trust levels

`attestation_head` picks the speed/assurance point:

| Head | Derived from | Latency | Verdicts |
|---|---|---|---|
| `finalized` (default) | **Finalized L1 data** — "tx finality", irreversible | ~20–40 min | Confirmed immediately |
| `safe` | L1 data, but the L1 block may still reorg before finality | Faster | Provisional until the finalized recheck |
| `unsafe` | **Sequencer gossip** | Seconds | Provisional (sequencer trust) until the finalized recheck |

Anything attested below the finalized head is **provisional**: re-derived
automatically once finalized covers it, corrected + alarmed
(`head_divergence_total`) on contradiction — the finalized result always
wins. Consumers opt in via `include_provisional` and must treat provisional
roots as reversible.

## Replica setup (op-node + op-reth)

The attestor's guarantee is "**my own** replica derived this from L1" — the
replica must be operator-owned, same host or private network. The attestor
only talks to op-node's RPC (no public provider exposes those methods
anyway). Follow the official
[run-a-node-from-source guide](https://docs.optimism.io/operators/node-operators/tutorials/run-node-from-source)
for version-current commands; below is the attestor-relevant summary (checked
2026-07-22).

**Execution client**: **op-reth** — op-geth reached end-of-support 2026-05-31
and cannot follow OP Mainnet past the Karst hardfork. (The attestor is
EL-agnostic; it only queries op-node.)

**Prerequisites**: an L1 execution RPC + an L1 beacon API serving blob
sidecars (blobs expire after ~18 days — a snapshot older than that can't
derive the gap without an archive blob source). Hardware: ≥16 GB RAM, ~700 GB
SSD for OP Mainnet (+~100 GB / 6 months). **No archive mode anywhere** — the
attestor only needs proofs at blocks ~30 min to a couple of days old, served
by a full node via the proof window below. Snap sync is fine.

```bash
git clone https://github.com/ethereum-optimism/optimism.git && cd optimism
git checkout op-node/v1.18.2          # latest coordinated release tag
cd op-node && just && cd ..
cd rust/op-reth && cargo build --release --bin op-reth && cd ../..
openssl rand -hex 32 > jwt.txt

# Execution layer:
./rust/target/release/op-reth node \
  --chain=optimism --datadir=$DATADIR --http --ws \
  --authrpc.jwtsecret=./jwt.txt \
  --rpc.eth-proof-window=100000

# Derivation driver (the endpoint the attestor uses):
./op-node/bin/op-node \
  --l1=$L1_RPC_URL --l1.rpckind=$L1_RPC_KIND --l1.beacon=$L1_BEACON_URL \
  --l2=ws://localhost:8551 --l2.jwt-secret=./jwt.txt --l2.enginekind=reth \
  --network=op-mainnet --syncmode=execution-layer \
  --rpc.addr=127.0.0.1 --rpc.port=9545
```

Notes the generic guide won't tell you:

- **`--rpc.eth-proof-window` matters**: `optimism_outputAtBlock` uses
  `eth_getProof` of the `L2ToL1MessagePasser`, served only inside this
  window. Proposals commit to ~1 h-old blocks, verdicts wait for the
  finalized head, restarts re-verify older pending games — size it for the
  worst-case lag (`100000` ≈ 2.3 days at 2 s; flag since op-reth v2.2.3).
  Symptom of too-small: `optimism_outputAtBlock` errors on old pending games
  while fresh ones attest fine.
- **Unsafe mode needs p2p** (on by default in op-node) — the unsafe head
  comes from sequencer gossip. Safe/finalized work with p2p off.
- **Keep op-node's RPC private** (bind 127.0.0.1 / firewall); sequencer mode
  stays off — the replica must remain a pure verifier.
- Snap sync takes hours; the attestor can run during sync — everything stays
  `pending` until the heads catch up (designed fail-safe posture).

Ready check — all three heads present and advancing on repeated calls:

```bash
curl -s -X POST -H 'Content-Type: application/json' \
  --data '{"jsonrpc":"2.0","id":1,"method":"optimism_syncStatus","params":[]}' \
  http://127.0.0.1:9545 | jq '.result | {unsafe: .unsafe_l2.number, safe: .safe_l2.number, finalized: .finalized_l2.number}'
```

## Deployment topology (sidecar + gRPC)

The attestor runs as a **standalone sidecar** co-located with the replica;
the relayer consumes the feed over gRPC — the same consume-a-verifier
relationship it has with the prover.

```
[L1 node] ← derives ← [op-node + op-reth replica] ← RPC ← [attestor binary] ← gRPC ← [relayer start]
                       (same host / private net)          feed + metrics          (op_to_cosmos, future)
```

- `server.grpc_port` enables the API (plaintext gRPC — localhost or private
  network only); HTTP metrics stays on `server.port`. Every production
  request must name `src_chain`; the daemon never infers it from one feed.
- Service definition: `proto/attestor/attestor.proto` (regenerate via
  `buf.gen.attestor.yaml`; generated code is never hand-edited). Methods:
  `Info`, `AttestedUpTo`, `AttestedRootAtOrBelow`, `WatchAttested` — see the
  [README](README.md#grpc-api-attestorattestorservice).
- All API methods are read-only; the attestation loop is the only writer.
- Smoke: `grpcurl -plaintext localhost:3001 attestor.AttestorService/Info`.

## Healthy operation

First start logs `bootstrapped ingest cursor at game index N of M`; restarts
resume from `state_path` instead. Then, in order of appearance:

- `ingested proposal: game …` as the factory window is scanned (OP Mainnet
  proposes ≈ 1 game/hour);
- `replica_finalized_head` climbing on `/metrics`, `attestation_lag_blocks`
  falling;
- `derived root attested at l2 block …` every derived gap once the gating
  head is covered — this is the feed the relay path consumes;
- `attested game N …` roughly hourly — the proposal watchdog agreeing with
  the proposer.

Query the feed the way the relayer will:

```bash
grpcurl -plaintext -d '{}' 127.0.0.1:3001 attestor.AttestorService/Info
grpcurl -plaintext -d '{"src_chain":"op-mainnet"}' 127.0.0.1:3001 attestor.AttestorService/AttestedUpTo
```

With **no replica reachable**, the correct posture is: proposals ingest,
`pending` fills, `attestation pass failed (nothing affirmed, retrying in …)`
with backoff 1 m → 15 m, feed stays empty. Nothing is ever affirmed by a down
or lagging replica.

A call that never returns is cut at `call_timeout_seconds` and then behaves the
same way: `attestation pass failed (…): op-node SyncStatus exceeded its 5m0s
deadline: …`, `pass_failures_total` and the backoff above. The deadline is per
call, so a pass over many games takes as long as its calls take and is never cut
for that. Once a replica snapshot exists, the gRPC health service turns
NOT_SERVING if neither the loop nor any of its upstream calls has made progress
for longer than `call_timeout_seconds` plus two `poll_interval_seconds`; a loop
whose passes fail keeps reporting SERVING with the heads of its last good
snapshot.

### Fail-safe invariants

What "healthy" rests on — the attestor fails toward *unaffirmed*, never
toward *wrong*:

- The ingest cursor **never advances** past a failed fetch.
- A game leaves `pending` only via an **explicit verdict** — replica errors
  keep it pending forever.
- A down or lagging replica **affirms nothing**; games pile up in pending by
  design (visible as `attestation_lag_blocks`).
- State is one **atomic-rename JSON file**; a corrupt file fails loudly at
  startup. Delete it to re-bootstrap — re-attesting old games is safe,
  silently resetting is not.

## Low-latency operation — the fastest safe profile

To serve a fresh root within seconds of L2 block production:

```json
"attestation_head": "unsafe",
"poll_interval_seconds": 2,
"derived_attestation_gap_blocks": 5
```

Measured on the local devnet: with gap 1 the feed attested every consecutive
L2 block ~1–2 s after production. **Gap 5–10 (10–20 s) is the recommended
production setting** — near-identical freshness with a 5–10× smaller
provisional backlog (provisional entries are never pruned, and each
finalized-recheck confirmation is a separate state-file write).

What keeps this fast profile safe — all four are load-bearing:

1. **Local execution still checks the block**: the replica executes every
   unsafe payload via the engine API, so an invalid state transition is
   rejected at your own node. What unsafe does NOT yet check: data
   availability and sequencer equivocation.
2. **Provisional is a contract, not a label**: every unsafe-head root is
   served `provisional=true` until the finalized re-derivation confirms it —
   consumers opt in explicitly and never use provisional roots for
   irreversible downstream effects.
3. **The finalized recheck is authoritative**: ~15–40 min later every
   provisional entry is re-derived from finalized L1; contradiction corrects
   the feed and fires the alarm. Ethereum finality is the floor no config can
   move — "fast" means fast *provisional*, not fast finality.
4. **Alert on `head_divergence_total`**: in this profile it is your only
   early-warning signal for sequencer equivocation. Non-zero = incident; stop
   consuming provisional roots until understood.

Operational requirements: op-node p2p enabled, and mind the L1 RPC rate —
every poll tick calls `gameCount()` (2 s poll ≈ 43k L1 calls/day; fine on
your own node, meterable on a provider).

## Failure drills

Run these before trusting a deployment — each must leave the feed
*unaffirmed rather than wrong*:

- **Restart**: kill mid-run and restart. Must resume from the state file —
  same cursor, same pending set, no re-affirmation, no skipped games. A
  corrupt state file fails loudly at startup; deleting it re-bootstraps from
  the lookback window (re-attesting old games is safe).
- **Mismatch / alarm path**: don't wait for a real invalid proposal — run a
  mock op-node (a ~20-line JSON-RPC stub answering `optimism_syncStatus` with
  huge heads and `optimism_outputAtBlock` with junk; same shape as
  `newFakeOpNode` in `opstack/replica_test.go`), point `op_node_rpc_url` at
  it, set `"disable_derived_roots": true` so the junk can't enter the feed as
  derived roots. Every pending game must log `OUTPUT ROOT MISMATCH`,
  increment `roots_mismatched_total`, and land in the state file's
  `mismatches` — with the attested feed staying empty.
- **Unsafe/safe mode**: against a real replica, attestations appear
  `provisional=true` (+`roots_provisional_total`), then
  `finalized head confirmed …` clears the flag. `head_divergence_total` must
  stay 0 on a healthy chain; non-zero means finalized derivation contradicted
  a provisional result (sequencer divergence at unsafe, an L1 reorg at
  safe) — treat as an incident.
- **Replica outage**: stop the op-node mid-run. Pending freezes (no verdicts,
  no cursor movement), `replica_errors_total{kind="rpc"}` climbs, backoff
  engages; everything resumes untouched when the replica returns.

## Local devnet testing

The fastest way to see real verdicts (minutes, no mainnet sync):

```bash
./scripts/local/run_optimism_node.sh     # full local OP Stack via Kurtosis:
                                         # L1 + sequencer + replica + proposer
                                         # posting real dispute games every 60s
source .op-devnet-run/attestor.env
./scripts/local/run_op_attestor.sh       # attestor against the devnet replica
```

`run_op_attestor.sh` also does production bring-up: point it at an existing
replica with `OP_NODE_RPC_URL=...`, or let it start op-reth + op-node itself
(`OP_RETH_BIN`/`OP_NODE_BIN`). It resolves the factory + game type fresh from
the chain via `cast`, generates the config, and health-checks metrics + gRPC.
Knobs: `ATTESTATION_HEAD`, `POLL_INTERVAL_SECONDS`, `DERIVED_GAP_BLOCKS`,
`METRICS_PORT`, `GRPC_PORT`, `ATTESTOR_RUN_DIR` — see the script header. Prefer
`ATTESTOR_RUN_DIR` over the legacy `RUN_DIR`: the devnet bring-up scripts use
`RUN_DIR` for their own artifacts, so exporting it into a shared shell sends
their package clones and handoffs into the attestor's directory.

Cheaper live-verdict environment than mainnet: an **OP Sepolia** replica
syncs far faster. Same config shape — but read that chain's factory address
and `respectedGameType()` first; both differ per chain.

Pure ingest smoke with no replica at all: public L1 RPC + a dead
`op_node_rpc_url` port — proposals ingest and everything stays pending (the
[fail-safe posture](#fail-safe-invariants)).

## Metrics reference

All under `spectre_op_attestor_*`, labeled `chain` (= `src_chain`):

| Metric | Type | Meaning |
|---|---|---|
| `games_ingested_total` | counter | Games read from the factory (all types) |
| `roots_matched_total` / `roots_mismatched_total` | counter | Proposal verdicts |
| `roots_derived_total` | counter | Self-derived roots attested at the configured head |
| `roots_provisional_total` | counter | Below-finalized attestations awaiting recheck |
| `head_divergence_total` | counter | Finalized rechecks contradicting a provisional result — **alert on non-zero** |
| `replica_errors_total{kind}` | counter | Replica interaction failures |
| `replica_finalized_head` / `replica_safe_head` / `replica_unsafe_head` | gauge | Replica heads |
| `attestation_lag_blocks` | gauge | Oldest pending L2 block − gating head |
| `pending_games` | gauge | Ingested respected-type games awaiting a verdict |
