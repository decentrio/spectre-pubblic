# Authenticated L2 ICS-08 clients

Spectre builds three checksum-distinct 08-wasm artifacts with unchanged filenames:

- `cw-ics08-wasm-arbitrum`;
- `cw-ics08-wasm-base`; and
- `cw-ics08-wasm-op`.

The artifacts share one authenticated client kernel. Their data-only profiles select the chain ID,
router, commitment slot and canonical execution-header fork.

> **Development boundary:** the contracts, relayer and attestor use the authenticated signed wire
> contract described below. The stack remains development-only until the manual release gate,
> operational key-custody review and authenticated OP/Base/Arbitrum E2E all pass against the exact
> candidate diff.

## Live 08-wasm surface

Each L2 artifact exports only the entry points used by the current client lifecycle:

- `instantiate` stores explicitly supplied client and consensus state;
- `sudo` handles update, misbehaviour, membership and non-membership operations; and
- `query` handles client-message verification, misbehaviour checks, timestamps and status; and
- `migrate` validates or atomically replaces the active attestor set during a governed code
  migration.

There is no `execute` entry point. Client upgrade and substitute-client recovery sudo messages
return typed unsupported-operation errors.

Light-client responses contain data only. ibc-go's 08-wasm keeper rejects attributes, events and
messages returned by a light client.

## Creation and profile

Creation uses the standard direct byte fields:

```text
InstantiateMsg { client_state, consensus_state, checksum }
```

The decoded client state contains `latest_height`, an optional `frozen_height`, one immutable
artifact profile and the active attestor configuration:

```json
{
  "latest_height": 1,
  "frozen_height": null,
  "profile": {
    "common": {
      "l2_chain_id": 11155420,
      "l2_router": "0x645280885749dc97ea461de280eb3273c91d36df",
      "commitment_slot": "0x1260944489272988d9df285149b5aa1b0f48f2136d6f416159f840a3e0747600",
      "profile_version": "op_attestor_v1",
      "l2_header_fork": "prague"
    }
  },
  "attestors": {
    "public_keys": ["<base64 Ed25519 public key>"],
    "threshold": 1
  }
}
```

The only accepted profile identifiers are `op_attestor_v1`, `base_attestor_v1` and
`arbitrum_attestor_v1`. Public keys must be unique, byte-sorted, exactly 32 bytes each and contain
at most 32 members. The threshold must be between one and the member count.

Creation and `replace_attestors` also refuse keys that no honest attestor holds
(`AttestorConfig::validate_for_install`, `packages/l2-client/src/state.rs`): the ten encodings of
small-order points, and any `y` coordinate at or above the field prime. A small-order key such as
the identity verifies a signature nobody made, so one such attestor would let anyone sign for it. The
check applies only when a set is installed. A client that already holds such a key keeps loading, and
should be rotated away from it.

These development artifacts are fresh-authenticated-state-only. A client state missing `attestors`
is rejected with a typed error before any migration write; create a fresh authenticated client
instead. The strict signed-header and required-attestors schemas prevent unsigned state or messages
from crossing into this client; the `_v1` profile suffix is not treated as a security boundary.

Creation also validates revision-zero nonzero height, nonzero roots and block hash, and equality
between the client latest height and bootstrap consensus height. It performs no host or L1 query.

## Governed attestor rotation

Attestors can change only while governance migrates the client to a different checksum through
IBC-go's authority-only `ibc.lightclients.wasm.v1.MsgMigrateContract`. The `msg` bytes use one of
these strict JSON payloads:

```json
{"keep_attestors":{}}
```

```json
{
  "replace_attestors": {
    "public_keys": ["<base64 Ed25519 public key>"],
    "threshold": 1
  }
}
```

`keep_attestors` validates the loaded client and leaves storage byte-identical.
`replace_attestors` validates the replacement with the same rules as instantiation, then rewrites
only the client-state envelope. Height, frozen status, profile, checksum bytes visible to the
contract, and every consensus state remain unchanged. IBC-go commits the new checksum after the
entry point succeeds. Invalid replacements fail before any write, and the old set stops being
accepted immediately after a successful replacement.

Rotation applies only to an already-valid authenticated client. It cannot add attestors to legacy
unsigned state; both `keep_attestors` and `replace_attestors` fail without writes for that input.

The checksum has to be new every time. ibc-go refuses a migration to the checksum the client already
runs (`ErrWasmCodeExists`, `modules/light-clients/08-wasm/v10@v10.3.0/keeper/keeper.go:154-156`) and
refuses to store code whose checksum it already holds (`keeper.go:109-111`). A second rotation
therefore needs a built artifact whose bytes, and so checksum, differ from the one the client runs,
even though the attestor set is the only thing that changes.

