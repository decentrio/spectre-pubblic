---
description: Review naming/terminology of changed Go (relayer) & Rust code against IBC / Cosmos / Ethereum / OP-Stack / Arbitrum domain vocabulary and this repo's own conventions.
argument-hint: "[git ref to diff against, default: merge-base with main]"
---

You are reviewing **naming and terminology only** — not logic, not tests. Scope: the code changed on the current branch (or the diff against `$1` if given; otherwise `git merge-base main HEAD`).

## Step 1 — gather the diff

Run `git diff $(git merge-base ${1:-main} HEAD)...HEAD --name-only` and read the changed Go/Rust files. Focus on identifiers the author introduced (types, funcs, fields, vars, consts, file names, package names, JSON/serde tags). Ignore generated files (`bindings/`, `*.pb.go`, `packages/go-abigen/`, `contracts/verifiers/Groth16Verifier_N*`) and vendored code.

## Step 2 — check against the domain vocabulary

Judge every introduced identifier on these axes. **The wire contract wins over Go house style**: any type/field that crosses the Go↔Rust JSON boundary MUST mirror the counterpart in `packages/l2-client/src/msg.rs` (`SignedAttestedL2Header`, `ClientMessage`, and every nested proof type) byte-for-byte — flag drift, never "improve" it.

- **IBC (ibc-go) terms** — use the canonical ones: `client` / `counterparty` / `ClientMessage` / `ConsensusState` / `ClientState`; packet lifecycle `SendPacket` / `RecvPacket` / `AckPacket` / `TimeoutPacket`; `MembershipProof` / `NonMembershipProof`; `ProofHeight`; `Height{RevisionNumber, RevisionHeight}`; `commitment` / `receipt` / `acknowledgement`. Flag invented synonyms (e.g. "message" for "packet", "proof block" for "proof height").
- **Cosmos** — `wasm client` (08-wasm), `checksum`, `MsgCreateClient` / `MsgUpdateClient` / `MsgRegisterCounterparty`, bech32 `signer`, `ics26_client_id`. Direction vocabulary matches the repo: `cosmos_to_eth` / `eth_to_cosmos` / `l2_to_cosmos`, dir consts `dirCosmosToEth` etc.
- **Ethereum** — go-ethereum / yellow-paper terms: `state_root`, `storage_root` / `StorageHash`, `BlockHash` (not `Blockhash`), `BlockNumber`, `eth_getProof`, account/storage `proof`, MPT `node`, `beneficiary` (not coinbase) and `ommers_hash` **when mirroring the wire header**, `logs_bloom`, `base_fee_per_gas`. Initialisms as the repo uses them (see below).
- **OP-Stack** — `DisputeGameFactory`, `game` / `game_index` / `game runtime` / `root claim`, `output root` / `output_root_proof`, `L2ToL1MessagePasser` → `message_passer_storage_root` (wire) = op-node `withdrawalStorageRoot`, `beacon_slot`, head vocab `unsafe` / `safe` / `finalized`, `attestor` / `AttestedRoot` / `AttestedUpTo`.
- **Arbitrum (BoLD)** — `RollupCore`, `assertion` / `AssertionClaim` / `assertion_hash`, `GlobalState`, `MachineStatus` (`running`/`finished`/`errored`), `inbox_acc`, `end_history_root`, Nitro.

## Step 3 — check house conventions (this repo)

- **Initialisms** follow the codebase, which is NOT strict-Go: it uses `Rpc` + `Url` (`EthRpcUrl`, `TmRpcUrl`, `BeaconUrl`), `ID` all-caps (`ClientID`), `IBC`, and `OP`/`L1`/`L2` caps (`OPStack`). Flag mixed casing of the SAME token across new code (e.g. `Evm` in one type but `EVM` in another; `OpNode` vs `OPStack`).
- **File names**: `chain/<adapter>/` uses single-word lowercase (`source.go`, `builder.go`); `cmd/` uses snake_case (`build_source.go`, `run_adapters.go`). Package names: short, lowercase, no underscores.
- **External SDK/generated fields are exempt** — `clienttypes.MsgUpdateClient.ClientId` (cosmos-sdk) and abigen fields (`ContractICS26Router...ClientId`) keep their generated casing; do not flag those.

## Step 4 — report

Output a table: `identifier | file:line | verdict (OK / drift / inconsistent) | domain-correct name | why`. Then a short verdict: is the change terminology-consistent, and list only the concrete renames worth doing (most-impactful first). Do **not** touch code — this is a review. Cite `file:line` for every finding; if a term is ambiguous, say so rather than guessing.
