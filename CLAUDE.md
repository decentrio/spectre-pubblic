# CLAUDE.md

Operating manual for working in this repository. When this file conflicts with your defaults, this file wins.

## What this project is

Production Solidity implementation of **IBC v2** ("Spectre") for Ethereum ↔ Cosmos interoperability. Three language layers:

- **Solidity** (`contracts/`) — IBC core, token transfer, ZK light client. The product.
- **Go** (`relayer/`) — the relayer. On this branch (`e2e/module-spectre`, AtomOne only, never merged to `main`) the gnark Groth16 prover is **not** in this repository: it is the sibling repository spectre-prover (`../prover`, module `github.com/decentrio/spectre-prover`), and the relayer calls its `proverd` over gRPC. `main` still carries the prover in `relayer/prover/`; statements below about gnark, ecip-gnark, `libgaraga_rs`, circuits and artifacts describe that other repository on this branch.
- **Rust** — CosmWasm Ethereum and L2 ICS-08 clients. The L2 clients live in
  `programs/cw-ics08-wasm-{op,base,arbitrum}` with matching verifier packages.

Key difference from upstream `solidity-ibc-eureka`: a **heavily-optimized purpose-built gnark Groth16 circuit** (Go, batched Ed25519) instead of SP1 (Rust RISC-V zkVM) for Tendermint light-client verification. When describing the project, credit the circuit + gnark optimization work as the moat — not just "Groth16 instead of zkVM".

## Commands

```bash
# Build
just build-contracts              # Compile Solidity
just build-go-relayer             # Build Go relayer (go build ./...)

# Test
just test-foundry                 # All Solidity tests
forge test --match-test <name> -vvv          # Single Solidity test
just test-go-relayer              # All Go relayer tests
cd relayer && go test -race -count=1 -run <TestName> ./...  # Single Go test (always -race)
just test-e2e <name>              # E2E test by name (needs Docker + Kurtosis)

# Lint / static analysis
just lint                         # All linters (solidity + go + buf)
just lint-solidity                # forge fmt + solhint + natlint
just slither                      # Slither static analysis

# Generate
just generate-abi                 # Extract ABIs (after Solidity changes)
forge test --match-contract EncodeTest -vvv  # Encoding cross-validation

# Local devnet — ORDER MATTERS (see "Named mistakes" #9); each script self-anchors to repo root
./scripts/local/run_eth_node.sh            # 1. Ethereum testnet via Kurtosis + deploy contracts
./scripts/local/run_cosmos_node.sh         # 2. Local Cosmos node (only after ETH beacon finalizes)
./scripts/local/wasm.sh                    # 3. Submit ETH light client WASM via governance
./scripts/local/run_prover.sh             # 4. (own terminal) proverd, before any relayer command that proves; needs ../prover + its libgaraga_rs
```

**Prerequisites**: `bun` (never npm/yarn), `just`, Foundry, Go (the version in `relayer/go.mod`). E2E also needs Docker + Kurtosis.

## Relayer CLI

Build the binary for operational commands — don't `go run` them:

```bash
cd relayer && go build -o relayer ./cmd

# One-time setup: create the light clients, ONE COMMAND PER CHAIN (the umbrella
# `create-clients` was removed — #255). Cosmos side first (creates the 08-wasm ETH
# client → learns its real client id), then the ETH side (deploys SpectreClient wired to that
# id). Both write their ids back into config.json. create-clients-cosmos also creates
# one L2 wasm client per --l2-config (all Cosmos-side, L1 client id auto-injected).
./relayer create-clients-cosmos --config config.json --wasm-checksum <hex>
./relayer create-clients-eth    --config config.json --trust-level 2/3

# Start relay loop (bi-directional); add --benchmark for per-inner-call gas + timing logs
./relayer start --config config.json
```

Config: JSON file with `modules` array containing `cosmos_to_eth` and `eth_to_cosmos` entries (see `relayer/config.example.json`). Secrets: `relayer/.env` ships with devnet-only sample keys (`ETH_PRIVATE_KEY`, `COSMOS_PRIVATE_KEY`, `COSMOS_CHAIN_ID`, `COSMOS_ADDRESS_PREFIX` — bech32 account prefix, default `cosmos`, `PROVER_ADDR` — gRPC address of the prover, default `127.0.0.1:50060`) — replace before any real deployment.