This operation is a proactive rotation mechanism: use it before a retiring key can be abused, or
to restore liveness when an unavailable key has not already produced untrusted updates. It is not
post-compromise recovery. A frozen client stays frozen, and consensus states accepted under the old
key remain in the client store. If a key may already have been abused, the current contract must be
replaced by a newly created client, including the packet sequence and receipt coordination described
in the E2E runbook. A follow-up recovery PR will implement IBC-go's authority-only
`MsgRecoverClient`/`migrate_client_store` substitute-client flow; that lifecycle operation remains
unsupported in these artifacts.

## Signed update contract

Update and client-message verification consume this shape inside the tagged `ClientMessage`
envelope:

```text
ClientMessage::Header(SignedAttestedL2Header {
    l2_header: CanonicalEvmHeader,
    router_proof: EvmAccountProof,
    attestor_signature: Vec<IndexedAttestorSignature>
})
```

Every update carries exactly `threshold` signatures in strictly increasing attestor-index order.
The relayer queries every configured endpoint concurrently and stops when a valid quorum is
available; one slow or unavailable endpoint therefore cannot prevent a healthy threshold from
building a header. `RelayableHeight` uses the same endpoint set rather than endpoint zero: it
returns the minimum frontier reported by the first complete quorum, so every member of that quorum
attests through the selected height. If fewer than `threshold` endpoints remain available, the
relay fails closed instead of falling back to an unsigned or single-attestor update.

The contract signs the fixed-width statement:

```text
SHA256("SPECTRE_L2_ATTESTATION_V1")
|| u64be(l2_chain_id)
|| l2_router[20]
|| SHA256(u16be(threshold) || u16be(member_count) || sorted_public_keys)
|| u64be(block_number)
|| block_hash[32]
|| state_root[32]
```

The statement remains exactly 164 bytes. Finality (`unsafe`, `safe`, or `finalized`) is an
off-chain selection policy and is intentionally not another signed field. Therefore every
finality tier must use disjoint attestor/KMS keys. The statement binds the router and the whole
attestor-set hash, threshold included, so sharing a key does not make a signature that was already
made valid at another client. The exposure is what a daemon signs on request: it takes the router
and the set hash from the request, so a weaker-tier daemon can be asked for a signature bound to a
stricter client's router and set hash, and that client accepts a header on `threshold` such
signatures. Weaker-tier daemons that hold that many of its keys can authenticate a block it would
refuse; each shared key below that still lowers by one the number of other keys that would have to
sign such a block. Each attestor daemon rejects
`VerifyStateRoot` requests whose `run_mode` does not exactly match its configured attestation
head (`attestor/optimism/adapter/adapter.go`), so a single daemon can't be tricked into signing
for the wrong tier. **Current limitation:** the relayer config loader does not itself check for
public-key overlap across `head_kind` values or across config files/processes — a single config
only ever declares one relay path (`relayer/cmd/config.go`), so there is no cross-tier
comparison to make within one file, and nothing today compares keys across an operator's
multiple config files or processes. Cross-tier key separation is presently an operational
discipline the operator must enforce, not a validated invariant.

Signature structure and Ed25519 verification complete before router-proof traversal. The contract
then validates the configured header fork, derives the L2 block hash and verifies the router
account proof against the authenticated state root. Unsigned, malformed, duplicate-index,
under-threshold and wrong-context messages fail without writes.

Updates may advance the latest height or backfill an absent historical height. Re-submitting the
same block is idempotent; a different identity at an existing height is rejected. Misbehaviour
requires two independently authenticated, conflicting headers before the client can freeze.

## Provisional attestations

Every `AttestedRoot` carries a `provisional` flag: the attestor has verified the root, but its
irreversible-finality check has not passed yet. The relayer's `include_provisional` setting decides
whether provisional roots may advance its frontier. Which check applies, and so what a provisional
root can still lose, depends on the root's `source`:

| `source` | Chain | Provisional until | Can still be lost to |
| --- | --- | --- | --- |
| `game`, `derived` | OP | the replica's finalized head reaches the L2 block | an L1 reorg; at `unsafe`, a sequencer divergence |
| `derived` | Arbitrum | Nitro's finalized head reaches the L2 block | an L1 reorg; at `unsafe`, a sequencer divergence |
| `assertion` | Arbitrum | RollupCore reports the assertion confirmed at finalized L1 **and** Nitro's finalized head reaches the L2 block | a lost challenge; with an early `l1_ingest_head`, also an L1 reorg |

For `game` and `derived` roots the finalized recheck either confirms the root or replaces it and
logs `HEAD DIVERGENCE`. These failures are probabilistic. An `assertion` root is different: it stays
provisional for the assertion's whole challenge period, however old its L2 block, and is removed
from the feed if the assertion is rejected or stops matching Nitro. That failure is adversarial.

A provisional root that is later lost cannot be undone on Cosmos. Neither the signed header nor the
Wasm consensus state carries `provisional`, assertion provenance or an L1 ingest head; the Wasm
client trusts its active attestor set for block identity. Removing a root from the attestor feed
therefore does not revoke a consensus state Cosmos has already stored, nor reverse packets
processed against it. Conflicting same-height updates are rejected, and freezing the client
requires explicit authenticated misbehaviour.

