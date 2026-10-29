# `chain` — multi-chain adapter contract

This package is the **contract** that lets the relayer target any
source/destination chain (Cosmos, Ethereum, and the Arbitrum / Optimism / Base
EVM L2s) without the core knowing that chain's specifics. Implement the
interfaces here, construct your adapter in `cmd/`, and the generic relay loop
(`relay.Module`) drives it. One relayer **process** still serves exactly one
source → destination path (`rejectExtraRelayPaths`, `cmd/config.go`) — this
package is what lets each of those processes be built from the same generic
loop regardless of which chain pair it drives.

**Start here, then open `chain.go`** — it is the single source of truth for the
method signatures. This README is the map.

## The three interfaces

| Interface | Who/what it models | Methods |
|---|---|---|
| `Source` | a chain whose state we prove to a destination | `Chain`, `Subscribe`, `LatestHeight`, `RelayableHeight`, `QueryHeader`, `MembershipProof`, `NonMembershipProof` |
| `Destination` | a chain hosting the light-client-of-source, where we submit | `Chain`, `UpdateClient`, `RelayPackets`, `HasPacketReceipt`, `ClientExpiresAt` |
| `Builder` | the per-source strategy that builds a `ClientUpdate` | `Name`, `Build` |

Data crossing the boundary is deliberately opaque (`[]byte` / small structs) so
each adapter owns its own encoding. **Do not leak chain-specific types across
the interface** — that is the whole point (`Event.Raw` / `RelayPacket.Packet` are
proto-marshaled `channeltypesv2.Packet`; `ClientUpdate.Payloads` is adapter-owned).

## The relay loop (`relay.Module`)

One `Module` drives a single source → destination path, depending only on the
interfaces above (no god-object, unit-testable with mock adapters). Per batch of
source events it:

```
1. track  every SendPacket for timeout recovery                (WithPacketTracker)
2. relayable := Source.RelayableHeight()   — cheap precondition
   • events above relayable  -> re-queue with WAITING backoff, no expensive work
   • none relayable          -> return (no client update, no proof)
3. Builder.Build(...) — prepare an update when the client is behind
4. Source.Membership/NonMembershipProof(...) per provable packet, targeting the
   prepared update height when it is not on-chain yet
   • chain.IsPermanent(err)  -> DROP (timed out; the scanner refunds it)
   • else                    -> re-queue transient
5. FoldingDestination.RelayWithUpdate(...) — update + packets in one atomic tx
   • unsupported/current client -> UpdateClient then RelayPackets fallback
   • chain.IsPermanent(err)  -> DROP    • else -> re-queue transient
```

Alongside `Run`, the module runs three background loops: an **expiry-driven
refresh** (`Destination.ClientExpiresAt`), an optional **timeout scanner**
(`WithTimeoutScanner`), and an optional **periodic forced update**
(`WithPeriodicUpdate`, e.g. pinned-set rotation) — see "Reliability" below.

## Error classification (`errors.go`, `retry.go`)

