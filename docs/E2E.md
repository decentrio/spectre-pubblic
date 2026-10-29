# End-to-end runbooks

Every local bring-up: Cosmos↔Ethereum, Cosmos↔OP, Cosmos↔Arbitrum, Cosmos↔Base,
and running against an L2 someone else operates. Split out of the README, which
had grown past the point where any of it was findable.

Prerequisites are in [the README](../README.md#requirements); these runbooks assume
the toolchain is in place and add Docker + Kurtosis on top.

**Read this first, whichever chain you are bringing up.**

Order is load-bearing. The Tendermint light client on the EVM side can only be
created once the L1 beacon has finalized an epoch, and the Ethereum light client on
Cosmos needs its wasm stored by governance first. Out of order, the failures land
several steps later and name the wrong thing.

Two things that are easy to get wrong before you start:

- **The circuit artifacts are not in the repo.** `.gitignore` excludes every
  `contracts/verifiers/Groth16Verifier_N*.sol`, so a fresh clone has none and
  `forge build` fails on `E2ETestDeployL2`'s import. Step 1 of the first runbook
  below is mandatory, not optional — and the `bin/` it writes must stay paired with
  the verifiers it emitted, because each `Setup()` produces a different verifying
  key. Deploying verifiers from one run and proving with another's `pk.bin` makes
  every proof revert on chain.
- **Gaia must be the custom-host build.** The scripts require the checkout to be on
  `test/ibc-host-customs`; see [Gaia binary](#gaia-binary) below.

### One L1, shared between rollups

Every rollup script calls `run_eth_node.sh` to guarantee an L1 exists in its enclave,
and **that script reuses a running enclave rather than rebuilding it**. Three
consequences worth knowing before you start:

- The rollup scripts may be run in **any order**, and re-running one does not disturb
  the L2 services, contracts or chain state of the others sharing that L1.
- `run_eth_node.sh` still re-discovers the endpoints and rewrites `eth.env` on every
  call, so a caller can source it unconditionally.
- `FORCE_RECREATE=1` destroys the enclave and rebuilds the L1 from scratch — and takes
  **every rollup in it** with it. That is the flag for a genuinely clean L1, not for
  routine re-runs.

Sharing is per **enclave**, and the defaults differ: `run_optimism_node.sh` and
`run_arbitrum_node.sh` both use `op-devnet`, so they share out of the box, while the
plain ETH↔Cosmos devnet (`run_eth_node.sh` on its own) uses `my-testnet` and is a
separate L1. Set `ENCLAVE` explicitly to put them together.

### Signing keys when several processes share a chain

`start` serves exactly one relay path per process, so any deployment with more than
one path runs more than one process. Two rules follow, and they are not the same
rule for the two chains.

**Every `start` process needs its own `COSMOS_PRIVATE_KEY`.** This holds for *every*
path, including one that looks like it only writes to an EVM chain: a Cosmos→L2
process still refunds expired Cosmos-origin packets **on Cosmos** through its timeout
scanner. So the running example — one `cosmos↔eth` process plus one `cosmos↔l2`
process against the same Cosmos chain — needs two keys, not one:

| Process | Writes Cosmos from |
| --- | --- |
| `cosmos↔eth` | the eth→cosmos direction, and `ScanCosmosTimeouts` refunds |
| `cosmos↔l2` | the l2→cosmos return leg, and `ScanCosmosTimeouts` refunds |
| `cosmos→l2`, forward only | `ScanCosmosTimeouts` refunds |

There is no `start` configuration that never writes to Cosmos, which is why the rule
has no exceptions to remember.

Sharing one key means sharing one account sequence, and nothing coordinates the two
processes. Each reads the sequence from committed state before signing, so the
collision window is a whole block, not an instant. If both processes talk to the same
RPC node the loser fails at CheckTx, which the relayer treats as transient and
retries — wasteful but safe. If they talk to **different** nodes both pass CheckTx and
the loser fails at DeliverTx instead, which is classified as a deterministic on-chain
failure: a single-packet transaction is then **dropped**, and its funds sit in escrow
until the timeout scanner refunds them. A shared key turns an infrastructure race into
a failed user transfer.

**`ETH_PRIVATE_KEY` only needs to differ between processes writing the same EVM
chain.** Nonces are tracked per `{chain id, address}`, so the `cosmos↔eth` process
(L1) and the `cosmos↔l2` process (L2) can share one key — different chains, independent
nonces. Two `cosmos↔eth` processes relaying two Cosmos sources into the *same* L1 do
need separate keys.

A shared ETH key also degrades more gracefully than a shared Cosmos one: an
unrecognised broadcast error invalidates the cached nonce and returns a transient
error, so the packet is re-queued rather than dropped.

**`start` enforces both rules**, with one advisory lock per contended resource, all
under `/tmp/spectre-relayer-<uid>/`.

The EVM half takes one lock per nonce domain it can write: a file per
`{chain id, address}`, named `evm-signer-<chain id>-<address>.lock`
(`cmd/evm_signer_lock.go:220`). The chain id is resolved from each configured
endpoint at startup rather than read from the config, so the lock names the domain
the process will actually write. A `cosmos<->eth` and a `cosmos<->l2` process write
different chains, take different locks, and may share one key -- the guard permits
exactly what the rule above permits.

The Cosmos half takes one lock on the signing address alone, named
`cosmos-signer-<address>.lock` (`cmd/cosmos_signer_lock.go`). Deliberately nothing
else is in that name -- not the chain id, not the config path, not the state
directory. A lock named after anything the runbook tells operators to vary per
process is not a lock: the two processes it exists to catch each take their own file
and both start. The directory itself reads no environment variable for the same
reason, since `.env` is loaded before the lock is taken.

Either lock refuses to boot and names the address and the lock file, instead of
colliding at the first submission. Both are `flock`, so they reach one user on one
machine; two hosts sharing a key still collide and nothing local can see it.

**A second ETH key is not usable until it is funded AND granted the router's
`RELAYER_ROLE`.** This is the step that costs an afternoon if it is missed, because
the two failures look identical from the relayer: `E2ETestDeploy.s.sol:95-96` builds
a one-element relayer list and grants the role to `msg.sender` alone, so any key
other than the deployer's is unauthorized. `updateApplicationState` then reverts with
a bare custom error — no revert string, and nothing in the relayer log beyond
`execution reverted`. Funding the address does not help; it is authorization, not
gas. `cast run <tx>` names the cause in one line: `canCall(<signer>, <ICS26Router>,
0x9c11bece) -> false`.

Grant it from the AccessManager admin, which is the account that deployed the
contracts. `RELAYER_ROLE` is `1` (`contracts/shared/access/IBCRolesLib.sol:14`):

```bash
AM=$(cast call <ICS26Router> "authority()(address)" --rpc-url $RPC)
cast send $AM "grantRole(uint64,address,uint32)" 1 <second-relayer-address> 0 \
  --private-key <deployer-key> --rpc-url $RPC
```

Then fund the address for gas. Repeat per extra EVM signer, and per chain: a key
authorized on the L1 router has no standing on an L2 router.

**`create-clients-cosmos` and `update-eth-client` sign with the same key.** They are
the commands besides `start` that write to Cosmos, so running either against a chain a
`start` process is already relaying will collide. Give it its own key, or run it while
`start` is stopped. The other commands do not touch the Cosmos signer: `update-client`
and `create-clients-eth` submit to Ethereum, `submit-misbehaviour` submits to Ethereum
under its own `MISBEHAVIOUR_PRIVATE_KEY`, and `genesis` broadcasts nothing.

Keys can be set per process without touching `relayer/.env`: `godotenv` does not
override a variable that is already set, so the export wins. Each runbook below
copies its example to `relayer/config.json`, so running two paths at once means
naming the second copy something else:

```bash
COSMOS_PRIVATE_KEY=$KEY_ETH_PATH ./relayer start --config config.json &
COSMOS_PRIVATE_KEY=$KEY_OP_PATH  ./relayer start --config config.op.json &
```

Each key is a distinct account and needs its own gas balance on the Cosmos chain.

## Prover key and verifier must be a pair

Every Cosmos → EVM update (`updateApplicationState`, `updateConsensusState`) carries a
Groth16 proof made with the prover's `bin/n4/pk.bin` and checked by the
`Groth16Verifier_N4` registered in `SignatureVerifier.buckets(4)` on the destination
chain (L1 or L2). The two come from one `scripts/solidity/build-prover-artifacts.sh`
run. If they are from different setups nothing fails at deploy or at send time:
the packet is accepted on Cosmos and the relayer then logs `ProofVerificationFailed()` on
every attempt and re-queues it forever. `pk.bin` is git-ignored and cannot be recovered
once lost; a set generated elsewhere (for example with `go run ./cmd/setup-circuits <dir>` in
the spectre-prover checkout) is a different setup, even though it looks identical.

`scripts/local/check_prover_pairing.sh` checks this and exits 1 with a warning banner:

```bash
# Before deploying: r1cs/pk/vk/verifier on disk are the set that
# build-prover-artifacts.sh published together (.artifacts/solidity/prover/provenance.json).
# r1cs/pk/vk are read from where proverd will load them: PROVER_BIN_DIR from the
# environment, else ./bin in the spectre-prover checkout (PROVER_REPO, default ../prover).
# The relayer loads no keys.
# deploy_eth_contracts.sh and deploy_l2_contracts.sh already run this and stop on a mismatch.
./scripts/local/check_prover_pairing.sh

# Before sending a packet or starting the prover and the relayer: additionally, the verifier registered
# on-chain is byte-identical to what forge compiled from the local Groth16Verifier_N4.sol.
# Needs a `forge build` of the current source (matched by content hash, not by timestamp); --signature-verifier defaults to .l2-deployment.json.
./scripts/local/check_prover_pairing.sh --rpc <evm-rpc> [--signature-verifier <addr>]
```

`PROVER_PAIRING=warn` prints the banner but continues. On a mismatch, regenerate the set
with `scripts/solidity/build-prover-artifacts.sh --force`, then redeploy the contracts and
recreate both clients (a new router also changes `l2_router` in the Cosmos-side client
profile) — see the `ProofVerificationFailed()` row in the L2 [Failure modes](#failure-modes) table.

## Local Cosmos ↔ Ethereum E2E

End-to-end run on local Cosmos + Ethereum nodes. Requires Docker + Kurtosis on
top of the toolchain in [Requirements](../README.md#requirements).

### Gaia binary

The Cosmos scripts need the custom Gaia build carrying the IBC host changes — most
visibly the 08-wasm Stargate allowlist entry for `ClientStatus`, without which L2
client creation fails with `status Unknown`. They default to `../gaia/build/gaiad`
and require that checkout to be **on** `test/ibc-host-customs`:

```bash
cd ../gaia
git checkout test/ibc-host-customs
GOTOOLCHAIN=go1.25.7 make build
cd ../spectre
```

`GAIAD=/path/to/gaiad` or `GAIA_DIR=/path/to/gaia` overrides that, and `GAIAD=`
takes priority over the branch check entirely — which is what you want when the
checkout is on a detached HEAD, or when the binary is already installed elsewhere.
The scripts never fall back to a PATH `gaiad` on their own, because a stock binary
produces a chain that fails several steps later for unrelated-looking reasons.

If you override, confirm you pointed at the right build:

```bash
$GAIAD version                         # -> test/ibc-host-customs-<sha>
strings $GAIAD | grep -c ClientStatus  # -> non-zero
```

> **Tip for local dev**: the setup tool (`cmd/setup-circuits` in the spectre-prover
> checkout; `just build-prover-artifacts` drives it) already defaults `-buckets` to
> `"4"` (`cmd/setup-circuits/main.go:30` there), which
> is the only bucket the local single-validator chain uses — no edit needed.
> Compiling the full family takes 30+ minutes (bucket 128 alone is ~30 min); to
> build the full production set, or a specific subset, pass it explicitly:
>
> ```bash
> (cd ../prover && go run ./cmd/setup-circuits -buckets 4,8,16,32,64,128 ./bin <this-repo>/contracts/verifiers)
> ```
>
> Each value must be a member of the protocol's bucket family declared in
> `witness/buckets.go:17` (`SupportedBuckets`) in the spectre-prover checkout; an
> unsupported value is rejected before any compile starts
> (`witness.ParseBuckets`, `witness/buckets.go:25` there). A bucket that already has keys
> in the output directory is kept, not replaced; `-force` replaces it and moves the old
> `n{N}/` and verifier aside as `*.bak-<time>`.
> `-list-buckets` prints the resolved set without building anything.
>
> The repo ships **no** generated verifiers — `.gitignore` excludes
> `contracts/verifiers/Groth16Verifier_N*.sol`, so `git ls-tree` lists none and a
> fresh clone has none. Step 1 below writes them, and `forge build` fails without
> it (`scripts/E2ETestDeployL2.s.sol` imports `Groth16Verifier_N4`).
>
> Whatever buckets you compile must cover the chain's quorum: a signer count with
> no bucket makes `SignatureVerifier.verifyBatchProof` revert `UnknownBucket(N)`.
> Chains larger than the local devnet need the bigger buckets built and deployed.

```bash
# 1. REQUIRED on a fresh clone: compile per-bucket circuits + emit
#    Groth16Verifier_N{N}.sol. Neither the circuit artifacts nor the verifiers are
#    committed. Re-run when circuit code changes — and redeploy the verifiers with
#    it, because a new Setup() means a new verifying key and stale verifiers reject
#    every proof.
#    CPU default:
#    (the supported path: runs the spectre-prover setup tool, keeps the keys in its bin/
#    and publishes only the verifiers into contracts/verifiers)
just build-prover-artifacts
#    GPU variant, from the spectre-prover checkout:
#    cd ../prover && go run -tags=icicle ./cmd/setup-circuits -gpu-prove ./bin <this-repo>/contracts/verifiers

# 2. Build the relayer binary (it links no gnark and no native library)
cd relayer
go build -o relayer ./cmd
#    Start the prover in its own terminal and leave it running; the relayer proves through
#    it over gRPC at PROVER_ADDR (127.0.0.1:50060 by default, from relayer/.env). It needs the
#    keys from step 1 and libgaraga_rs (build it once in the prover checkout with
#    scripts/build-garaga-ffi.sh). GPU: set GPU_PROVE=1 here, with an icicle build of proverd.
#      (cd .. && scripts/local/run_prover.sh)

# 3. Start Ethereum first and wait until the beacon node finalizes.
#    Beacon RPC is pinned to 32101 via eth-network-params.yaml (public_port_start).
#    run_eth_node.sh owns only the node (endpoints -> .eth-devnet-run/eth.env);
#    deploy_eth_contracts.sh is the deploy half (reads that handoff).
./scripts/local/run_eth_node.sh         # Kurtosis Ethereum testnet (node only)
# Poll until finalized.epoch > 0:
curl -s http://127.0.0.1:32101/eth/v1/beacon/states/head/finality_checkpoints
#    The deploy patches relayer/config.json, so create it first. Each path has a
#    runnable example carrying just that path's module pair; config.example.json
#    is the catalogue of all of them and is NOT runnable on its own.
cp relayer/config.ethereum.example.json relayer/config.json
./scripts/local/deploy_eth_contracts.sh # deploy core contracts + patch relayer config
#    Deploy with the SAME key the relayer runs with: E2ETestDeploy sets
#    relayers[0] = msg.sender, so the deployer receives the ICS26Router relayer role.
#    Defaults to the devnet key relayer/.env ships; on any other network set
#    ETH_DEPLOYER_ADDRESS + ETH_DEPLOYER_PRIVATE_KEY (and E2E_FAUCET_ADDRESS if the
#    test ERC20 should go elsewhere).

# 4. Then start Cosmos and submit the Ethereum LC WASM via governance
./scripts/local/run_cosmos_node.sh   # local Cosmos chain with funded test accounts
./scripts/local/wasm.sh              # submit + vote-pass the Ethereum LC WASM proposal
#
# Multi-validator alternative — spin up N nodes (180 default) with a
# Cosmos-Hub-like staked-power distribution so the relayer exercises a
# real signer-selection path (top ~20 hold ~2/3). Each node's RPC is
# striped from :31000.
#    NUM_NODES=20 ./scripts/local/run_cosmos_node_n.sh
#    NUM_NODES=20 ./scripts/local/wasm_n.sh
# After multi-node bring-up, point `relayer/config.example.json` and any
# `gaiad` --node / --home flags at the val0 home + RPC (defaults
# $HOME/.gaia-multi/val0 + tcp://127.0.0.1:31000).

# 5. Create the light clients, one command per chain. The Cosmos side comes first
#    (creates the 08-wasm ETH client, writing cosmos_wasm_client_id), then the ETH
#    side deploys the Tendermint light client wired to that id (writing
#    spectre_client). Both write back into relayer/config.json automatically.
#    create-clients-eth refuses to deploy unless update_client, membership and
#    misbehaviour are set in the config and hold code on eth_rpc_url (the
#    SpectreClient constructor rejects a module without code as well); the deploy
#    script in step 3 patches all three in.
./relayer create-clients-cosmos --config config.json --wasm-checksum <hex-from-wasm.sh>
./relayer create-clients-eth    --config config.json --trust-level 2/3

# 6. Start the bi-directional relay loop. Use the SAME config the steps above
#    patched — deploy_eth_contracts.sh and create-clients-* write into config.json.
./relayer start --config config.json
#    GPU proving is proverd's setting (GPU_PROVE=1), not the relayer's.
#
#    Multiple Cosmos sources: ONE PROCESS PER SOURCE. The relayer refuses a config
#    that declares more than one relay path at load, naming each path it found — so give
#    each source its own config file with its own `ics26_client_id` (the ETH
#    router's client id for that Cosmos chain; config.example.json uses
#    "cosmoshub-1", a second source might be "osmosis-1") and its own state
#    directory. Create each source's clients from its own file, then start one
#    process per file:
#      ./relayer create-clients-cosmos --config config-osmosis.json --wasm-checksum <hex>
#      ./relayer create-clients-eth    --config config-osmosis.json
#      ./relayer start --config config.json &
#      ./relayer start --config config-osmosis.json &
#    Give each process its own COSMOS_PRIVATE_KEY, and — because every Cosmos
#    source relays into the SAME L1 router — its own ETH_PRIVATE_KEY too. See
#    "Signing keys when several processes share a chain" above for why, for what
#    each extra ETH key has to be granted, and for the cases where sharing one
#    ETH key is fine.

# 7. send packet

. ./scripts/local/gaiad_binary.sh
ABS_TIMEOUT=$(($(date +%s) + 2000))

"$GAIAD" tx ibc-transfer transfer transfer 08-wasm-0 0x8943545177806ed17b9f23f0a21ee5948ecaa776 1000stake \
  --from test1 \
  --home "$HOME/.gaia" \
  --chain-id test-ibc-eth \
  --node tcp://127.0.0.1:26657 \
  --keyring-backend test \
  --gas-prices 1stake \
  --absolute-timeouts \
  --packet-timeout-timestamp "$ABS_TIMEOUT" \
  --generate-only \
| jq '.body.messages[0].encoding = "application/x-solidity-abi"' \
| "$GAIAD" tx sign /dev/stdin \
    --from test1 \
    --home "$HOME/.gaia" \
    --chain-id test-ibc-eth \
    --keyring-backend test \
| "$GAIAD" tx broadcast /dev/stdin \
    --node tcp://127.0.0.1:26657 \
    -y


# 8. Check it landed. Addresses come from step 3's output, not from here —
#    ICS20Transfer is printed by deploy_eth_contracts.sh, and the wrapper is
#    derived from the denom trace (which carries the client the token arrived on).
WRAPPED=$(cast call <ICS20Transfer> 'ibcERC20Contract(string)(address)' \
  'transfer/<cosmos-client-on-eth>/stake' --rpc-url <eth-rpc>)
cast call "$WRAPPED" 'balanceOf(address)(uint256)' <receiver> --rpc-url <eth-rpc>

#    And the ack on the Cosmos side:
"$GAIAD" q txs --query "message.action='/ibc.core.channel.v2.MsgAcknowledgement'" \
  --node tcp://127.0.0.1:26657 -o json
```

Send an ICS-20 transfer from Cosmos to trigger a client update + `recvPacket`
round-trip; the `[UpdateCosmosClient]` log line reports the chosen bucket.

### `router_deploy_block`: where packet-flush log lookups start

Optional, for every EVM source. The packet-flush backstop fetches each outstanding
packet's event with `eth_getLogs`; this setting picks the block range it searches.

- **Unset (`0`):** walks back from the head at most **64 windows**. A window is the
  first of these that applies:
  1. `log_scan_chunk`, on an L2 return module that sets it;
  2. the span the provider narrowed to after refusing a wider range — once narrowed,
     it stays narrowed for every later lookup in the process;
  3. otherwise **10 000 blocks**.

  So the walk covers `64 × window` blocks, not a fixed 640 000. With the 10 000
  default that is about 89 days on L1 (12 s blocks), about 15 days on a 2 s L2, and
  about 44 hours on Arbitrum (~0.25 s blocks). With `log_scan_chunk: 10` (Alchemy free
  tier) it is **640 blocks — under 3 minutes on Arbitrum**. An older event is skipped
  with `no log in the newest N blocks ... giving up on it this pass`; `N` is
  approximate if the window narrowed during that walk.
- **Set:** the first query is one call over `[router_deploy_block, head]`, which
  covers the whole history **only if the provider serves that range in one call**. If
  it refuses, the relayer narrows the window and walks back from the head as above:
  never below `router_deploy_block`, but still capped at 64 windows. On a provider
  with a range cap, a deploy block therefore does **not** reach events older than
  `64 × cap` blocks — for those, use a provider without a range cap. On an uncapped
  provider, a deploy block far below the oldest outstanding packet costs one wider
  `eth_getLogs` per outstanding sequence per flush pass.

On a node with pruned history (`pruned history unavailable`), the relayer starts at
the lowest block the node still serves and logs `provider has pruned history below
block N`. Events older than that need an archive provider. This mostly hits L1 geth
nodes.

| Log line | Meaning |
|----------|---------|
| `[... Flush] provider has pruned history below block N` | The node dropped older logs; lookups start at N. Older packets need an archive provider. |
| `[... Flush] seq=N: no log in the newest M blocks ... giving up` | The event is older than the 64-window walk. On an uncapped provider, set `router_deploy_block`. On a capped one (`provider refused a ... log range` earlier in the log, or `log_scan_chunk` set), a deploy block does not help; use a provider without a range cap. |

**Value:** the block the router **proxy** was deployed at (`ics26_address`, or
`l2_router` on an L2). Too low only widens the query; too high silently hides older
packets — when unsure, go lower. From the forge broadcast of the deploy
(`E2ETestDeployL2.s.sol` for an L2), or the proxy's creation tx in a block explorer:

```bash
jq -r --arg a "<ics26_address>" '.receipts[]
  | select((.contractAddress // "" | ascii_downcase) == ($a | ascii_downcase))
  | .blockNumber' broadcast/E2ETestDeploy.s.sol/<chain-id>/run-latest.json | xargs cast to-dec
```

**Module:** Ethereum → `cosmos-to-eth`. L2 → the return module (`op-to-cosmos`,
`arbitrum-to-cosmos`, `base-to-cosmos`); the forward module ignores it. The
`config.*.example.json` files already carry the key as `0`.

## Local Cosmos ↔ OP E2E

Brings up an L1 + OP Stack L2 + Cosmos and relays both directions. The L1 is shared:
`run_optimism_node.sh` calls `run_eth_node.sh` for it (Fusaka-from-genesis, the same
`eth-network-params.yaml` the ETH↔Cosmos devnet uses), which reuses the enclave's L1
if one is already up — see [One L1, shared between rollups](#one-l1-shared-between-rollups).

```bash
# 1. L1 (Fulu) + OP Stack L2 in one Kurtosis enclave. Ends with games proposed
#    and .op-devnet-run/attestor.env written (L1/L2/op-node/beacon endpoints).
./scripts/local/run_optimism_node.sh

# 2. Attestor — independent verifier over the replica op-node; serves gRPC :3001.
#    Sources attestor.env automatically. Every attestor script defaults to
#    GRPC_PORT=3001, so a host running several needs distinct ports; the
#    config.example.json modules expect op :3001, arbitrum :3002, base :3003.
./scripts/local/run_op_attestor.sh

# 3. Cosmos node, then gov-store the OP light-client wasm. The Ethereum client
#    is NOT needed for a Cosmos<->OP deployment: the attestor-trusted L2 client
#    authenticates nothing through it. Add ./scripts/local/wasm.sh only if this
#    config also relays Cosmos<->Ethereum.
./scripts/local/run_cosmos_node.sh
./scripts/local/wasm_op.sh     # -> OP client checksum

# 4. IBC contracts on the L2 (E2ETestDeployL2). Deploy with the SAME key the relayer
#    will run with (relayer/.env ETH_PRIVATE_KEY): E2ETestDeployL2 grants the
#    ICS26Router relayer role to msg.sender, and without that role every
#    updateApplicationState reverts. The values below are the devnet-only key that
#    relayer/.env ships as ETH_PRIVATE_KEY — change BOTH together or they drift apart.
#    The account also needs an L2 balance (the L1 devnet faucet address has none
#    there), but funding alone does NOT fix the role. Fund it by depositing through
#    the OptimismPortal (OPTIMISM_PORTAL in .op-devnet-run/attestor.env).
#    The script first runs check_prover_pairing.sh and stops when pk.bin does not
#    belong to Groth16Verifier_N4.sol -- see "Prover key and verifier must be a pair".
DST_CHAIN=opstack \
L2_DEPLOYER_ADDRESS=0x8943545177806ED17B9F23F0a21ee5948eCaa776 \
L2_DEPLOYER_PRIVATE_KEY=bcdf20249abf0ed6d944c0288fad489e33f66b3960d9e6229c1cd214ed3bbe31 \
  ./scripts/local/deploy_l2_contracts.sh
# It patches relayer/config.json (override with RELAYER_CONFIG), both the forward
# module and the matching l2_to_cosmos module's rollup_profile.common.l2_router.

# 5. Clients. The OP client on Cosmos, then the L2 side (SpectreClient deployed
#    + addClient'd). Copy op-l2-config.example.json and fill in the checksum from
#    step 3, the L2 router address from step 4, and the L2 chain id.
cd relayer
./relayer create-clients-cosmos --config config.json --l2-config op-l2-config.json
./relayer create-clients-eth --config config.json --trust-level 2/3

# 6. Relay both directions. Before this (and before the first packet), confirm the
#    verifier now registered on the L2 is the one pk.bin was set up with; a mismatch
#    prints a warning and exits 1 instead of letting packets re-queue forever.
(cd .. && . .op-devnet-run/attestor.env && forge build && ./scripts/local/check_prover_pairing.sh --rpc "$L2_RPC_URL")
./relayer start --config config.json
```

### Running against an existing L2 (no devnet L2)

The flow above brings up its own L2. To relay against a node someone else operates —
OP Sepolia, or a replica you already run — skip step 1 and point the attestor at it.
There is no local L1 to start either: the attestor-trusted client verifies nothing
against L1, so only the *attestor* needs an L1 RPC, and a public endpoint is enough.

The predecessor of this flow was verified against OP Sepolia. The authenticated flow below
includes every required signing input, but must be revalidated live on the current stack before
it can be described as verified end to end.

```bash
# 1. Attestor against the existing replica. NETWORK must name the real chain, and
#    the dispute-game contracts cannot be auto-resolved for a public network — read
#    them off the chain once (see below) and pass them.
OP_NODE_RPC_URL=http://<host>:9545 \
L1_RPC_URL=https://ethereum-sepolia-rpc.publicnode.com \
NETWORK=op-sepolia SRC_CHAIN=op-sepolia \
L2_CHAIN_ID=11155420 \
ATTESTOR_SIGNING_KEY=<32-byte-ed25519-seed-hex> \
ATTESTATION_HEAD=unsafe DERIVED_GAP_BLOCKS=10 \
DISPUTE_GAME_FACTORY=0x05F9613aDB30026FFd634f38e5C4dFd30a197Fa1 \
RESPECTED_GAME_TYPE=8 \
  ./scripts/local/run_op_attestor.sh

# 2. Cosmos + the OP wasm, unchanged from the devnet flow.
GAIAD=~/go/bin/gaiad ./scripts/local/run_cosmos_node.sh
GAIAD=~/go/bin/gaiad ./scripts/local/wasm_op.sh

# 3. config.json: one runnable config per path, already cut to the OP module
#    pair. The deploy below patches this file, so it has to exist first.
cp relayer/config.op.example.json relayer/config.json
# then fill in the fields listed in "Values you fill in by hand" below.

# 4. Deploy the L2 contracts with a key funded ON THAT L2.
#    DO THIS BEFORE STARTING THE ATTESTOR if both share one rate-limited RPC —
#    see "One RPC budget, two processes" in the Arbitrum section.
DST_CHAIN=opstack L2_RPC=http://<host>:8545 RELAYER_CONFIG=relayer/config.json \
L2_DEPLOYER_ADDRESS=0x... L2_DEPLOYER_PRIVATE_KEY=... \
  ./scripts/local/deploy_l2_contracts.sh

# 5. l2-config, then clients and relay. The relayer must sign with the SAME key
#    that deployed (E2ETestDeployL2 grants the router role to msg.sender). Pass it
#    in the environment rather than editing relayer/.env — godotenv does not
#    override a variable already set, so the export wins.
cp relayer/op-l2-config.example.json relayer/op-l2-config.json
# fill in wasm_checksum (step 2), l2_rpc_url, l2_router (= ICS26_ADDRESS from step 4),
# and the attestor public key as described below
cd relayer
./relayer create-clients-cosmos --config config.json --l2-config op-l2-config.json
ETH_PRIVATE_KEY=<deployer-key> ./relayer create-clients-eth --config config.json \
  --trust-level 2/3
ETH_PRIVATE_KEY=<deployer-key> ./relayer start --config config.json
```

#### Values you fill in by hand, and where each comes from

Everything not listed here is written for you — see
[What the tooling fills in for you](#what-the-tooling-fills-in-for-you).

The attestor logs its public key twice at startup: hex for human comparison and standard base64
for the relayer. Copy the base64 value from
`.op-attestor-run/attestor.log` into both locations below before creating the client. They must
contain the public half of the exact `ATTESTOR_SIGNING_KEY` supplied in step 1; leaving the
checked-in disposable devnet key in either file lets the daemon start but makes every certificate
fail against the client or relayer configuration.

**`relayer/config.json`:**

| Field | Module | Value comes from |
|---|---|---|
| `eth_rpc_url` | `cosmos-to-op` | your L2 HTTP endpoint |
| `eth_ws_url` | `cosmos-to-op` | your L2 WebSocket endpoint; optional, the relayer skips the WS client when empty |
| `l2_rpc_url` | `op-to-cosmos` | the same L2 HTTP endpoint (step 4 overwrites it with `L2_RPC` anyway) |
| `attestor_endpoints[0].address` | `op-to-cosmos` | `127.0.0.1:$GRPC_PORT` from step 1; the example ships `127.0.0.1:3001`, which matches the script default. The list takes one `{address, attestor_index}` entry per attestor daemon, and the config is rejected when it has fewer entries than `attestors.threshold` |
| `attestor_endpoints[0].attestor_index` | `op-to-cosmos` | the position of that attestor's key in `attestors.public_keys` (`0` for the single local attestor). Each index may appear once and must lie inside the key list |
| `attestor_src_chain` | `op-to-cosmos` | the `SRC_CHAIN` you passed in step 1 (`op-sepolia`) — must match, or `AttestedUpTo` answers for another chain |
| `head_kind` | `op-to-cosmos` | the same choice as `ATTESTATION_HEAD` in step 1. `start` asks each attestor's `Info` for the head it gates on and refuses to boot when the two differ — the attestor signs only its configured head, so a mismatch relays nothing |
| `rollup_profile.common.l2_chain_id` | `op-to-cosmos` | `eth_chainId` on your L2 (`11155420` on OP Sepolia). `start` verifies this against the RPC and refuses to boot on a mismatch |
| `attestors.public_keys` | `op-to-cosmos` | the base64 public key logged by the attestor started with `ATTESTOR_SIGNING_KEY` |
| `log_scan_chunk` | `op-to-cosmos` | your provider's `eth_getLogs` span cap — measure it, see the Arbitrum section |
| `router_deploy_block` | `op-to-cosmos` | optional — block the `l2_router` proxy was deployed at; see [above](#router_deploy_block-where-packet-flush-log-lookups-start) |

**`relayer/op-l2-config.json`:** `wasm_checksum` (from `wasm_op.sh`), `l2_rpc_url`, and
`rollup_profile.common.l2_router` (= `ICS26_ADDRESS` printed by step 4 — which is why
this file is filled in after the deploy), plus the same base64 key in
`attestors.public_keys`.

**Resolving the dispute-game contracts.** `run_op_attestor.sh` can derive them with
`cast` for a chain it knows; for a public network it exits with
`set DISPUTE_GAME_FACTORY and RESPECTED_GAME_TYPE`. Read them from the chain — the
portal address comes from op-node itself, so nothing is hardcoded:

```bash
PORTAL=$(curl -s -X POST -H 'content-type: application/json' \
  --data '{"jsonrpc":"2.0","id":1,"method":"optimism_rollupConfig","params":[]}' \
  http://<host>:9545 | jq -r .result.deposit_contract_address)
cast call $PORTAL "disputeGameFactory()(address)" --rpc-url $L1_RPC_URL
cast call $PORTAL "respectedGameType()(uint32)"  --rpc-url $L1_RPC_URL
```

**The replica must serve `eth_getProof` below its head.** The relayer proves the
router account at the attested height, which trails the head, and op-reth's default
proof window is small — a node serving only `latest` fails every build with
`distance to target block exceeds maximum proof window`. The mode-2 bring-up in
`run_op_attestor.sh` starts op-reth with `--rpc.eth-proof-window=100000`; an external
replica needs the operator to have done the same. Check before starting:

```bash
H=$(cast block-number --rpc-url $L2)
cast rpc eth_getProof '["<router>",[],"'$(printf '0x%x' $((H-1000)))'"]' --rpc-url $L2
```

**Gaia must be the custom-host build.** `gaiad_binary.sh` requires the checkout to be
*on* branch `test/ibc-host-customs`, which a detached HEAD fails even when the code is
right. Point at an already-built binary instead — `GAIAD=` takes priority and skips
the branch check. Confirm it is the right one:

```bash
$GAIAD version                       # → test/ibc-host-customs-<sha>
strings $GAIAD | grep -c ClientStatus  # the 08-wasm allowlist entry L2 clients need
```

**Set `DERIVED_GAP_BLOCKS` explicitly.** The local devnet handoffs export `5`; an
attestor attached to an external replica inherits nothing and takes the default 150,
which puts a ~5 minute floor under the return direction. See the latency section above.

## Local Cosmos ↔ Arbitrum E2E

Brings up an L1 + Arbitrum Nitro/BoLD L2 + Cosmos and relays both directions. The L1
is shared with the OP flow: `run_arbitrum_node.sh` attaches Arbitrum to the existing
Kurtosis enclave if OP already brought one up, or creates the same local L1 itself
through `run_eth_node.sh` when the enclave does not exist — see
[One L1, shared between rollups](#one-l1-shared-between-rollups).

```bash
# 1. L1 (Fulu) + Arbitrum Nitro/BoLD L2 in one Kurtosis enclave. Ends with a
#    finalized AssertionCreated check and .arbitrum-devnet-run/attestor.env written
#    (L1/L2/beacon/Nitro feed/RollupCore endpoints).
./scripts/local/run_arbitrum_node.sh

# 2. Attestor — independent non-sequencing Nitro replica; serves gRPC :3001.
#    Sources .arbitrum-devnet-run/attestor.env automatically.
GRPC_PORT=3002 ./scripts/local/run_arbitrum_attestor.sh

# 3. Cosmos node, then gov-store the Arbitrum light-client wasm. The Ethereum
#    client is NOT needed for a Cosmos<->Arbitrum deployment — same as OP, the
#    attestor-trusted L2 client authenticates nothing through it. Add
#    ./scripts/local/wasm.sh only if this config also relays Cosmos<->Ethereum.
./scripts/local/run_cosmos_node.sh
./scripts/local/wasm_arb.sh    # -> Arbitrum client checksum

# 4. IBC contracts on the Arbitrum L2 (E2ETestDeployL2). Use the same relayer key
#    rules as OP: the deployer receives the ICS26Router relayer role, so it must be
#    the key in relayer/.env ETH_PRIVATE_KEY unless you change both together.
DST_CHAIN=arbitrum \
L2_DEPLOYER_ADDRESS=0x8943545177806ED17B9F23F0a21ee5948eCaa776 \
L2_DEPLOYER_PRIVATE_KEY=bcdf20249abf0ed6d944c0288fad489e33f66b3960d9e6229c1cd214ed3bbe31 \
  ./scripts/local/deploy_l2_contracts.sh
# It patches the cosmos_to_l2 module whose dst_chain is arbitrum.

# 5. Clients. Cosmos side first, one invocation per kind, then the L2 side
#    (SpectreClient deployed + addClient'd). The Arbitrum client does not anchor to
#    an Ethereum client, so create it on its own:
cd relayer
./relayer create-clients-cosmos --config config.json --l2-config <arb-l2-config.json>
#
#    Only run the Ethereum half (--wasm-checksum, no --l2-config) if this config also
#    relays Cosmos<->Ethereum AND that client does not exist yet. Re-running it against
#    a working deployment rewrites cosmos_to_eth.cosmos_wasm_client_id and repoints the
#    path at a client the Sepolia-side SpectreClient was never registered against; every
#    recvPacket then reverts on a counterparty mismatch.
./relayer create-clients-eth --config config.json --trust-level 2/3

# 6. Relay both directions.
./relayer start --config config.json
```

Copy `relayer/arb-l2-config.example.json` and fill in three values. It is the same
shape as OP's and Base's `--l2-config` — five profile keys and nothing else. There is
no `protocol` block, no `assertions_mapping_slot` and no `rollup` address in it: the
client verifies no assertion, so nothing about BoLD reaches it. Those values configure
the **attestor**, not the client.

- `wasm_checksum`: checksum printed by `wasm_arb.sh`.
- `l2_rpc_url`: `.arbitrum-devnet-run/attestor.env` `L2_RPC_URL`.
- `rollup_profile.common.l2_router`: `ICS26_ADDRESS` from `deploy_l2_contracts.sh`.
- `rollup_profile.common.l2_chain_id`: `L2_CHAIN_ID` from the handoff (`412346` on the
  local devnet, `421614` on Arbitrum Sepolia).
- `counterparty_client_id`: must equal the `cosmos_to_l2` module's `ics26_client_id`
  (`arb-client-0` in the example config) — both name the same client on the L2 router.

`l2_header_fork` is `london`, not `prague` as on OP and Base: Nitro emits none of the
post-London optional header fields.

### Running against an existing Arbitrum L2 (no devnet L2)

The Arbitrum counterpart of the OP section above. There is no local L1 and no local
Nitro: the attestor reads a public Arbitrum RPC and a public Sepolia RPC, and the
client verifies nothing against L1, so nothing else needs an L1 endpoint.

The predecessor of this flow was verified against Arbitrum Sepolia. The authenticated flow below
includes every required signing input, but must be revalidated live on the current stack before
it can be described as verified end to end.

```bash
# 1. Attestor. CHAIN_PROFILE supplies the RollupCore address, both chain ids, the
#    BoLD slot/offset and a scan start block near the head, all from
#    attestor/arbitrum/config.arbitrum-sepolia.json — pass only the endpoints.
CHAIN_PROFILE=arbitrum-sepolia \
L1_RPC_URL=https://ethereum-sepolia-rpc.publicnode.com \
L2_RPC_URL=<arbitrum-sepolia-rpc> \
L2_WS_URL=<arbitrum-sepolia-ws> \
ATTESTOR_SIGNING_KEY=<32-byte-ed25519-seed-hex> \
ATTESTATION_HEAD=unsafe DERIVED_GAP_BLOCKS=10 GRPC_PORT=3002 DETACH=1 \
  ./scripts/local/run_arbitrum_attestor.sh

# 2. Cosmos + the Arbitrum wasm, unchanged from the devnet flow.
GAIAD=~/go/bin/gaiad ./scripts/local/run_cosmos_node.sh
GAIAD=~/go/bin/gaiad ./scripts/local/wasm_arb.sh          # -> checksum

# 3. config.json: one runnable config per path, already cut to the Arbitrum pair.
cp relayer/config.arbitrum.example.json relayer/config.json
# then fill in the seven fields listed in "Values you fill in by hand" below.

# 4. Deploy the L2 contracts with a key funded ON Arbitrum Sepolia.
#    DO THIS BEFORE STARTING THE ATTESTOR if both share one rate-limited RPC —
#    see "One RPC budget, two processes" below.
DST_CHAIN=arbitrum L2_RPC=<arbitrum-sepolia-rpc> RELAYER_CONFIG=relayer/config.json \
L2_DEPLOYER_ADDRESS=0x... L2_DEPLOYER_PRIVATE_KEY=... \
  ./scripts/local/deploy_l2_contracts.sh
# Patches BOTH modules: ics26_address on the forward one, rollup_profile.common.l2_router
# and l2_rpc_url on the return one.

# 5. l2-config, then clients. The relayer must sign with the SAME key that deployed
#    (E2ETestDeployL2 grants the router role to msg.sender). Pass it in the
#    environment rather than editing relayer/.env — godotenv does not override a
#    variable already set, so the export wins.
cp relayer/arb-l2-config.example.json relayer/arb-l2-config.json
# fill in wasm_checksum (step 2), l2_rpc_url, l2_router (= ICS26_ADDRESS from step 4),
# and the attestor public key as described below
cd relayer
./relayer create-clients-cosmos --config config.json --l2-config arb-l2-config.json
ETH_PRIVATE_KEY=<deployer-key> ./relayer create-clients-eth --config config.json \
  --trust-level 2/3
ETH_PRIVATE_KEY=<deployer-key> ./relayer start --config config.json
```

**`l2_wasm_client_id` is an output, not an input.** `create-clients-cosmos` creates that
client and writes its id back into the module, overwriting whatever is there — the
example's `08-wasm-N` placeholder or an empty string alike. Every other command still
requires it, because by then the client must exist.

**Set `log_scan_chunk` to your provider's `eth_getLogs` span cap.** The relayer scans
L2 packet logs over ranges, and a provider that caps the span rejects the call rather
than truncating it. Alchemy's free tier caps at **10 blocks**, drpc at 10 000; `0`
means one call per range and is right for a provider with no cap. Check yours:

```bash
H=$(cast block-number --rpc-url $L2)
cast rpc eth_getLogs '[{"fromBlock":"'$(printf '0x%x' $((H-1000)))'","toBlock":"'$(printf '0x%x' $H)'"}]' --rpc-url $L2
```

**The L2 RPC must serve `eth_getProof` well below the head.** The relayer proves the
router account at the attested height, which trails the head. A pruned node fails every
build. Alchemy serves it at archive depth; check before starting:

```bash
H=$(cast block-number --rpc-url $L2)
cast rpc eth_getProof '["<router>",[],"'$(printf '0x%x' $((H-5000)))'"]' --rpc-url $L2
```

**With `CHAIN_PROFILE` set to anything but `devnet`, a leftover `.arbitrum-devnet-run/`
is ignored.** The devnet handoff exports `ROLLUP_CORE_ADDRESS` and both chain ids as real
environment variables, which would otherwise beat the profile and silently produce
`l2_chain_id=412346` and `src_chain=arbdev` against a public RPC. The script says so when
it skips the handoff.

#### Values you fill in by hand, and where each comes from

Everything else in both files is written for you — the table after this one says by what.

The attestor logs its public key twice at startup: hex for human comparison and standard base64
for the relayer. Copy the base64 value from
`.arbitrum-attestor-run/attestor.log` into both locations below before creating the client. They
must contain the public half of the exact `ATTESTOR_SIGNING_KEY` supplied in step 1; leaving the
checked-in disposable devnet key in either file lets the daemon start but makes every certificate
fail against the client or relayer configuration.

**`relayer/config.json`:**

| Field | Module | Value comes from |
|---|---|---|
| `eth_rpc_url` | `cosmos-to-arbitrum` | your L2 HTTP endpoint |
| `eth_ws_url` | `cosmos-to-arbitrum` | your L2 WebSocket endpoint. Optional — the relayer skips the WS client when it is empty, and the L2→Cosmos direction polls regardless |
| `l2_rpc_url` | `arbitrum-to-cosmos` | the same L2 HTTP endpoint. Step 4 overwrites this with `L2_RPC`, so a mismatch here is corrected rather than fatal |
| `attestor_endpoints[0].address` | `arbitrum-to-cosmos` | `127.0.0.1:$GRPC_PORT` from step 1. The example ships `127.0.0.1:3002`, which matches the `GRPC_PORT=3002` above — **change it if you used a different port**, or the relayer dials a closed socket. The list takes one `{address, attestor_index}` entry per attestor daemon, and the config is rejected when it has fewer entries than `attestors.threshold` |
| `attestor_endpoints[0].attestor_index` | `arbitrum-to-cosmos` | the position of that attestor's key in `attestors.public_keys` (`0` for the single local attestor). Each index may appear once and must lie inside the key list |
| `attestor_src_chain` | `arbitrum-to-cosmos` | `src_chain` in `attestor/arbitrum/config.<profile>.json` (`arbitrum-sepolia`). Must equal what the attestor reports, or `AttestedUpTo` answers for a chain you did not ask about |
| `head_kind` | `arbitrum-to-cosmos` | `unsafe`, `safe` or `finalized` — the same choice as `ATTESTATION_HEAD` in step 1. `start` asks each attestor's `Info` for the head it gates on and refuses to boot when the two differ — the attestor signs only its configured head, so a mismatch relays nothing |
| `rollup_profile.common.l2_chain_id` | `arbitrum-to-cosmos` | `eth_chainId` on your L2 (`421614` on Arbitrum Sepolia, `412346` on the local devnet). `start` verifies this against the RPC and refuses to boot on a mismatch |
| `attestors.public_keys` | `arbitrum-to-cosmos` | the base64 public key logged by the attestor started with `ATTESTOR_SIGNING_KEY` |
| `log_scan_chunk` | `arbitrum-to-cosmos` | your provider's `eth_getLogs` span cap — measure it, see below |
| `router_deploy_block` | `arbitrum-to-cosmos` | optional — block the `l2_router` proxy was deployed at; see [above](#router_deploy_block-where-packet-flush-log-lookups-start) |

**`relayer/arb-l2-config.json`:**

| Field | Value comes from |
|---|---|
| `wasm_checksum` | the hex printed by `wasm_arb.sh` in step 2 |
| `l2_rpc_url` | your L2 HTTP endpoint |
| `rollup_profile.common.l2_router` | `ICS26_ADDRESS` printed by step 4 — so this file is filled in *after* the deploy, which is why step 5 comes after step 4 |
| `attestors.public_keys` | the same base64 public key logged by the attestor |

`counterparty_client_id` and `l2_chain_id` in that file already match the example config
(`arb-client-0`, `421614`); change them only if you changed the module's
`ics26_client_id` or are on a different chain.

#### What the tooling fills in for you

Identical for OP, Arbitrum and Base. Do not pre-fill these — the commands overwrite
whatever is there:

| Field | Written by |
|---|---|
| `ics26_address`, `membership`, `update_client`, `signature_verifier`, `misbehaviour` | `deploy_l2_contracts.sh` (forward module) |
| `rollup_profile.common.l2_router`, `l2_rpc_url` | `deploy_l2_contracts.sh` (return module — it prints `Patched the return module "…"`) |
| `cosmos_wasm_client_id`, `l2_wasm_client_id` | `create-clients-cosmos`, which overwrites the example's `08-wasm-N` placeholder |
| `spectre_client` | `create-clients-eth` |

#### One RPC budget, two processes

Applies to every L2 in this document; the numbers below were measured on Arbitrum
Sepolia.

**The attestor and the relayer both hammer the L2 endpoint, and on a free tier they do
not fit together.** The attestor issues `runtime_backfill_concurrency` (8) parallel
header fetches every refresh; the relayer adds `eth_getLogs` scans, `eth_getProof`
builds and transaction sends. Against one Alchemy free-tier key this measurably
exceeds the concurrent-request budget:

- `deploy_l2_contracts.sh` fails outright with `Max retries exceeded HTTP error 429
  with empty body` when the attestor is already running. Pausing the attestor
  (`docker stop spectre-arbitrum-attestor`) makes the same command succeed.
- With both running, the attestor logs `backing off: failures=3 delay=40.5s
  rate_limited=true` and its frontier stops. The relayer then reports `source relayable
  height` frozen for minutes with `DERIVED_GAP_BLOCKS=10` set, which looks like a
  relayer problem and is not one.

Two ways out, in order of preference:

**Give each process its own endpoint.** The relayer does not need a keyed provider.
Measured on Arbitrum Sepolia, both of these serve what it requires with no API key:

| Endpoint | `eth_getProof` at head−500 | `eth_getLogs` span | WebSocket |
|---|---|---|---|
| `https://sepolia-rollup.arbitrum.io/rpc` | yes | ≥10 000 | — |
| `https://arbitrum-sepolia.drpc.org` | yes | ≥10 000 | `wss://arbitrum-sepolia.drpc.org` |
| `https://arbitrum-sepolia-rpc.publicnode.com` | **no** — fails below ~head−128 | ≥10 000 | `wss://…publicnode.com` |

So point `L2_RPC_URL` (attestor) at the keyed endpoint and the relayer's `eth_rpc_url` /
`l2_rpc_url` at a keyless one, and neither starves the other. Do not use publicnode for
the relayer: it cannot prove the router account at the attested height.

**Or serialise the load.** Deploy the contracts first, then start the attestor, and
lower `RUNTIME_BACKFILL_CONCURRENCY` if the attestor still trips the limit.

#### Adding an L2 to a deployment that already relays Cosmos↔Ethereum

`create-clients-cosmos` creates one kind of client per invocation. Without `--l2-config`
it creates the Ethereum light client for the Cosmos↔Ethereum path and rewrites the
`cosmos_to_eth` module's `cosmos_wasm_client_id`. With `--l2-config` it creates only the
named L2 clients and does not touch the Ethereum one:

```bash
# Adding Arbitrum to a running deployment — the Ethereum client is left alone
./relayer create-clients-cosmos --config config.json --l2-config <arb-l2-config.json>
```

`--wasm-checksum` is not needed there (nothing Ethereum-side is being created); the L2
client's own wasm checksum comes from the `--l2-config` file.

Keeping the two apart matters: re-running the Ethereum half against a working deployment
replaces a client the Ethereum-side SpectreClient is already registered against, so every
`recvPacket` starts reverting on a counterparty mismatch while the original client is
orphaned with nothing advancing it until it expires.

Several L2s can still be created in one run:

```bash
./relayer create-clients-cosmos --config config.json \
  --l2-config op.json --l2-config base.json --l2-config arb.json
```

The `--l2-config` file carries the full ICS-08 profile (see
[docs/L2_CLIENTS.md](L2_CLIENTS.md#creation-and-profile)). For the
attestor-trusted clients `rollup_profile.common` is five keys — `l2_chain_id`,
`l2_router`, `commitment_slot`, `profile_version`, and `l2_header_fork` — and nothing
else; an unknown key fails `instantiate` on the Rust side. `profile_version` must match
the wasm artifact (`op_attestor_v1`, `base_attestor_v1`, `arbitrum_attestor_v1`), and
`l2_header_fork` must match the chain's execution-header layout: `prague` for OP and
Base, `london` for Arbitrum Nitro, which produces none of the post-London header fields.
`head_kind` remains outside the 164-byte signed statement, so modules using different finality
tiers must use disjoint `attestors.public_keys`/KMS keys. Nothing enforces this: a config declares
one source, and the relayer compares no keys across `head_kind` values, config files or processes,
so a key shared between tiers is not reported (see
[docs/L2_CLIENTS.md](L2_CLIENTS.md#signed-update-contract)). The attestor does reject a
verification request whose `run_mode` differs from its configured head.
`packages/l2-op-stack/config/op-sepolia.json`,
`packages/l2-op-stack/config/base-sepolia.json` and
`packages/l2-arbitrum/config/arbitrum-sepolia.json` are working public-network
templates.

### Sending a test packet

The source client is the Cosmos client that tracks the **L2** (the OP/Arbitrum client, e.g.
`08-wasm-1`), not the Ethereum client. It needs a registered counterparty, and that counterparty
must equal the client id the SpectreClient was added under on the L2 router — the relayer only
picks up packets whose `destination_client` matches its configured `ics26_client_id`.

`create-clients-cosmos` registers it for you when the l2-config carries
`counterparty_client_id`, which is the normal path; the command below is for the case where it
does not, or where you need to repoint one by hand.

```bash
. ./scripts/local/gaiad_binary.sh

"$GAIAD" tx ibc client add-counterparty <l2-client-on-cosmos> <ics26_client_id> "" \
  --from test1 --home "$HOME/.gaia" --chain-id test-ibc-eth \
  --keyring-backend test --gas-prices 1stake --gas 300000 -y

ABS_TIMEOUT=$(($(date +%s) + 2000))
"$GAIAD" tx ibc-transfer transfer transfer <l2-client-on-cosmos> <evm-receiver> 1000stake \
  --from test1 --home "$HOME/.gaia" --chain-id test-ibc-eth \
  --node tcp://127.0.0.1:26657 --keyring-backend test --gas-prices 1stake \
  --absolute-timeouts --packet-timeout-timestamp "$ABS_TIMEOUT" --generate-only \
| jq '.body.messages[0].encoding = "application/x-solidity-abi"' \
| "$GAIAD" tx sign /dev/stdin --from test1 --home "$HOME/.gaia" \
    --chain-id test-ibc-eth --keyring-backend test \
| "$GAIAD" tx broadcast /dev/stdin --node tcp://127.0.0.1:26657 -y
```

`create-clients-cosmos` writes the L2 client id into a `cosmos_to_l2` module's
`cosmos_wasm_client_id` when the l2-config carries `counterparty_client_id`; that value is what
`create-clients-eth` registers as the SpectreClient's counterparty. If it holds the *Ethereum*
client id instead, `recvPacket` reverts with a counterparty mismatch (the revert data carries the
expected and actual client ids), so set `counterparty_client_id` in the l2-config — or fix
`cosmos_wasm_client_id` by hand before `create-clients-eth`.

Two more settings decide whether the **return** direction (acks, and packets sent from the L2)
works at all, and both fail silently — nothing errors, the direction just never moves:

- `l2_ics26_client_id` on the `l2_to_cosmos` module must be the client id the SpectreClient was
  added under on the L2 router. The L2 subscriber filters events by it, so a stale placeholder
  silently drops every `WriteAcknowledgement` the L2 emits.
- Leave the attestor's `disable_derived_roots` at its default (`false`). The header builder proves
  no settlement object any more, so it accepts any attested root; turning derived roots off leaves
  the frontier waiting on games, which are posted long after the L2 block they commit, and the
  return direction simply idles for hours.

#### Return-direction latency is set by the attestor, not the prover

The forward leg costs a Groth16 proof; the return leg costs a wait. The wait is
`DERIVED_GAP_BLOCKS`: the attestor publishes one derived root per gap, and
`RelayableHeight` follows that frontier, so a packet written just after an attestation
waits nearly a full gap. At the default 150 blocks that is about 5 minutes on a 2 s chain
(150 × 2 s).

All three local devnet handoffs (`run_optimism_node.sh`, `run_base_node.sh`,
`run_arbitrum_node.sh`) export `DERIVED_GAP_BLOCKS=5`, so a devnet return leg is
seconds.
**An attestor attached to an external replica inherits none of this** — it takes the
script default of 150. Pass the knob explicitly for an E2E:

```bash
DERIVED_GAP_BLOCKS=10 OP_NODE_RPC_URL=... ./scripts/local/run_op_attestor.sh
```

`run_arbitrum_attestor.sh` takes the same variable under the same name.

The forward leg has the same shape on every stack: one Groth16 proof, then
`updateApplicationState` + `recvPacket` on the L2. What differs between stacks is the
return leg's wait, which is the gap times the chain's block time. This runbook stores no
timings or gas figures; measure your own. The relayer logs each proof job's duration
(`[prover] … done in <duration>`, always on, in `proverd`'s log) and, with `--benchmark`, the gas of every
multicall inner call (see [Benchmark mode](../README.md#benchmark-mode)). Quote any
figure together with the hardware and the number of runs behind it.

Lower is not free: each derived root is an attestation, and at `unsafe`/`safe` heads
they stay provisional until the finalized head catches up, so the provisional backlog
grows as the gap shrinks.

While a packet waits, the relayer logs one `waiting: N packet(s) not yet relayable` line
per batch period — many identical lines across a default-gap wait. That is expected, not
a stall; `source relayable height` climbing is the thing to watch.

#### Sending back (L2 → Cosmos)

The forward transfer above mints a wrapped token on the L2. Sending it back is a
contract call, not a CLI command — the relayer picks the packet up from the router
event either way. Resolve the wrapper first: the denom is the full trace, so it
carries the client the token arrived on, not just `stake`.

```bash
L2=<l2-rpc>
WRAPPED=$(cast call <ICS20Transfer> "ibcERC20Contract(string)(address)" \
  "transfer/<l2-router-client-id>/stake" --rpc-url $L2)

cast send "$WRAPPED" "approve(address,uint256)" <ICS20Transfer> 1000 \
  --private-key $ETH_PRIVATE_KEY --rpc-url $L2

ABS_TIMEOUT=$(($(date +%s) + 2000))
cast send <ICS20Transfer> \
  "sendTransfer((address,uint256,string,string,string,uint64,string))" \
  "($WRAPPED,1000,<cosmos1-receiver>,<l2-router-client-id>,transfer,$ABS_TIMEOUT,\"\")" \
  --private-key $ETH_PRIVATE_KEY --rpc-url $L2
```

The tuple is `SendTransferMsg` in field order (`contracts/apps/ics20/messages/IICS20TransferMsgs.sol`):
denom, amount, receiver, sourceClient, destPort, timeoutTimestamp, memo.

`<l2-router-client-id>` is `sourceClient`: the client id **on the L2 router** — the one
the SpectreClient was added under, which is the `ics26_client_id` in the `cosmos_to_l2`
module (`arb-client-0`, `op-client-0`, `base-client-0`). It is **not** the Cosmos-side
`08-wasm-N`; that id names the L2 client living on Cosmos, and the L2 router has never
heard of it.

**The empty memo needs explicit quotes.** Writing the tuple's last field as a bare
trailing comma — `…,$ABS_TIMEOUT,)` — makes `cast` fail with `parser error` pointing at
the end of the tuple, which reads like the client id is wrong when it is not. Use
`\"\"`. Check the encoding without sending anything:

```bash
cast calldata "sendTransfer((address,uint256,string,string,string,uint64,string))" \
  "(<wrapped>,1000,<cosmos1-receiver>,<l2-router-client-id>,transfer,1786300000,\"\")"
```

`timeoutTimestamp` is in **seconds** — the same unit trap `--absolute-timeouts` exists
for on the Cosmos side.

The leg is done when the ack is relayed back and the packet leaves the pending
tracker (`[CosmosTimeoutScan] Checking 0 pending packets`) — a forward-only success
proves half the system.

### Automated timeout recovery test

`relayer/cmd/l2_timeout_devnet_test.go` (build tag `l2devnet`) drives the Cosmos → L2
timeout lifecycle against a devnet that is already up through step 5, on either OP or
Base; the config file decides which. It starts the relayer until a Cosmos cursor is
durable, stops it, sends a 777-unit transfer with a 45 s absolute timeout, waits for
the **L2** head timestamp to pass it, then restarts the relayer on the same config, so
the same `.spectre-state` directory. It passes when:

- the sender's balance is refunded by exactly the transfer amount (the send fee stays spent);
- the L2 router holds no receipt, and a simulated late `recvPacket` reverts with
  `IBCInvalidTimeoutTimestamp`;
- the packet leaves both the relayer's live `/status` pending set and the persisted Cosmos
  pending tracker.

The config must set `status_address` (e.g. `"127.0.0.1:7071"`): the Cosmos commitment
disappears when the `MsgTimeout` lands, a moment before the relayer retires its tracker
entry, so only the live pending set says when the timeout has fully settled. The test also
refuses a state directory that still owes a timeout on the client (a tracked packet whose
Cosmos commitment still exists), since refunding it in the same run would skew the balance
check; let the relayer settle it first.

```bash
just test-l2-timeout relayer/config.json
```

The sender defaults to the devnet's `test2` key. Override it with `L2_TIMEOUT_SENDER_KEY`
(hex), and never use the relayer's `COSMOS_PRIVATE_KEY`: the relayer pays the `MsgTimeout`
fee from that account, which would hide the refund. Stop any relayer already running on
the config first, or the state-directory lock refuses the test's relayer.
`L2_TIMEOUT_LOG_DIR` keeps the relayer logs, and `RELAYER_BINARY` runs a prebuilt binary
instead of building one; `L2_TIMEOUT_WINDOW` and `L2_TIMEOUT_SETTLE` tune the waits.

### Failure modes

| Symptom | Cause |
|---|---|
| `beacon api unavailable at ...:59717` | The `eth-to-cosmos` module still has the example's beacon URL. Point `eth_beacon_api_url` at this run's `ETH_BEACON_API` (from `attestor.env`). |
| `MsgStoreCode` fails: `reference-types not enabled` | The wasm was built with a plain `cargo build`. Build through `cosmwasm/optimizer` (`just build-cw-ics08-wasm-*`); a raw release build embeds features the CosmWasm VM rejects. |
| Optimizer fails: `rustc 1.86.0 is not supported ... requires rustc 1.90` | A dependency raised its MSRV above the optimizer image's Rust. Pin the dependency down (e.g. `cargo update -p ruint --precise 1.17.0`) or bump the optimizer image. |
| `MsgCreateClient`: `status Unknown: client state is not active` | 08-wasm Stargate allowlist is missing `ClientStatus` — see [Gaia binary](#gaia-binary). |
| L2 client update panics the tx: `returning attributes from a contract is not allowed` | The deployed wasm predates the fix that made the client return data only. Rebuild through `cosmwasm/optimizer` and gov-store it. |
| L2 client update fails: `unsigned L2 headers are unsupported` | The producer is stale or misconfigured: the authenticated client requires the relayer to collect the configured threshold of indexed attestor signatures before submitting any router proof. Confirm the relayer and wasm are from the same stack and that `attestor_endpoints`, indices and key set match the fresh authenticated client; see [docs/L2_CLIENTS.md](L2_CLIENTS.md#signed-update-contract). |
| `only N valid attestor signatures` or `only N attestor frontiers available` | Fewer than the configured threshold of endpoints answered with a valid signature/frontier. The relayer queries endpoints concurrently, so one failed endpoint is tolerated whenever the remaining endpoints still satisfy the threshold; check every endpoint address, immutable index and daemon health. |
| `updateApplicationState` reverts, ~82k gas, no revert string | The relayer's signer lacks the ICS26Router relayer role on the L2. `cast run <tx>` shows `canCall(...) → false`; funding the address does not help. |
| `updateApplicationState` / `updateConsensusState` reverts with `ProofVerificationFailed()` on every attempt; packet re-queued forever | The prover's `bin/n4/pk.bin` is not from the same setup as the `Groth16Verifier_N4` registered on that chain (typically `pk.bin` was lost and a new set was generated, or the contracts predate a regeneration). Run `./scripts/local/check_prover_pairing.sh --rpc <evm-rpc>`; fix per [Prover key and verifier must be a pair](#prover-key-and-verifier-must-be-a-pair). |
| Send fails: `timeout exceeds the maximum expected value` | Sent without `--absolute-timeouts`, so the CLI writes `timeout_timestamp` in nanoseconds while IBC v2 reads seconds. |
| `eth_getProof failed: historical state ... is not available` | The Ethereum light client on Cosmos is behind, so the execution node has pruned the state it trusts. Stop the relayer that uses this Cosmos signer, run `./relayer update-eth-client --config config.json`, then restart it. The command prints the resulting `latest_slot` and `latest_execution_block_number`; if it says the client is already current, the stale proof has another cause. |
| Ack fails: `unknown field account_proof, expected one of key, value, proof` | The L2 membership proof was built in the Ethereum L1 shape. The L2 client wants a bare `EvmStorageProof` — see [docs/L2_CLIENTS.md](L2_CLIENTS.md). |
| `packet at height N not yet covered by the destination client (trusts M)` | **L2 → Cosmos:** the attestor frontier has not reached the packet's block yet, so `RelayableHeight` is still below it. It normally clears on the next attestation; if it persists, check the attestor's log. **Ethereum → Cosmos:** the Ethereum beacon light client on Cosmos may be lagging. If it stays behind, stop the relayer that uses this Cosmos signer, run `./relayer update-eth-client --config config.json`, check the reported `latest_slot` and `latest_execution_block_number`, then restart the relayer. If the client is already current, check whether the packet's block is finalized yet. |
| ETH client stops advancing: `404 NOT_FOUND: Sync committee for period N not found` | The beacon does not serve a `light_client/bootstrap` for that period. The relayer takes the committee from the preceding period's update instead, so this should only appear if that update is also unavailable — check the endpoint serves `/eth/v1/beacon/light_client/updates`. |
| `the attestor does not recognise L2 block N ... as canonical` | The relayer's L2 RPC and the attestor's replica disagree at that height — a reorg past the frontier, or the two pointed at different chains. Not retried away: confirm both use the same L2. |
| A direction goes silent — no error, no retry, other directions healthy | A hung RPC. `kill -QUIT <relayer-pid>` dumps every goroutine; look for one blocked in `net/http.(*persistConn).roundTrip`. Note the dump kills the process. |
| Gov proposal ends `REJECTED` without votes | `wasm.sh` resolves the proposal id after a fixed `sleep`; if indexing is slower the id is empty and the vote step is skipped. Vote manually before the (short devnet) voting period ends. |
| `eth_getProof`: `distance to target block exceeds maximum proof window` | The L2 execution node will not prove below its head. The relayer proves the router account at the *attested* height, which always trails. On local Base, this usually means the relayer was pointed at the sequencer RPC instead of the follower RPC. Otherwise widen the node's proof window (`--rpc.eth-proof-window`); on someone else's node, ask — no relayer setting works around it. |
| Attestor exits: `set DISPUTE_GAME_FACTORY and RESPECTED_GAME_TYPE for network <x>` | The script resolves those from a known chain preset and cannot for a public network. Read them off the chain — see [Running against an existing L2](#running-against-an-existing-l2-no-devnet-l2). |
| `[gaiad_binary] ERROR: ... is on branch unknown; expected test/ibc-host-customs` | The Gaia checkout is on a detached HEAD, which fails the branch check even when the code is right. Set `GAIAD=` to an already-built binary — see [Gaia binary](#gaia-binary). |
| Attestor exits: `failed to listen on attestor grpc address 127.0.0.1:3001: address already in use` | An earlier attestor still holds the port. Find it with `lsof -nP -iTCP:3001 -sTCP:LISTEN` — plain `lsof -ti :3001` also matches the relayer *connected* to that port, and killing that list takes the relayer down with it. |
| `forge script` fails `insufficient funds ... have 0` | The deployer has no balance on the L2 — use an L2-funded account (step 4). Balances do not carry between rollups: funded on L1 Sepolia, OP or Arbitrum still means zero on Base. |
| `eth_getProof` returns nothing at all — no result, no error, the call just hangs | The node prunes state below some depth and does not say so. Measure the boundary (see the external-L2 sections) and keep the attested height inside it; `DERIVED_GAP_BLOCKS=10` trails ~10–20 blocks, `head_kind=finalized` trails ~600. |
| Arbitrum attestor stops attesting: last log line is a normal attestation, container still up | The runtime back-fill is bounded (`runtime_backfill_max_blocks`, default 2048) and parallel (`runtime_backfill_concurrency`, default 8); an omitted or zero value uses the default. If the concurrency is too low for the endpoint's latency the attestor falls behind the chain, and skipped heights are logged as `backfill capped`. Size both as described in `attestor/arbitrum/README.md` before filing an issue. |
| `start` refuses to boot: `l2_chain_id is N but l2_rpc_url ... serves chain M` | The profile and the RPC name different chains. Transactions take their chain id from `eth_chainId` on the RPC and nothing else reads `l2_chain_id`, so without this check the relayer would relay against **M** while the config, the client's committed profile and the docs all said **N**. Fix whichever is wrong; the error prints both. |
| `start` refuses to boot: `head_kind is "unsafe" but attestor N at ... gates "opstack" on attestation_head "finalized"` | `head_kind` and the attestor's `ATTESTATION_HEAD` disagree. The builder sends `head_kind` as the `run_mode` and the attestor refuses any `run_mode` but its own, so every header would be rejected at the first packet. Make them equal; the error names both. The sibling `serves [...], not attestor_src_chain` means the endpoint is an attestor for a different L2. An attestor that does not answer `Info` is skipped with a log line, not refused. |
| Arbitrum attestor attests the wrong chain (`src_chain=arbdev`, `l2_chain_id=412346`) despite `CHAIN_PROFILE=arbitrum-sepolia` | A leftover `.arbitrum-devnet-run/attestor.env` was sourced; its exports are real env vars and beat the profile. A non-`devnet` `CHAIN_PROFILE` makes the script ignore the handoff and log that it did; if you do not see that log line, delete the directory and check you are on a current checkout. |
| `eth_getLogs` rejected: `you can make eth_getLogs requests with up to a 10 block range` | The provider caps the log span. Set `log_scan_chunk` in the `l2_to_cosmos` module to that cap (Alchemy free tier 10, drpc 10 000). |
| `[... Flush] provider has pruned history below block N` | The node dropped older logs; lookups start at N. Older packets need an archive provider — see [`router_deploy_block`](#router_deploy_block-where-packet-flush-log-lookups-start). |
| `[... Flush] seq=N: no log in the newest M blocks ... giving up` | The event is older than the 64-window walk — with `log_scan_chunk: 10` that is only 640 blocks, under 3 minutes on Arbitrum. A capped provider cannot reach it, even with `router_deploy_block` set; use one without a range cap (then `router_deploy_block` covers the whole history). See [`router_deploy_block`](#router_deploy_block-where-packet-flush-log-lookups-start). |
| Return direction sees no events at all, forward direction fine | `l2_ics26_client_id` does not equal the forward module's `ics26_client_id`. The L2 subscriber filters events on it, so a mismatch drops every one silently. |

## Local Cosmos ↔ Base E2E

Same shape as the OP flow, with three differences that will bite if you copy the OP
steps and rename the scripts.

**Base is not vanilla OP Stack.** `base/base` ships one unified Rust node (execution
and consensus in a single process) plus its own batcher, so `run_base_node.sh` runs
base's own images from a pinned clone rather than optimism-package. It still speaks
the op-node RPC namespace, which is why the attestor is shared.

**Base gets its own enclave by default** (`base-devnet`), so a Base run cannot
disturb an OP or Arbitrum devnet. Pass `ENCLAVE=op-devnet` to settle it on the same
L1 as those instead.

**Base and Optimism are the same chain type to the relayer.** `src_chain` /
`dst_chain` stay `opstack` — those are the only four chain kinds (`relayer/chain/chain.go:33-36`),
enforced when a module is parsed (`relayer/cmd/config.go:140`). The two are told
apart by module **name** and client id, which is why
the deploy step below needs `MODULE_NAME`.

```bash
# 1. L1 (Fulu) + Base L2. Brings the L1 up via run_eth_node.sh if the enclave does
#    not exist. Ends with .base-devnet-run/attestor.env written.
./scripts/local/run_base_node.sh

# 2. Attestor. run_base_attestor.sh is a symlink to run_op_attestor.sh; it picks
#    .base-devnet-run/attestor.env from its own name, so do NOT call
#    run_op_attestor.sh here — that one attaches to the OP devnet.
GRPC_PORT=3003 ./scripts/local/run_base_attestor.sh

# 3. Cosmos node, then gov-store the Base light-client wasm. The Ethereum client
#    is NOT needed for a Cosmos<->Base deployment — like OP and Arbitrum, the
#    attestor-trusted L2 client authenticates nothing through it. Add
#    ./scripts/local/wasm.sh only if this config also relays Cosmos<->Ethereum.
./scripts/local/run_cosmos_node.sh
./scripts/local/wasm_base.sh     # -> Base client checksum

# 4. Deploy the L2 IBC contracts onto Base. MODULE_NAME is required: the module's
#    dst_chain is "opstack" (see above), so matching on it alone would also match
#    the Optimism module. DST_CHAIN only picks which devnet handoff to read. The
#    deploy txs go to Base's sequencer RPC, while the relayer config is patched to
#    Base's follower RPC because L2->Cosmos membership proofs need eth_getProof
#    below the head.
L2_ENV_FILE=.base-devnet-run/attestor.env MODULE_NAME=cosmos-to-base \
DST_CHAIN=base ./scripts/local/deploy_l2_contracts.sh

# 5. Create the clients. Copy relayer/base-l2-config.example.json and fill in the
#    checksum from step 3, the L2 router from step 4, and the L2 chain id. There is
#    only ONE checksum: the file carries no ethereum_client, because the client
#    verifies nothing against L1.
cd relayer && go build -o relayer ./cmd
./relayer create-clients-cosmos --config config.json --l2-config base-l2-config.json
./relayer create-clients-eth --config config.json \
    --trust-level 2/3

# 6. Relay.
./relayer start --config config.json
```

Everything in the "Local Cosmos ↔ OP E2E" section about the ICS26Router relayer
role and `--absolute-timeouts` on the test transfer applies unchanged — Base uses
the same attestor and the same L2 contracts.

### Running against an existing Base L2 (no devnet L2)

Skip step 1 and point the attestor at the node someone else runs. The predecessor of this flow
was verified against Base Sepolia. The authenticated flow below includes every required signing
input, but must be revalidated live on the current stack before it can be described as verified
end to end.

```bash
# 1. Attestor against the existing node. NETWORK must name the real chain, and the
#    dispute-game contracts cannot be auto-resolved for a public network — read
#    them off the chain once (below) and pass them.
OP_NODE_RPC_URL=http://<host>:7545 \
L1_RPC_URL=https://ethereum-sepolia-rpc.publicnode.com \
NETWORK=base-sepolia SRC_CHAIN=base-sepolia \
L2_CHAIN_ID=84532 \
ATTESTOR_SIGNING_KEY=<32-byte-ed25519-seed-hex> \
ATTESTOR_RUN_DIR=.base-attestor-run \
ATTESTATION_HEAD=unsafe DERIVED_GAP_BLOCKS=10 \
DISPUTE_GAME_FACTORY=0xd6E6dBf4F7EA0ac412fD8b65ED297e64BB7a06E1 \
RESPECTED_GAME_TYPE=621 \
GRPC_PORT=3003 \
  ./scripts/local/run_base_attestor.sh

# 2. Cosmos + the Base wasm, unchanged from the devnet flow.
GAIAD=~/go/bin/gaiad ./scripts/local/run_cosmos_node.sh
GAIAD=~/go/bin/gaiad ./scripts/local/wasm_base.sh

# 3. config.json: one runnable config per path, already cut to the Base pair.
cp relayer/config.base.example.json relayer/config.json
# then fill in the fields listed in "Values you fill in by hand" below.

# 4. Deploy the L2 contracts with a key funded ON Base Sepolia. MODULE_NAME is
#    still required — dst_chain is "opstack" and would also match cosmos-to-op.
#    DO THIS BEFORE STARTING THE ATTESTOR if both share one rate-limited RPC —
#    see "One RPC budget, two processes" in the Arbitrum section.
DST_CHAIN=opstack MODULE_NAME=cosmos-to-base \
L2_RPC=http://<host>:8545 RELAYER_CONFIG=relayer/config.json \
L2_DEPLOYER_ADDRESS=0x... L2_DEPLOYER_PRIVATE_KEY=... \
  ./scripts/local/deploy_l2_contracts.sh

# 5. l2-config, then clients. The relayer must sign with the SAME key that
#    deployed (E2ETestDeployL2 grants the router role to msg.sender). Pass it in
#    the environment rather than editing relayer/.env — godotenv does not override
#    a variable already set, so the export wins.
cp relayer/base-l2-config.example.json relayer/base-l2-config.json
# fill in wasm_checksum (step 2), l2_rpc_url, l2_router (= ICS26_ADDRESS from step 4),
# and the attestor public key as described below
cd relayer
./relayer create-clients-cosmos --config config.json --l2-config base-l2-config.json
ETH_PRIVATE_KEY=<deployer-key> ./relayer create-clients-eth --config config.json \
  --trust-level 2/3
ETH_PRIVATE_KEY=<deployer-key> ./relayer start --config config.json
```

#### Values you fill in by hand, and where each comes from

Everything not listed here is written for you — see
[What the tooling fills in for you](#what-the-tooling-fills-in-for-you).

Base uses the OP Stack attestor, which logs its public key twice at startup: hex for human
comparison and standard base64 for the relayer. Copy the base64 value from
`.base-attestor-run/attestor.log` into both locations below before creating the client. They must
contain the public half of the exact `ATTESTOR_SIGNING_KEY` supplied in step 1; leaving the
checked-in disposable devnet key in either file lets the daemon start but makes every certificate
fail against the client or relayer configuration.

**`relayer/config.json`:**

| Field | Module | Value comes from |
|---|---|---|
| `eth_rpc_url` | `cosmos-to-base` | your L2 HTTP endpoint |
| `eth_ws_url` | `cosmos-to-base` | your L2 WebSocket endpoint; optional, the relayer skips the WS client when empty |
| `l2_rpc_url` | `base-to-cosmos` | the same L2 HTTP endpoint (step 4 overwrites it with `L2_RPC` anyway) |
| `attestor_endpoints[0].address` | `base-to-cosmos` | `127.0.0.1:$GRPC_PORT` from step 1. The example ships `127.0.0.1:3003`, matching the `GRPC_PORT=3003` above — Base needs its own port so it can coexist with an OP attestor on 3001. The list takes one `{address, attestor_index}` entry per attestor daemon, and the config is rejected when it has fewer entries than `attestors.threshold` |
| `attestor_endpoints[0].attestor_index` | `base-to-cosmos` | the position of that attestor's key in `attestors.public_keys` (`0` for the single local attestor). Each index may appear once and must lie inside the key list |
| `attestor_src_chain` | `base-to-cosmos` | the `SRC_CHAIN` you passed in step 1 (`base-sepolia`) |
| `head_kind` | `base-to-cosmos` | the same choice as `ATTESTATION_HEAD` in step 1. `start` asks each attestor's `Info` for the head it gates on and refuses to boot when the two differ — the attestor signs only its configured head, so a mismatch relays nothing |
| `rollup_profile.common.l2_chain_id` | `base-to-cosmos` | `eth_chainId` on your L2 (`84532` on Base Sepolia). `start` verifies this against the RPC and refuses to boot on a mismatch |
| `attestors.public_keys` | `base-to-cosmos` | the base64 public key logged by the attestor started with `ATTESTOR_SIGNING_KEY` |
| `log_scan_chunk` | `base-to-cosmos` | your provider's `eth_getLogs` span cap — measure it, see the Arbitrum section |
| `router_deploy_block` | `base-to-cosmos` | optional — block the `l2_router` proxy was deployed at; see [above](#router_deploy_block-where-packet-flush-log-lookups-start) |

**`relayer/base-l2-config.json`:** `wasm_checksum` (from `wasm_base.sh`), `l2_rpc_url`,
and `rollup_profile.common.l2_router` (= `ICS26_ADDRESS` printed by step 4), plus the same
base64 key in `attestors.public_keys`.

**Fund the deployer on Base itself.** An account funded on L1 Sepolia, OP Sepolia or
Arbitrum Sepolia has nothing here — balances do not carry across rollups. The deploy
fails at the first transaction with `insufficient funds for gas * price + value: have
0`, after the script has already written `broadcast/`. Check first:
`cast balance <addr> --rpc-url $L2 --ether`.

**Resolving the dispute-game contracts.** The values above are Base Sepolia's, read
from the chain rather than hardcoded anywhere — the portal address comes from the
node itself:

```bash
PORTAL=$(curl -s -X POST -H 'content-type: application/json' \
  --data '{"jsonrpc":"2.0","id":1,"method":"optimism_rollupConfig","params":[]}' \
  http://<host>:7545 | jq -r .result.deposit_contract_address)
cast call $PORTAL "disputeGameFactory()(address)" --rpc-url $L1_RPC_URL
cast call $PORTAL "respectedGameType()(uint32)"  --rpc-url $L1_RPC_URL
```

**Check how far below the head the node will serve `eth_getProof`.** The relayer
proves the router account at the attested height, which trails the head. A node that
prunes state does not error — it can simply stop responding, which looks like a hung
relayer rather than a configuration problem. One measured node served head-500 in
about a second and returned nothing at all at head-800 after 30 s. With
`DERIVED_GAP_BLOCKS=10` the attested height trails by ~10–20 blocks, comfortably
inside that; `head_kind=finalized` trails by ~600 and would not be.

```bash
H=$(cast block-number --rpc-url $L2)
cast rpc eth_getProof '["<router>",[],"'$(printf '0x%x' $((H-500)))'"]' --rpc-url $L2
```

Useful:

```bash
./scripts/local/run_base_node.sh --status   # are the Base services up
./scripts/local/run_base_node.sh --reset    # redeploy Base only; L1 preserved
./scripts/local/run_base_node.sh --stop     # stop Base, leave the L1 running
kurtosis enclave rm -f base-devnet          # remove the L1 and everything on it
```