**A replaced root is freeze evidence.** The signed statement binds the domain, chain ID, router,
attestor-set hash, block number, block hash and state root, and nothing else
(`packages/l2-client/src/verification.rs:19-41`): not `provisional`, not a finality tier. When a
provisional root is lost and the attestor then signs a different block identity for the same
height, both certificates are valid, the client cannot tell the replacement from equivocation, and
anyone who holds both can submit them as misbehaviour. The client freezes at that height
(`packages/l2-client/src/runtime.rs:290-295`) and stays frozen. A deployment whose attestors sign
roots that can still be lost accepts this. Whether a third party can obtain both certificates
depends on who can reach the attestors' gRPC API, which these artifacts do not decide. A
certificate is `threshold` signatures and each daemon supplies one, so the question is who can
reach at least `threshold` of the client's attestors. That API is plaintext and unauthenticated,
`VerifyStateRoot` signs for any caller that names a height at or below the configured head
together with that block's state root, and the listen address is operator configuration
(`server.address` for OP, `grpc_listen_address` for Arbitrum) with no loopback restriction in the
code. At `unsafe` and `safe`, a caller that reaches `threshold` of the client's attestors on both
sides of a reorg can assemble both certificates, so bind each daemon to loopback or a private
network and expose it only to the relayers that serve the client. Binding the finality tier into
the statement would change the wire format of the attestor, the contract and the relayer
together, and is not part of the current artifacts.

**The default accepts pending Arbitrum assertions, deliberately.** `include_provisional` defaults to
`true` for every `head_kind` except `finalized` (`relayer/cmd/build_l2_source.go`). A confirmed-only
Arbitrum frontier cannot advance until each assertion's challenge period has elapsed, which is most
of the path's latency. Two properties bound the risk of not waiting:

- The attestor serves a pending assertion only if its L2 block is canonical under the attestor's
  own Nitro safe head. An assertion that does not match the replica is never returned.
- BoLD lets a correct assertion win its challenge, provided at least one honest, funded and online
  party defends it.

Under that assumption, a served assertion can lose its challenge only if the attestor's Nitro
replica is itself wrong. With an early `l1_ingest_head`, an L1 reorg can also withdraw it. Operators
who cannot accept either outcome set `include_provisional: false`, at the cost of the full challenge
period on every Arbitrum update.

Arbitrum has three independent settings that touch this path:

- `l1_ingest_head` (attestor): the L1 snapshot used to discover assertions, `finalized` by default
  or opt-in `safe`/`latest`. Assertions discovered before L1 finality stay provisional until both
  finality checks above pass. Early ingestion changes nothing in the Wasm protocol. OP game
  ingestion always uses finalized L1 evidence.
- `attestation_head` (attestor): the Nitro head used to sign `derived` roots.
- `include_provisional` (relayer): whether provisional roots may advance the relayer's frontier.

`attestation_head` deliberately does not apply to `assertion` roots. The assertion path resolves a
confirmed assertion at Nitro's `finalized` head. A pending assertion, or a confirmed one whose block
the finalized head has not reached yet, is resolved at `safe` and served as provisional
(`resolveProposal` in `attestor/arbitrum/bold/attestor.go`). An assertion commits to an L2 block
only after that block's batch is posted to L1, so the block is already at or below Nitro's safe head
when the assertion can be read. The binding constraint is the assertion's L1 confirmation, not the
L2 head, so a head setting there would change nothing.

## Packet proofs

Membership and non-membership load the exact stored L2 consensus height and verify a bounded
`EvmStorageProof` against its authenticated router storage root and configured commitment mapping
slot. Delay values other than zero fail closed.

## Fixtures and validation

The deterministic Go generator owns the cross-language Ed25519 vectors:

```bash
cd relayer
go run ../scripts/generate-l2-attestation-fixtures.go \
  -client-message-out ../test/fixtures/wasm-contracts/l2-client-message.json \
  -unsigned-client-message-out ../test/fixtures/wasm-contracts/l2-client-message-unsigned.json
```

The combined validator regenerates those vectors to a temporary file, checks the committed digest,
runs Rust and Go tests, builds all four Wasm artifacts and verifies their interfaces:

```bash
scripts/validate-l2-clients.sh
```

Set `VALIDATE_OPTIMIZED=1` to build each artifact twice with the pinned optimizer and run the pinned
wasmvm conformance and gas suites. Generated validation and gas reports are written under
`target/`; release manifests are not tracked while the application remains under development.

Run the complete release gate locally with:

```bash
just validate-wasm-release
```

The local gate requires a complete gas baseline and a fresh four-artifact conformance report. It
does not enable or depend on GitHub Actions while repository workflows remain disabled.