**One process serves exactly one source → destination pair.** `loadConfig` rejects a config declaring more than one relay path at parse time (`rejectExtraRelayPaths`, `cmd/config.go`), listing every path it found; `appConfig` holds one value per direction, never a list. A Cosmos→L2 destination and its L2→Cosmos return leg count as one path; Cosmos↔ETH is one path driven by a single `cosmos_to_eth` module. Multiple sources therefore means one config file and one process per source — and each process needs **its own `COSMOS_PRIVATE_KEY`**, because `cosmosMu` serializes only within a process. `ETH_PRIVATE_KEY` is narrower: it must differ only between processes writing the **same** EVM chain, since the nonce cache is keyed `(chainID, address)`. For multiple Cosmos sources that condition always holds — they all relay to the same Ethereum router, so two processes sharing an ETH key sit on one nonce domain and will hand out the same nonce. A `cosmos↔eth` process and a `cosmos↔l2` process write different chains and may share one key; `docs/E2E.md` states the rule and why a shared ETH key degrades more gracefully than a shared Cosmos one. A second ETH key is unusable until it is funded **and** granted the router's `RELAYER_ROLE` — `E2ETestDeploy.s.sol` grants it to the deployer alone, and the resulting revert carries no revert string; `docs/E2E.md` has the `grantRole` command. **`start` enforces both rules** — one advisory lock per `{chain id, address}` EVM nonce domain (`cmd/evm_signer_lock.go`) and one on the Cosmos signing address (`cmd/cosmos_signer_lock.go`), both under `/tmp/spectre-relayer-<uid>/`, so a second process on a key one of those domains already holds refuses to boot and names the lock file. `flock` is per machine and per user, so two hosts sharing a key still collide and nothing local sees it. Run `create-clients-cosmos` then `create-clients-eth` once per source, each against that source's own config file (there is no `--source` flag: a file names one source). `config.example.json` is a catalogue of every module and is deliberately NOT runnable — copy `config.{ethereum,op,base,arbitrum}.example.json` instead.

Circuit setup: `scripts/solidity/build-prover-artifacts.sh` runs spectre-prover's `cmd/setup-circuits` (`PROVER_REPO`, default `../prover`), generates the supported N4 pair in ignored staging, smoke-tests it, publishes the keys into the prover's `bin/` and the verifier into `contracts/verifiers/`, and records provenance. After regeneration the verifying key changes: **redeploy the generated N4 verifier and re-register it via `SignatureVerifier.setBucket(...)`**, or every proof fails on-chain.

## Architecture — orient here before editing

```
ICS26Router (UUPS) ← main IBC entry point
  ├─ ICS20Transfer (UUPS) ← token bridge
  │   ├─ IBCERC20 (Beacon) ← bridged token wrapper
  │   └─ Escrow (Beacon) ← token custody
  └─ SpectreClient (immutable, NOT proxied — replaced via router migrateClient) ← ZK light client; owns ALL client state (ERC-7201 Store)
      │    • updateApplicationState = advance appHash vs pinned valset (per-packet, no re-store)
      │    • updateConsensusState   = rotate pinned valset (24h routine; checks nextValidatorsHash)
      │    • quorum accounting (Σ voting power > ⅔ vs pinned set) + ALL Store writes live HERE, not in the modules
      ├─ light-clients/modules/ (UpdateClient, Misbehaviour = delegatecall; Membership = staticcall) ← stateless verifiers; read the Store, never write
      └─ SignatureVerifier ← rebuilds witness digest, dispatches by bucket (owner-set registry)
          └─ Groth16Verifier_N{N}.sol ← GENERATED, one per N ∈ {4,8,16,32,64,128}, vk baked as constants
```

