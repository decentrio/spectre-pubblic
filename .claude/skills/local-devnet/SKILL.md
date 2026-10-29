---
name: local-devnet
description: Bring up the local ETH↔Cosmos or Cosmos↔OP devnet and relayer in the proven order (L1 → beacon finality → [OP L2 + attestor] → cosmos → wasm gov → build relayer → circuits → create-clients → start), with wait conditions, failure modes, and recovery. Use when running E2E manually, testing the relayer against live local chains, or debugging devnet bring-up.
---

# Spectre local devnet runbook

The order below is load-bearing: the Tendermint light-client on ETH can only be created after Ethereum has **finalized** at least one epoch, and the ETH light-client on Cosmos needs the WASM code stored via governance. Doing these out of order produces confusing downstream failures, not clear errors at the wrong step.

Build setup (`bun install`, `just build-contracts`) is assumed done — it is separate from this runbook.

For the **Cosmos↔OP** path, jump to "Cosmos↔OP variant" below after reading steps 2–6 — it reuses the same Cosmos/wasm/create-clients machinery with an OP L2 and an attestor layered on.

## 1. Ethereum node (Kurtosis), then contracts

```bash
./scripts/local/run_eth_node.sh          # L1 only; writes .eth-devnet-run/eth.env
./scripts/local/deploy_eth_contracts.sh  # sources that handoff, deploys, patches config
```

The node and the deploy are separate scripts (`run_eth_node.sh` is parametrized by `ENCLAVE` / `ETH_PIN` / `L1_PARAMS` / `RUN_DIR` / `SKIP_COSMOS_RESET` so the OP flow can reuse it). Endpoints land in `eth.env` — the EL RPC, WS, and the **beacon (CL) REST** URL you need for the next step. `eth-network-params.yaml` runs Fusaka from genesis, so the ETH light client must have Fulu fork support.

## 2. Wait for beacon finality — poll, never sleep blindly

```bash
watch -n 5 'curl -s http://127.0.0.1:<beacon-port>/eth/v1/beacon/states/head/finality_checkpoints | jq .data.finalized'
```

Proceed only when `finalized.epoch > 0`. On a fresh Kurtosis net this takes a few minutes. Starting the Cosmos side before this wastes the whole bring-up: `create-clients` will fail against a chain with no finalized checkpoint.

## 3. Cosmos node, then WASM light client via governance

```bash
./scripts/local/run_cosmos_node.sh     # local node with test accounts
./scripts/local/wasm.sh                # stores the ETH light-client WASM through a gov proposal
```

Docker variants exist (`run_cosmos_node_docker.sh`, `wasm_docker.sh`) if running the node natively is not possible. `wasm.sh` prints the stored **checksum** — record it; `create-clients` needs it.

Checksum facts (matter when targeting a real testnet, not just local):
- The 08-wasm checksum is the sha256 of the **uncompressed** wasm.
- `MsgStoreCode` is gov-gated; `MsgCreateClient` referencing an existing checksum is permissionless.
- Do NOT reuse checksums already stored on public testnets without verifying the ConsensusState schema: current schema is **5-field** (slot, state_root, timestamp, current_sync_committee, next_sync_committee — no `storage_root`). Stale 6-field builds fail at DeliverTx with `missing field storage_root`. Inspect a stored build: fetch `GET <rest>/ibc/lightclients/wasm/v1/checksums/{c}/code`, base64-decode, `strings | grep 'struct ConsensusState'`.

## 4. Build the relayer binary, circuit artifacts, and start the prover

The prover is its own repository and process (spectre-prover, a sibling checkout at `../prover`); the relayer links no gnark and calls it over gRPC at `PROVER_ADDR` (`relayer/.env`, default `127.0.0.1:50060`). Build `libgaraga_rs` once there (`scripts/build-garaga-ffi.sh`), generate the keys with `just build-prover-artifacts`, and keep `proverd` running in its own terminal for every step that proves (`start`, `update-client`, `submit-misbehaviour`).

```bash
cd relayer
go build -o relayer ./cmd                              # use the binary, not `go run`, for all ops
(cd .. && ./scripts/local/check_prover_pairing.sh)     # subshell: stay in relayer/ for the next steps; MUST pass before any deploy (deploy_*_contracts.sh run it too)
(cd .. && ./scripts/local/run_prover.sh)               # own terminal; foreground. `start` fails fast, naming PROVER_ADDR, if it is not up
```

