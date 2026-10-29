# External-L1 Base Kurtosis package

This package deploys a Base development chain into an existing Kurtosis enclave.
It intentionally does not deploy Ethereum.

Use the repository wrapper rather than invoking the package directly:

```sh
# Base in its own enclave (base-devnet), with its own L1 — the default:
./scripts/local/run_base_node.sh

# …or settle Base to an L1 shared with OP/Arbitrum by naming their enclave:
./scripts/local/run_optimism_node.sh
ENCLAVE=op-devnet ./scripts/local/run_base_node.sh
```

Base does not run vanilla OP Stack binaries: the [base/base](https://github.com/base/base)
monorepo ships one unified Rust node (`base`, execution + consensus in a single
process, role selected by the `sequencer` / `rpc` / `bootnode` subcommand) plus
its own batcher. The package reproduces that repo's docker-compose devnet
topology, minus the ZK prover pipeline and observability stack:

| Service | Role |
| --- | --- |
| `base-bootnode` | EL discv4 + CL discovery bootstrap; publishes the consensus ENR |
| `base-builder` | sequencer (execution + consensus), flashblocks producer |
| `base-batcher` | posts L2 batches to the shared L1 |
| `base-client` | independent follower — what the attestor and the L2 IBC contracts point at |

The package performs three operations:

1. Runs base/base's own `setup-l2.sh` (`devnet-setup:local`) against the supplied
   L1: `op-deployer apply --deployment-target live` deploys the standard OP Stack
   L1 contracts, then `op-deployer inspect` emits the L2 genesis, rollup config
   and L1 addresses. The same task derives the L1 chain config the Base node
   wants (`--l1-config-file`, the bare `.config` object) from the
   ethereum-package genesis artifact, and mints the node's JWT.
2. Starts the bootnode and stores its consensus ENR as a file artifact.
3. Starts the sequencer, batcher and follower, wired to each other and to the
   shared L1.

Funding the Base role accounts on the L1 is the wrapper's job — the setup image
carries `op-deployer`, not `cast`.

**Where the images come from.** The package only names them (`base:local`,
`base-batcher:local`, `devnet-setup:local`); Kurtosis resolves each from the
local Docker daemon, because `image_download` defaults to `missing`. The wrapper
builds any that are absent from a pinned clone of base/base (`BASE_REF`) under
`.base-devnet-run/base`, patched for the WSL2 vendored-OpenSSL build failure —
the same clone+patch recipe `run_eth_node.sh` and `run_optimism_node.sh` use for
their packages. Nothing depends on a sibling `../base` checkout; set `BASE_REPO`
to build from one anyway, or `REBUILD_IMAGES=1` to force a rebuild.

It stores these Kurtosis file artifacts:

| Artifact | Contents |
| --- | --- |
| `base-devnet-configs` | `jwt.hex`, `el/chain-config.json`, `l2/{genesis,rollup,l1-addresses}.json`, p2p keys |
| `base-cl-bootnode-enr` | the bootnode's consensus ENR |

Both names take `artifact_suffix`. Kurtosis cannot overwrite or delete a stored
artifact, so a retry or a `--reset` in the same enclave would otherwise collide
with the previous run's names; the wrapper claims the next free suffix and
downloads the matching one.

Base fork activations (`azul_block` / `beryl_block` / `cobalt_block`, defaults
20/21/22 from base/base's own devnet, `isthmus_block` unset because op-deployer
already enables it) are passed to `setup-l2.sh`, which converts each height to a
timestamp and patches both `genesis.json` and `rollup.json`. Set one to `""` to
leave that fork off. The compose devnet additionally deploys a
`MockProtocolVersions` contract for *runtime* upgrade signalling; that is
optional (the node only demands `--upgrade-signal.l1-rpc` if a contract is
configured — `crates/execution/cli/src/standard_node.rs`), so this package
activates forks statically and skips it.

Two deliberate differences from the compose devnet:

- **No dispute games.** Base replaced `op-proposer` with a ZK prover pipeline
  that this package does not run, so `DisputeGameFactory.gameCount` stays 0 and
  what gets exercised is the proposal-independent derived-root path — exactly
  what the attestor's feed consumers wait on.
- **Blob DA requires Lighthouse >= v8 on the shared L1** (`batcher_da_type`,
  default `blobs`). base-node reads batches back from the Fulu/PeerDAS endpoint
  `eth/v1/beacon/blobs/{slot}`
  (`crates/consensus/providers/src/beacon_client.rs`), which v8 added and v7
  does not serve — on v7 the follower 404s on every batch, declares blobs
  permanently unavailable and resets its derivation pipeline forever with
  `safe_l2` stuck at 0. `eth-network-params.yaml` pins that image explicitly for
  exactly this reason. Set `batcher_da_type: "calldata"` (which never touches
  the beacon API) if the L1 is ever downgraded;
  `--no-force-blobs-when-throttling` then keeps DA-backlog throttling from
  overriding the choice back to blobs.
- **The batcher waits for L2 block 1.** It chooses its start cursor once, at
  boot; with the sequencer still at genesis it polls the latest block — genesis,
  which has no transactions — and halts fatally on batch composition. Compose
  papers over this with `restart: unless-stopped`; Kurtosis has no restart
  policy, so the package gates the batcher on the sequencer having produced a
  block instead.
- **Discovery ports are published but not health-checked** (`wait = None`).
  Kurtosis dials every declared port before a service counts as started, and the
  bootnode advertises tcp/udp 9003 in its ENR while only listening on UDP — the
  default check fails a service that is in fact healthy. Readiness is asserted
  where it means something: the published ENR for the bootnode, `/healthz` for
  the nodes.
- **No static IPs.** Compose pins each node to an address in a fixed subnet;
  Kurtosis assigns container IPs dynamically, so every node resolves its own
  advertised IP at start-up and the bootnode's enode is assembled after Kurtosis
  has placed it.

`l1_slot_duration` defaults to 2 (the ethereum-package L1) rather than the 4 of
base/base's bundled L1, and `verifier_l1_confs` defaults to 4 rather than 15 so
the follower's derived head stays fresh for the attestor.

All configured keys are public development keys taken from base/base's
`etc/docker/devnet-env`. Never use this package against a public or valuable L1.