ZK flow: top-N validator Ed25519 sigs (≥⅔ voting power) → padded to nearest bucket with deterministic dummy keypairs → circuit hashes the witness into a single SHA-256 public input + ECIP batch verify → per-bucket verifier checks the proof. **Pubkeys (A) are committed into the witness hash** so the on-chain pinned-set lookup is honest; **R/S are deliberately NOT in calldata or the hash** (bound by the Ed25519 verify itself). The witness layout must match byte-for-byte between spectre-prover's `witness/witness.go` and `contracts/light-clients/spectre/SignatureVerifier.sol`; `test/fixtures/witness_hash_vectors.json` (generated by that repository's `witness` tests with `SPECTRE_DIR`, consumed by `SignatureVerifierHashEquivalenceTest`) is the bridge.

```
relayer/
├── bindings/       # GENERATED Go bindings — never hand-edit
├── chain/          # multi-chain adapter contract (Source/Destination/Builder) + cosmos/ evm/ l2rollup/ adapters + codec/ (opaque-payload codecs) — see chain/README.md
├── relay/          # generic RelayModule that drives one source→dest path via the chain adapters (the cutover engine)
├── client/         # Tendermint + Ethereum RPC/Beacon API clients
├── prover/         # CLIENT side only: job.go (ProofJob/ProofResult/sentinel errors), witness/types.go (ValidatorSignature), grpcclient/ (services.Prover over gRPC), proverpb/ (vendored prover.proto + generated Go; drift: scripts/check-prover-proto-sync.sh). The prover itself is spectre-prover.
├── services/       # batch builder, PendingPacketTracker, timeout scanners, adapter_api.go surface consumed by the adapters (StartLoop removed at cutover; the adapter engine drives relay)
├── subscriber/     # Cosmos WebSocket + ETH event listeners (both sides have gap recovery — keep it that way)
├── transaction/    # ETH tx (nonce under h.mu) + Cosmos tx (account sequence under cosmosMu)
├── utils/          # IBC path helpers, byte utils
└── cmd/main.go     # CLI: start (runAdapterEngine per source), status (+ `status reactivate`), create-clients-{cosmos,eth}, update-client, update-eth-client, submit-misbehaviour, genesis
```

`packages/go-abigen/` — GENERATED bindings consumed by the relayer (`spectreclient`, `ics26router`, `ics20transfer`, `ibcerc20`, `relayerhelper`).

## Documentation map

`docs/` is deliberately small: six operational runbooks, nothing else. Architecture, design conventions, security doctrine, test policy, observability guides, and benchmark results are **not** documented in this repo — the code is their only description, and performance numbers exist only in runs you make yourself. Earlier versions of those documents are recoverable from git history (`git log --diff-filter=D -- docs/`) but are stale by definition; do not resurrect a claim from one without re-verifying it against the code. Read the runbook whose domain the task touches.

| Document | When to read |
|----------|--------------|
| [docs/ATTESTOR_RUNBOOK.md](docs/ATTESTOR_RUNBOOK.md) | Operating and recovering the attestor services |
| [docs/E2E.md](docs/E2E.md) | Bringing up a devnet or running an end-to-end relay |
| [docs/ERC20_LOCKBOX_ADAPTER.md](docs/ERC20_LOCKBOX_ADAPTER.md) | Deploying or operating an ERC20 lockbox adapter |
| [docs/L2_CLIENTS.md](docs/L2_CLIENTS.md) | OP, Base, or Arbitrum ICS-08 client work |
| [docs/PRODUCTION_DEPLOYMENT.md](docs/PRODUCTION_DEPLOYMENT.md) | Governance-gated deployment, escrow launch gate, governed client migration |
| [docs/MISBEHAVIOUR_RUNBOOK.md](docs/MISBEHAVIOUR_RUNBOOK.md) | Cosmos equivocation incident response |

Docs can lag the code (they have before — "cache"/"planned" wording for features that shipped). **Code is the source of truth; when a doc contradicts the code, believe the code and flag the doc.**

## Hard limits — never cross without explicit instruction