The module re-queues by default (never drop a valid packet) and only diverges on
explicitly-tagged errors. There are three classes (`retry.go`'s own design note
puts it as "§3.6 of the component design says there are three, and the code had
two"):

| Tag | Meaning | Who acts on it |
|---|---|---|
| `chain.Transient` | transient infra (RPC blip) | relay module: re-queue immediately |
| *(waiting)* | not yet relayable (finality / AppHash H+2 lag) | relay module: re-queue with growing backoff |
| `chain.Permanent` | deterministic (timed-out packet, on-chain revert) | relay module: **drop** (funds still safe via the timeout scanner) |
| `chain.NeedsChange` | not enough resources (out of gas, batch too large, provider refusing a log range) | the adapter call site: applies the paired `ChangeFunc` and climbs its own backoff ladder locally (see `retry.go:24-38,133`) before the failure ever reaches the relay module |

Adapters tag their errors: e.g. a source's `MembershipProof` returns
`chain.Permanent` for a send whose timeout has passed; a destination's
`RelayPackets` wraps `services.ErrPermanentRelayFailure` (on-chain revert) as
`chain.Permanent`. `chain.NeedsChange` is used where the raw failure mode is
"the request itself is too big/expensive," not "the destination is offline" —
e.g. `transaction/ethereum.go`, `chain/evmflush/flush.go`, and
`chain/l2rollup/subscribe.go` all climb their own ladder (`Climb`/`Next`,
`retry.go:144,210`) rather than retrying the same request unchanged. The
generic relay module itself never inspects `NeedsChange`: it only branches on
`chain.IsPermanent(err)` — drop if permanent, re-queue the whole batch
otherwise (`relay/batch.go:343-349`) — so `NeedsChange` handling is resolved
before the module sees the error, not by the module.
`errors.go` still holds the older two-value sentinels (`ErrRetryable`/
`IsRetryable`, `ErrPermanent`/`IsPermanent`) that `retry.go`'s `Retry` type
wraps for backward compatibility.

## Adding a chain = a package here, plus one branch at the composition root

Adapters are constructed **directly**, by their own package's constructor, in
`cmd/`. There is no registry and no factory indirection:

```go
// cmd/build_l2_source.go
source := l2rollup.NewSource(cfg.kind, l2, headKind, /* ... */)
dest := l2rollup.NewDestination(worker, svcCtx, cfg.L2WasmClientID)
builder := l2rollup.NewAttestedBuilder(headerBuilder)
```

`ChainType` still selects the branch — `src_chain` / `dst_chain` in config
resolve to one — but the branch calls the constructor rather than looking up a
factory. Adding a chain means adding a package under `chain/` and one branch in
`cmd/`; `relay/` does not change, which is the property that matters.

Adapters are built by direct constructor calls rather than through a registry of
factories: constructors take the shared `services` context and worker, which a
cfg-only factory could not.

## Principle: wrap, don't rewrite

The existing Cosmos↔ETH code is battle-tested — **wrap it behind the
interfaces, don't reimplement it.** The `groth16` builder calls the current
signature-extract → pinned-set select → `GenerateProof` → assemble pipeline
**unchanged**; `Build` just returns the assembled message as
`ClientUpdate{Height, Payloads}`. The generic reliability machinery (gap-recovery
subscriber, pending tracker, timeout scanner, nonce/sequence serialization, batch
builder) is reused, not rewritten — adapters only supply the per-chain parts
(RPC, event decode, tx build, finality notion). The `services` surface the
adapters reuse is exported in `services/adapter_api.go`.

If a builder needs to read destination trust state (e.g. the `groth16` builder
reads the pinned validator set + trusted consensus from the ETH SpectreClient),
keep that read inside the builder's own package via the shared context — do
**not** add Cosmos-specific reads to the generic `Destination` interface.

## Finality hook: `Source.LatestHeight` / `RelayableHeight`

Two related hooks carry the confirmation policy:

- **`LatestHeight`** — the highest height this adapter is willing to *advance the
  client to* (the finality/trust decision). It **must** be in the same number
  space as `RelayableHeight`, event heights, and `ClientUpdate.Height` (the module
  compares all four against `m.lastHeight`):
  - **Cosmos** (BFT instant finality): the latest committed height.
  - **ETH L1**: the finalized execution block number (the beacon client only
    advances to finalized headers, so latest == relayable here).
  - **EVM L2**: the height at the source's `HeadKind` confirmation policy —
    `Finalized` (L1-finalized L2 height) / `Safe` / `Unsafe` (soft head).
    The selected kind is encoded with the height and passed to the L2 header
    builder; it is an update request, not proof. The wasm client derives the
    accepted level only from authenticated evidence and its stored policy.
- **`RelayableHeight`** — the highest height whose packets can be *proven right
  now*, ≤ `LatestHeight`, encoding each chain's state-availability lag (Cosmos:
  `latest-2` for the AppHash H+2 lag; ETH: the finalized execution block). The
  module uses it as the cheap precondition so it never burns a proof for a packet
  that isn't yet provable.

⚠️ **Confirmation policy trades safety for latency.** Proving at the unsafe head
exposes the bridge to L2 reorgs / sequencer equivocation (a released packet on
the destination against a source state that later reverts). The L2 wasm client
verifies an **attestor signature quorum** (ed25519, threshold-of-N) plus a
router account proof against the L2 header's state root — it does not itself
observe L1, so nothing on-chain distinguishes an unsafe-head attestation from a
finalized one. The guarantee instead comes from **each `HeadKind`/`run_mode`
using disjoint attestor keys**: an attestor daemon configured for `finalized`
refuses to sign a `VerifyStateRoot` request whose `run_mode` doesn't match its
own configured head (`attestor/optimism/adapter/adapter.go`). That per-daemon
check is enforced; cross-tier key separation across an operator's whole fleet
is not currently checked by the relayer config loader (see `docs/L2_CLIENTS.md`)
— it's an operational requirement today, not yet a validated one.

## Reliability behaviors

The generic module provides these reliability properties:

- **Requeue transient / drop permanent** — see the error table above. The relay
  path has no retry budget and no dead-letter set: a packet whose own transaction
  deterministically reverts is dropped (logged as `DROP packet … (permanent)`), and
  the timeout scanner refunds it if it expires. Because a multicall is atomic, a
  permanent BATCH failure retries each packet individually (`relayIsolated`), so a
  poison packet drops alone while its valid siblings still relay. Two cases are
  never dropped: a destination client that is not active holds the direction and
  keeps the packets queued, and an unresolved EVM nonce leaves them in the pending
  tracker to settle on their acknowledgement or time out.
- **Timeout submission has a retry budget and a dead-letter set**
  (`services/pending.go`, `services/timeout_retry.go`). Only a deterministic revert is
  charged; RPC, broadcast and proof-endpoint failures are deferred with backoff and
  never charged. After `maxTimeoutAttempts` (8) charged failures the packet moves to
  the dead-letter set. It is kept, not dropped, because its funds are still escrowed:
  the relayer logs `[ATTENTION] … exhausted timeout attempts`, `relayer status` lists
  it, and `relayer status reactivate` returns it to the scan set with a fresh budget
  (the running relayer must have `status_allow_control` enabled).
