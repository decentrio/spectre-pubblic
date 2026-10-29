# External-L1 Arbitrum Kurtosis package

This package deploys an Arbitrum Nitro BoLD development chain into an existing
Kurtosis enclave. It intentionally does not deploy Ethereum.

Use the repository wrapper rather than invoking the package directly:

```sh
# Arbitrum only — the wrapper brings the shared L1 up itself:
./scripts/local/run_arbitrum_node.sh

# …or share one L1 with OP by starting that stack first:
./scripts/local/run_optimism_node.sh
./scripts/local/run_arbitrum_node.sh
```

The package performs three operations:

1. Funds the standard local Nitro owner, sequencer, and validator accounts
   from the ethereum-package development account.
2. Builds RollupCreator from the pinned `nitro-contracts` ref and deploys
   RollupCore to the supplied L1.
3. Starts a simple Nitro node that sequences, posts batches, and creates BoLD
   assertions.

After the package starts, the wrapper deposits ETH through the generated Inbox
and funds the standard L2 development account used by
`scripts/local/deploy_l2_contracts.sh`.

The wrapper pins the BoLD `_assertions` mapping slot to `0x75`, matching the
committed storage layout in `nitro-contracts` v3.1.0, and verifies the slot
against a finalized local assertion before writing the attestor handoff.

It stores these Kurtosis file artifacts:

| Artifact | Contents |
| --- | --- |
| `arb-deployment-info` | Raw RollupCreator deployment metadata |
| `arb-chain-info` | Nitro chain information containing RollupCore addresses |
| `arb-sequencer-config` | Self-contained Nitro configuration with embedded chain info |

The sequencer is not the verifier used by the attestor. The separate
`run_arbitrum_attestor.sh` launcher starts its own non-sequencing Nitro replica
with an independent persistent database.

All configured keys are public development keys. Never use this package
against a public or valuable L1.