**`pk.bin` (in the prover checkout's `bin/`, or `PROVER_BIN_DIR`) must be the pair of `contracts/verifiers/Groth16Verifier_N{N}.sol`.** `pk.bin` is git-ignored and unrecoverable; a `pk` from any other setup deploys and sends fine, then every Cosmos→EVM `updateApplicationState`/`updateConsensusState` reverts `ProofVerificationFailed()` and the packet re-queues forever (nothing else in the log says why). `check_prover_pairing.sh` compares r1cs/pk/vk/verifier on disk with `.artifacts/solidity/prover/provenance.json` (r1cs/pk/vk are read from the directory the relayer will really load — `PROVER_BIN_DIR` from the environment, else `relayer/.env`, else `./bin` — so a redirected `PROVER_BIN_DIR` cannot pass on the strength of `relayer/bin`) and exits 1 with a warning banner on any mismatch or missing file. If it fails, do **not** deploy or send packets: regenerate the whole set with `scripts/solidity/build-prover-artifacts.sh --force` (overwrites `relayer/bin/n4` and the git-ignored verifier — ask the user first, CLAUDE.md), then redeploy and recreate the clients. Do not use `go run ./prover/cmd <dir>` outside that script: it writes no provenance, and a set generated into a scratch dir has a different vk than any verifier already deployed.

Circuit regeneration changes the vk → the generated `Groth16Verifier_N{N}.sol` must be **redeployed** (a fresh router via `deploy_*_contracts.sh`, then recreate both clients — the Cosmos-side client profile pins `l2_router`) or re-registered via `SignatureVerifier.setBucket(...)`. If contracts were just deployed by `run_eth_node.sh` from freshly generated verifiers, they already match — regeneration mid-session is what breaks the pairing.

Bucket sizing: the prover picks the smallest N ∈ {4,8,16,32,64,128} that covers a greedy-by-power ≥⅔ quorum. Local devnets and even the Cosmos Hub `provider` testnet (top-4 validators > ⅔) need only N=4 — don't compile big buckets you won't use.

## 5. Create clients

```bash
./relayer create-clients-cosmos --config config.json --wasm-checksum <hex-from-step-3>
./relayer create-clients-eth    --config config.json --trust-level 2/3
```

One command per chain — the umbrella `create-clients` was removed in #255. Run the Cosmos half first (08-wasm client → learns its client id), then the ETH half (SpectreClient wired to that id). They write `cosmos_wasm_client_id` + `spectre_client` back into `config.json` automatically — no manual copying. Config is JSON (`config.example.json` shape: `modules` array with `cosmos_to_eth` + `eth_to_cosmos`); `.env` holds only secrets/prover paths (`ETH_PRIVATE_KEY`, `COSMOS_PRIVATE_KEY`, `COSMOS_CHAIN_ID`, `COSMOS_ADDRESS_PREFIX` — bech32 account prefix, default `cosmos`; set it for non-`cosmos` chains like Realio, `PROVER_BIN_DIR` — the shipped values are devnet-only).

## 6. Start the relay loop

Before `start` and before sending the first packet, check the verifier **registered on the destination chain** is the pk's pair (needs a current `forge build`; `--signature-verifier` defaults to `.l2-deployment.json`, pass it for an L1 deploy). A mismatch prints a warning banner and exits 1 — stop and fix it rather than sending packets:

```bash
(cd .. && forge build && ./scripts/local/check_prover_pairing.sh --rpc <evm-rpc> [--signature-verifier <addr>])   # subshell: `./relayer start` below must run from relayer/
```

```bash
./relayer start --config config.json              # add --benchmark for per-inner-call gas + timing logs
```

Sanity signals in the log: both subscribers connected (`Successfully subscribed to ICS26Router events`), startup recovery scans complete, no repeating error line every second.

## 7. Verify with a transfer

Send a token transfer in one direction, watch it relay, then send the reverse direction. **Always test both directions** — the relayer is a mirror and one-sided success proves half the system. A packet from ETH relays only after its block is beacon-finalized (minutes on devnet) — "not relayed yet" right after send is normal, not a bug.

## Cosmos↔OP variant

Same skeleton, with an OP L2 + attestor layered on the L1. `run_optimism_node.sh` calls `run_eth_node.sh` internally (it must pass the exact ethereum-package ref optimism-package pins, which is only known after cloning it), so do **not** run the L1 separately.

```bash
./scripts/local/run_optimism_node.sh   # L1 (Fulu) + OP L2 in one enclave -> .op-devnet-run/attestor.env
./scripts/local/run_op_attestor.sh     # attestor over the replica op-node; gRPC :3001
./scripts/local/run_cosmos_node.sh
./scripts/local/wasm_op.sh             # OP client — the ETH one is NOT needed here
DST_CHAIN=opstack L2_DEPLOYER_ADDRESS=<l2-funded> L2_DEPLOYER_PRIVATE_KEY=<key> \
  ./scripts/local/deploy_l2_contracts.sh
cd relayer
./relayer create-clients-cosmos --config config.json --l2-config <op.json>
./relayer create-clients-eth --config config.json --trust-level 2/3
./relayer start --config config.json
```

Load-bearing details, each one cost a debugging session:

- **Wasm must come from the optimizer**, never a plain `cargo build` — a raw release build carries `reference-types` and `MsgStoreCode` rejects it. If the optimizer image's Rust is older than a dependency's MSRV, pin the dependency down rather than shipping a raw build.
- **Host allowlist**: the 08-wasm Stargate allowlist needs `ClientStatus` on top of `ClientState`/`ConsensusState`, or L2 client creation fails with `status Unknown` — see `docs/L2_CLIENTS.md`.
- **`deploy_l2_contracts.sh` patches `relayer/config.json`** (override with `RELAYER_CONFIG`), so there is nothing to copy across. It also patches the matching `l2_to_cosmos` module's `rollup_profile.common.l2_router` — leaving that at the example placeholder used to fail much later as a membership-proof error against an account that does not exist.
- **Deploy the L2 contracts with the key the relayer will run with** (`relayer/.env` `ETH_PRIVATE_KEY`). `E2ETestDeployL2` grants the ICS26Router relayer role to `msg.sender`, so a different deployer leaves the relayer unauthorized and every `updateApplicationState` reverts. That account also needs an L2 balance — the L1 devnet faucet address has none there — but **funding alone does not fix the role**, and the revert looks identical either way (see the `canCall` entry below).
- **Send packets on the Cosmos client that tracks the L2**, register its counterparty to the L2 router's client id, and make sure the module's `cosmos_wasm_client_id` is that same L2 client before `create-clients-eth` (it decides the SpectreClient's counterparty; a mismatch reverts `recvPacket`).
- Gov proposals: `wasm.sh` resolves the proposal id after a fixed sleep — if indexing lags, the vote step silently no-ops and the proposal is REJECTED. Vote manually within the (short) devnet voting period.
- **Point `eth_beacon_api_url` at this run's beacon.** The `eth-to-cosmos` module keeps the example's port; `create-clients-cosmos` dies with `beacon api unavailable` before it does anything.
- **The return direction needs `l2_ics26_client_id` set to the L2 router's real client id** — the subscriber filters events on it, so a placeholder drops every ack silently. It must equal the `cosmos_to_l2` module's `ics26_client_id`; both name the same client.
- **Two things this list used to demand are now wrong**, and doing them costs you: `disable_derived_roots: true` and a pinned, advancing Ethereum client. Both belonged to the settlement-proof client, which verified a dispute game against L1. Since #345/#347 the attestor-trusted client verifies nothing there, so the builder no longer needs a game-backed root and there is no L1 client in the L2 path at all (`cmd/build_l2_source.go` — the #276 dependency is retired). Both attestor scripts now default `DISABLE_DERIVED_ROOTS=false` on purpose: a game lands long after the L2 block it commits, so games-only leaves the frontier hours behind for no gain.
- **`DERIVED_GAP_BLOCKS` is the floor on return-direction latency.** All three devnet handoffs export `5`; an attestor pointed at an external node inherits nothing and takes 150, which is ~5 minutes on a 2 s chain.

- **The relayer's ETH signer must hold the ICS26Router role on the L2**, and the sample `relayer/.env` key does not. Every `updateApplicationState` then reverts with a bare custom error (`0x068ca9d8` + the caller address) — no revert string, nothing in the relayer log beyond `execution reverted`. `cast run <tx>` shows the cause in one line: `canCall(<signer>, <ICS26Router>, 0x9c11bece) → false`. Funding the address does not help; it is authorization, not gas. Export the key `E2ETestDeployL2` granted the role to, or grant the role to the `.env` address.
- **Send with `--absolute-timeouts` and an absolute second-precision timestamp.** Without that flag the CLI builds `timeout_timestamp` from `time.Now().UnixNano()`, and the IBC v2 send path — which reads the field as **seconds** — rejects it as `timeout exceeds the maximum expected value: invalid packet timeout`. This happens with the flag's own default too, so a plain `--packet-timeout-timestamp 600` fails just as hard. The runbook in docs/E2E.md already has the correct form:
  ```bash
  ABS_TIMEOUT=$(($(date +%s) + 2000))
  gaiad tx ibc-transfer transfer transfer <cosmos-l2-client-id> <evm-receiver> 1000stake \
    ... --absolute-timeouts --packet-timeout-timestamp "$ABS_TIMEOUT" --generate-only \
    | jq '.body.messages[0].encoding = "application/x-solidity-abi"' | ...
  ```
  `broadcast` returns `code: 0` for a tx that later fails in DeliverTx — always re-query with `gaiad q tx <hash>`.
- **Re-creating the Cosmos-side L2 client while keeping the L2 router's client id replays sequence numbers.** `create-clients-eth` repoints the existing router client (`migrateClient`) instead of adding a new one, so the source restarts at sequence 1 while the destination's receipt store still holds receipts for 1, 2, … The relayer then drops those packets **permanently** with `IBCPacketReceiptMismatch` — correct behaviour (a receipt exists), but it looks like a proof failure. Either send throwaway packets past the reused range, or redeploy the L2 contracts for a clean router.
- **`E2ETestDeployL2` never grants `LIGHT_CLIENT_MIGRATOR_ROLE`**, and that role is per-client (`keccak("LIGHT_CLIENT_MIGRATOR_ROLE_" || clientId)`). So the migrate above reverts for *everyone*, deployer included, with a bare `IBCUnauthorizedMigrator`. Grant it from the AccessManager admin (the L2 deployer) first:
  ```bash
  AM=$(cast call <ICS26Router> "authority()(address)" --rpc-url $L2)
  ROLE=$(cast to-uint256 $(cast keccak "LIGHT_CLIENT_MIGRATOR_ROLE_<clientId>") | head -c 20)  # low 64 bits
  cast send $AM "grantRole(uint64,address,uint32)" <role> <relayer-address> 0 \
    --private-key <deployer-key> --rpc-url $L2
  ```
  Read the role id straight out of the `hasRole(...)` line in `cast run <reverted-tx>` rather than deriving it.
- **`op-proposer` runs out of L1 funds after a few hours** — every dispute game costs a bond. It then logs `insufficient funds for transfer` on a loop and stops proposing, so the attestor's frontier freezes while the L2 keeps producing blocks. The relayer's return path just says `waiting: … source relayable height=N` forever, pointing at nothing. Check `docker logs <op-proposer-…>` whenever the frontier stalls, and top the proposer up:
  ```bash
  P=$(cast wallet address --private-key $(docker inspect <op-proposer-container> \
        --format '{{range .Config.Cmd}}{{println .}}{{end}}' | grep -o '0x[0-9a-f]\{64\}'))
  cast send $P --value 200ether --private-key <l1-funded-key> --rpc-url $L1
  ```
- **L2 startup recovery is bounded unless configured otherwise.** `Source.Subscribe` has no persisted cursor and restarts one startup window below the head (`chain/l2rollup/subscribe.go`). The window is a duration (default 8m32s) converted to blocks at the chain's measured block time; set `L2_STARTUP_LOOKBACK` (a Go duration, e.g. `90m`) high enough to cover the expected attestation wait and downtime before restarting.

- **The Ethereum client must be able to cross a sync-committee period.** Periods roll every 8192 slots. The relayer takes the next period's committee from the preceding period's light-client update, so the beacon must serve `/eth/v1/beacon/light_client/updates` — check before a long run:
  ```bash
  curl -s "$BEACON/eth/v1/beacon/light_client/updates?start_period=0&count=2" | head -c 120
  ```
  If it only serves `finality_update`, the client advances for one period and then stops dead with `404 NOT_FOUND: Sync committee for period N not found`, which cascades into `historical state ... is not available` on every proof.

Verify **both** directions: a forward-only success proves half the system. The forward leg ends with the token minted on the L2; the return leg ends with the ack relayed back and the packet leaving the relayer's pending tracker (`CosmosTimeoutScan] Checking 0 pending packets`).

## Failure modes → cause

| Symptom | Cause / fix |
|---|---|
| `create-clients` fails creating Tendermint LC on ETH | ETH not finalized yet — redo step 2 |
| `MsgStoreCode`: `reference-types not enabled` | Wasm built with plain `cargo build` — rebuild via `cosmwasm/optimizer` |
| `MsgCreateClient`: `status Unknown: client state is not active` | 08-wasm allowlist missing `/ibc.core.client.v1.Query/ClientStatus` |
| `recvPacket` reverts with two client ids in the revert data | SpectreClient counterparty ≠ the source client the packet was sent on |
| Attestor attests nothing and names no cause | Wrong BoLD `_assertions` slot reads an empty mapping. The devnet slot is `0x75`, which `run_arbitrum_node.sh` verifies at bring-up before writing its handoff |
| `updateApplicationState` reverts, `gasUsed` ~82k, no revert string | Signer lacks the ICS26Router role on the L2 — `cast run <tx>` shows `canCall(...) → false` |
| `timeout exceeds the maximum expected value` on send | Sent without `--absolute-timeouts`, so `timeout_timestamp` is nanoseconds; IBC v2 reads it as seconds |
| `packet at height N not yet covered by the destination client (trusts M)` | Normal wait — the client landed on a game committing below the packet; the next game covers it |
| `404 NOT_FOUND: Sync committee for period N not found` | The beacon serves no bootstrap for that period; confirm it serves `light_client/updates` |
| `unknown field account_proof, expected one of key, value, proof` | An L2 membership proof built in the Ethereum L1 shape — see `docs/L2_CLIENTS.md` |
| A relay direction goes silent — no error, no retry, other directions fine | An RPC call is hung. `kill -QUIT <relayer-pid>` dumps every goroutine to the log; look for one blocked in `net/http.(*persistConn).roundTrip`. Note the dump kills the process; set `L2_STARTUP_LOOKBACK` (a duration) large enough before restarting so in-flight L2 events remain in the recovery window (default: 8m32s of blocks). |
| `IBCPacketReceiptMismatch` and the packet is dropped as permanent | The destination already has a receipt at that sequence — the Cosmos client was re-created while the L2 router client id was reused |
| `forge script`: `insufficient funds ... have 0` | Deployer unfunded on the L2 — use an L2-funded account |
| DeliverTx `missing field storage_root` | Stale 6-field 08-wasm build — store a current 5-field build (step 3) |
| Every `updateClient` proof reverts on-chain | Circuit artifacts regenerated after verifiers were deployed — redeploy verifiers + `setBucket` |
| `ProofVerificationFailed()` on every `updateApplicationState`/`updateConsensusState`, packet re-queued forever (the relayer sends the Cosmos packet fine) | `pk.bin` is not the pair of the `Groth16Verifier_N4` registered on that chain. `./scripts/local/check_prover_pairing.sh --rpc <evm-rpc>` names which side differs. Fix = regenerate the set + redeploy + recreate both clients, never a pk from another setup |
| Attestor exits `no contract code at given address` on `DisputeGameFactory.gameCount` right after bring-up | It reads the factory at the L1 **finalized** block, which predates the factory deploy. Poll `cast call <factory> 'gameCount()(uint256)' --block finalized` until it answers, then start the attestor |
| Attestor exits `state source identity ... does not match configured` | `.op-attestor-run` still holds state from a previous devnet's factory. Start with a fresh `ATTESTOR_RUN_DIR` instead of deleting the old one |
| `deploy_l2_contracts.sh`: `insufficient funds ... have 0` | Fund the deployer by `OptimismPortal.depositTransaction(<deployer>, 10ether, 100000, false, 0x)` with `--value 10ether` from an L1-funded key (`OPTIMISM_PORTAL` in `attestor.env`); wait for the L2 balance to appear |
| `account sequence mismatch` on Cosmos | Something bypassed the serialized send path; check for a second process using the same key |
| Relayer idle though events fire | Wrong client-ID filter in config, or subscriber connected to the wrong port — check the mapped Kurtosis ports again |
| Beacon finality wait loops forever in relayer | Kurtosis net stalled — check CL container logs; restart the network rather than the relayer |

## Teardown

Kurtosis enclaves and the Cosmos node keep running until stopped: `kurtosis enclave ls` / `kurtosis enclave rm -f <name>`, and kill the cosmos node process. Stale enclaves are the usual cause of "port already in use" on the next bring-up.