- **Timeout recovery** — `WithPacketTracker` records every send and untracks it once
  the relay succeeds (a delivered packet can no longer time out); `WithTimeoutScanner`
  runs the existing `ScanCosmosTimeouts` / `ScanEthTimeouts` on a ticker, refunding
  packets that expired undelivered.
- **Periodic forced update** (`WithPeriodicUpdate`, Cosmos→ETH only) — force-
  rotates the SpectreClient pinned validator set with exponential backoff on
  failure, so a quiet period cannot let the set decay below quorum. It is scheduled
  from the client's **on-chain freshness** (`PinnedSetRotationDueIn` → `initialDelay`),
  not process uptime, so a restart near the deadline rotates promptly; an
  underivable interval is fatal at startup. The beacon client has no pinned set, so
  ETH→Cosmos omits it.
- **Waiting backoff** — a not-yet-relayable packet re-checks with a growing delay
  (3→15s) instead of every batch period, so a long finality wait is quiet and
  does not churn the finality RPC (`services.BatchBuilder` `NotBefore`).
- **Atomic folding + shared serialization** — capable destinations prepend the
  client update to the packet batch in one transaction; non-router EVM and L2
  destinations retain the two-transaction fallback. The shared
  `TransactionHandler` serializes ETH nonce / Cosmos sequence across all sources.
- **Graceful shutdown** — `handleBatch` takes the `Run` ctx and the background
  loops re-check `ctx.Err()` after each timer/tick, so cancellation stops new
  proof/tx work instead of continuing past teardown.
- **Enumeration backstop** (`flushLoop`/`flushOnce`, `relay/module.go`) — for
  sources implementing the optional `chain.PacketLister`, periodically
  enumerates on-chain packet state directly instead of relying solely on the
  event subscription, so a missed/reorged event cannot silently strand a
  packet.
- **Overdue-ack watch** (`ackWatchLoop`, `relay/module.go`, wired via
  `WithAckWatch`/`WithPacketFlush`) — flags packets whose ack is overdue so a
  stuck destination-side ack doesn't go unnoticed between timeout-scanner runs.

## L2 (rollup) adapters

The L2→Cosmos path is **attestor-signed**, not trustless: a configured
threshold of independent attestors sign the canonical L2 header plus a router
account proof at the requested height (ed25519; `l2rollup/header_attested.go`'s
`attestedHeaderBuilder`). The generic `AttestedBuilder` (`l2rollup/builder.go`)
packages the result as `ClientUpdate.Payloads[0]`. The L2 wasm client
(`programs/cw-ics08-wasm-{op,base,arbitrum}`) verifies the attestor signature
quorum against its stored attestor public-key set, then the router account
proof against the header's state root — it does **not** verify any L1
rollup-contract witness, and it does not defer to the shared `cw-ics08-wasm-eth`
client. See `docs/L2_CLIENTS.md` for the full wire contract and the
development-only status gate.

`attestedHeaderBuilder` is the single `HeaderBuilder` implementation, shared by
OP-Stack (op/base) and Arbitrum; only its `name` label (`l2-op`/`l2-base`/
`l2-arbitrum`) and the `profile_version` the wasm client checks differ by
chain — there is no separate `l2-opstack`/`l2-arbitrum` package. The builder
fails closed when the required attestor quorum is unavailable or does not
match the requested L2 height and confirmation policy.

## Status

- **Contract** (`chain.go`, `errors.go`), the **Cosmos↔ETH adapters**
  (`cosmos/`, `evm/`, `codec/`), the generic **`relay.Module`** and the `cmd`
  wiring are in place: `runAdapterEngine` is the sole `start` engine. Both
  directions (Cosmos→ETH groth16, ETH→Cosmos beacon) relay via adapters.
- **L2 adapters** (`l2rollup/`): implemented — attestor-signed header assembly
  (`header_attested.go`), event listener with gap-recovery (`subscribe.go`),
  storage/account proof assembly (`source.go`), and Cosmos destination
  submission (`destination.go`). See the L2 section above and
  `docs/L2_CLIENTS.md`.
- The command layer assembles `services.RelayDeps`, while adapters and transaction
  handlers receive only the scoped `CosmosEndpoint`, `EVMEndpoint`, `ClientIDs`,
  and config values they consume.

**One process serves exactly one source → destination pair**
(`rejectExtraRelayPaths`, `cmd/config.go`) — a config declaring more than one
relay path is rejected at parse time, before any module is even fully parsed.
Multiple Cosmos sources to one ETH destination therefore means **one config
file and one process per source**, each started independently
(`./relayer start --config <path>.json`), sharing only the deployed
contracts/clients, not a process. See CLAUDE.md's `rejectExtraRelayPaths`
section for the private-key and nonce-domain implications of running multiple
processes against the same destination.