1. **Never `git commit` or `git push` on your own.** Finish the edit, verify it, report, stop. Wait for the user to say so — every time; prior approval does not carry over.
2. **Never `git add .` / `git add docs/` or any blanket staging.** The working tree intentionally holds untracked private files (`*_VI.md`, scratch dirs like `diagrams-tmp/`, local config edits). Stage explicit paths only. Before staging anything you didn't create this session, check `git status` and ask.
3. **Never invent, pad, or extrapolate benchmark data.** Benchmark artifacts are customer-facing. The repo stores no measured numbers, so every figure must come from a run you actually executed this session, quoted with its honest sample count and the hardware it ran on. Never mix Forge isolated-gas numbers with E2E gas numbers in one comparison.
4. **Never hand-edit generated code**: `relayer/bindings/`, `packages/go-abigen/`, `contracts/verifiers/Groth16Verifier_N*.sol`, anything under `abi/`. Fix the source, regenerate.
5. **All GitHub artifacts in English** — issues, PR titles/bodies, commit messages, code comments — regardless of the conversation language.

## Conventions

### In force in this repo

- **Commits**: conventional format `<type>: <description>` (feat, fix, refactor, docs, test, chore, perf, ci), English. The history contains drift ("updates", "nits") — that is debt, not license; PRs should squash-merge to a conventional message.
- **Encoding is a contract**: `contracts/light-clients/spectre/libraries/Encode.sol` must produce byte-identical output to Go `proto.Marshal()`. Both sides change in the same PR, cross-validated by `EncodeTest.t.sol`.
- **Solidity**: 120-col lines, 4-space width, double quotes (`foundry.toml` enforces). UUPS proxies for core contracts, Beacon for per-instance contracts. Modules under `contracts/light-clients/spectre/modules/` stay `pure`/stateless (read the Store, never write) — all client state lives in `SpectreClient`.
- **Go**: wrap errors with `%w` + context; classify relayer failures transient vs permanent (`ErrPermanentRelayFailure`); every shared mutation under its owning mutex; tests run with `-race`.
- **Bindings**: after any Solidity ABI change, regenerate with `abigen` into `packages/go-abigen/` (and `relayer/bindings/` where applicable) in the same PR.
- **Tooling**: `bun` for JS deps; `just` recipes over raw commands when a recipe exists. `relayer/go.mod` no longer requires gnark, `decentrio/ecip-gnark` or any `decentrio/*` module, and needs neither `GOPRIVATE` nor `libgaraga_rs`; those belong to spectre-prover (its `go.mod` pins `decentrio/ecip-gnark` and the `decentrio/gnark` replace; `scripts/build-garaga-ffi.sh` there builds `libgaraga_rs`). `consensys/gnark-crypto` remains here only as an indirect dependency (go-ethereum, go-eth-kzg, `attestor/types`). Never commit machine-local `replace` paths.

### Added — rules this codebase needs that you won't infer

- **Path symmetry**: the relayer is a mirror — Cosmos→ETH and ETH→Cosmos have paired handlers, subscribers, trackers, and timeout scanners. When you change one direction, open the mirror and diff behavior. Most reliability bugs found here were asymmetries (gap recovery on one side only; purge semantics differing between the two timeout scanners).
- **Reliability is a first-class review dimension**: audits catch exploits, not missing functionality. For any relayer change ask: what happens on crash/restart? on RPC failure? does an error path advance a cursor/timestamp it shouldn't? Trace every "no X recovery" to concrete user impact (stuck funds, expired client).
- **Measured claims only**: statements about gas, contract size, or performance come from a command you ran this session (`forge build --sizes`, benchmark logs). Nothing in the repo holds prior measurements, so there is no number to look up — if it doesn't exist, measure it. A temporary probe contract is acceptable if deleted before handoff.
- **Settled design decisions — do not relitigate**: (a) the witness commit hashes the full opaque byte layout; a byte-level 4-segment split circuit was tried and rejected (R1CS concat too expensive) — don't re-propose below N=64 scale. (b) Quorum/pinned-set logic lives in `SpectreClient`, not in the modules — it's shared with Misbehaviour and needs storage; the modules' purity (read-only Store access) is a safety property. (c) Per-bucket verifier contracts are intentional (vk as constants → zero-SLOAD verify, constant size regardless of validator count).
- **`roleManager == address(0)` means permissionless**: the constructor grants roles to `address(0)` as an escape hatch and the role modifier passes for everyone. It is documented design — don't "fix" it as a bug, don't break it silently.

## Named mistakes a weaker model makes here — and the rule that prevents each

1. **Encoding drift** — edits `Encode.sol` or a Go proto type alone. → *Encoding changes touch both languages in one PR and pass `forge test --match-contract EncodeTest`.*
2. **Trusting stale docs** — repeats "planned"/"cache" wording for shipped features. → *Before repeating any capability claim, find the code and cite `file:line`; if you can't find it, write "unverified".*
3. **Guessing at regressions** — sees a missing symbol, declares "broken all along", or recreates the file. → *Run `git log -S '<symbol>'` first; identify the culprit commit before proposing any fix.*
4. **Auto-committing** — "helpfully" commits after an edit. → *Hard limit 1: edit, verify, report, stop.*
5. **Blanket staging** — `git add .` sweeps in private untracked files. → *Hard limit 2: explicit paths only.*
6. **Fabricated numbers** — pads benchmark samples, or quotes gas from memory or from a deleted benchmark file. → *Hard limit 3: every number has a command you ran this session behind it.*
7. **Regenerating circuits without redeploying** — runs the prover's `cmd/setup-circuits`, forgets the vk changed. → *Circuit regen ⇒ redeploy `Groth16Verifier_N{N}` + `setBucket`, stated in the same breath.*
8. **Editing generated files** — patches `bindings/` or a generated verifier directly. → *Hard limit 4: fix the generator/source, regenerate.*
9. **Wrong devnet order** — starts Cosmos before Ethereum finalizes; Tendermint LC creation then fails confusingly. → *eth node → poll beacon `finality_checkpoints` until `finalized.epoch > 0` → cosmos node → `wasm.sh`.*
10. **One-sided relayer fixes** — patches the Cosmos path, leaves the ETH mirror with the old bug. → *Every relayer change ends with a mirror-path diff; name the paired file in your report.*
11. **Advancing state on failure** — bumps a routine freshness timestamp or a recovery cursor when the operation failed. These guard client expiry and event-loss windows. → *Never advance a timestamp/cursor/tracker on a failed operation.*
12. **Sequence/nonce races** — adds a submission path that skips the handler mutexes. → *All ETH sends go through the nonce block under `h.mu`; all Cosmos sends through `SendCosmosTxBatch` under `cosmosMu`.*
13. **npm/yarn, or `go run` for ops** — → *`bun` for JS; built `./relayer` binary for `create-clients-{cosmos,eth}`/`start`.*
14. **Downgrading pinned deps to match a stale mirror** — a mirror lags, model "fixes" by downgrading. → *Never regress a pinned version to satisfy a mirror; fix the install source.*
15. **Chat language leaking into GitHub** — non-English chat bleeds into an issue or commit. → *Hard limit 5.*
16. **Misreading the permissionless escape hatch** — flags `hasRole(ROLE, address(0))` as a vulnerability. → *It's the documented permissionless mode; report it as design context, not a finding.*
17. **Weakening witness binding** — moves a field out of the witness hash "to save gas", or adds R/S to calldata "for clarity". → *The hashed-field set in spectre-prover's `witness/witness.go` is a security boundary; any change needs an explicit binding argument for both the before and after states.*

## Quality bar per deliverable — checkable, not adjectives

**Solidity change**
- [ ] `just build-contracts` clean; `just lint-solidity` clean
- [ ] Targeted tests pass (`forge test --match-test/-contract ... -vvv`); full `just test-foundry` before handoff
- [ ] If encoding touched: `EncodeTest` passes and the Go counterpart changed in the same diff
- [ ] If ABI changed: bindings regenerated in the same diff
- [ ] If light-client/verifier touched: `forge build --sizes` run and the new `SpectreClient` EIP-170 margin stated
- [ ] `just slither` run for anything touching access control, upgrades, or value transfer

**Go relayer change**
- [ ] `go build ./...` and `go vet ./...` clean from `relayer/`
- [ ] `go test -race -count=1 ./...` passes for affected packages
- [ ] New pure logic (cursors, backoff, dedupe, classification) has a unit test
- [ ] Mirror path checked — name the file you diffed and what you found (even if "already symmetric")
- [ ] No error path advances cursor/timestamp/tracker state; failures logged with context, never swallowed
- [ ] Any new goroutine/channel: state who owns each mutable structure and on which goroutine it is touched

**Prover/circuit change**
- [ ] Witness layout change mirrored in `SignatureVerifier.sol` byte-for-byte, stated as a table (offset, width, field)
- [ ] Explicit note whether R1CS changed — if yes, regeneration + redeploy + `setBucket` steps listed
- [ ] Bench harness still compiles; synthetic signatures still satisfy prefix-binding

**Docs change (`docs/` or the Obsidian vault)**
- [ ] Every technical claim verified against code, with `file:line` cited in your report
- [ ] No dead wikilinks/anchors introduced (grep the targets); external links fetched, confirmed non-404
- [ ] Performance claims qualitative unless quoted from a run made this session
- [ ] Terminology matches the code (SpectreClient, SignatureVerifier + bucket verifiers, updateConsensusState, updateApplicationState). Former (pre-refactor) names for translating git history / old branches: Groth16ICS07Tendermint, WrapperVerifier, reAnchorPinnedSet, updateClient respectively

**PR / code review**
- [ ] Reviewed the real merge-base diff (`gh pr diff` or `git diff $(git merge-base main <branch>)...<branch>`), not the PR description
- [ ] Every claimed fix traced to the diff hunk implementing it; every linked-issue item marked done/partial/missing
- [ ] Branch built and race-tested locally; results quoted verbatim
- [ ] Verdict separates **blockers** from **nits**; meta checked (commit message format, `close` vs `refs` when the issue is only partially fixed, whether CI actually ran)

**GitHub issue**
- [ ] English; one checkbox per independently assignable item
- [ ] Each item: `file:line` pinned to a named ref/commit, concrete user impact, suggested fix
- [ ] Priority order + a "reviewed and healthy" section so assignees don't re-audit clean code

**E2E / devnet run**
- [ ] Bring-up followed the canonical order; every wait condition polled, never slept blindly
- [ ] Failures reported with the actual log excerpt, not a paraphrase

## When uncertain — exact escalation rules

**Proceed without asking** (reversible, in scope):
- Reading anything; running builds, tests, linters, static analysis
- Editing Solidity/Go/Rust/docs within the task's stated scope
- Scratch files in the session scratchpad; a temporary probe contract you delete before handoff

**Stop and ask first**:
- `git commit`, `git push`, and opening/closing/commenting on GitHub issues or PRs — unless this task explicitly asked for that artifact
- Deleting or overwriting any file you didn't create this session
- Regenerating trusted-setup/circuit artifacts (expensive; invalidates deployed verifiers)
- Any action that spends funds, touches a live network, or publishes externally
- Expanding scope beyond the request — report the finding, propose, wait

**Conflict-resolution order** when sources disagree:
1. The user's explicit instruction in this conversation
2. This file
3. The code as it exists (read it; use `git log -S` for its history)
4. `docs/` and saved notes — may be stale; flag staleness when detected

**If a claim can't be verified** (endpoint down, code ambiguous, no measurement): write "unverified" plus what would verify it. Never round uncertainty up to a fact.

**If tests fail**: paste the failing output, state whether your change caused it (verify against a clean checkout if unsure), and never adjust tests to match broken code.

**If you find a security-relevant bug** while doing something else: stop, report it with `file:line` and impact, let the user choose sequencing. Never fold a security fix silently into an unrelated diff.

## Working with the user

- **Chat in the language the user writes in; all repo/GitHub artifacts in English.**
- When asked to review, deliver the assessment and stop — don't apply fixes until asked. When asked to fix, fix exactly what was scoped.
- He delegates via GitHub issues — write findings so a teammate can pick them up without re-deriving context.
- Explanations: conclusion first, then evidence. If he says it's hard to follow, simplify with a concrete analogy — don't repeat the same abstraction louder.
- Obsidian vault (`/Users/ducnt/Documents/tmp`): duplicate a note to `<name>-duc.md` before editing; never touch the original unless told per-file.

## Agent skills

### Issue tracker

GitHub Issues on Spectre's `origin` remote, via the `gh` CLI. Labels are not used. See `docs/agents/issue-tracker.md`.

### Domain docs

Single-context: one `GLOSSARY.md` + `docs/adr/` at the repo root. `docs/agents/` and `docs/adr/` are exempt from the "six runbooks only" rule above. See `docs/agents/domain.md`.
