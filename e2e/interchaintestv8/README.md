# End-to-end testing suite (interchaintest)

The e2e tests are built on the [interchaintest](https://github.com/cosmos/interchaintest)
library. Each run boots a real Ethereum PoS devnet (Kurtosis + ethereum-package),
a real IBC-Go simd chain (Docker), starts the Spectre relayer binary against
them, then drives ICS-20 packet flows end-to-end.

Each test boots a full devnet from scratch and is slow; `just test-e2e` allows up to
120 minutes per invocation (`-timeout 120m` in the `justfile`). Designed for both CI
and local dev.

## Prerequisites

On top of the toolchain in the [repo root README](../../README.md#requirements):

- **Docker** daemon running
- **[Kurtosis CLI](https://docs.kurtosis.com/install/)** installed
- A checkout of spectre-prover next to this repository (`../prover`, or set `PROVER_REPO`),
  with its private gnark forks (`GOPRIVATE`) reachable and `libgaraga_rs` built there with
  `scripts/build-garaga-ffi.sh`; see [the prover](../../README.md#the-prover)
- Nothing to trim: the prover setup tool's `-buckets` flag already defaults to `"4"`
  (`cmd/setup-circuits/main.go:30` in that checkout), and the suite only ever exercises bucket 4
  (single-validator simd chain). Building the full bucket family takes far longer
  than building N=4 and is only needed if you pass `-buckets` explicitly.

`just test-e2e` first runs these recipes:

- `clean-foundry` — wipes `cache/`, `out/`, `broadcast/`
- `install-go-relayer` — `go build -o $GOPATH/bin/relayer ./cmd`
- `build-prover-artifacts` — `scripts/solidity/build-prover-artifacts.sh`
  (generates the configured buckets, N=4 by default, as `{r1cs,pk,vk}.bin` in the prover
  checkout's `bin/n{N}/` and `contracts/verifiers/Groth16Verifier_N{N}.sol` here)

When every configured artifact already exists the script exits without regenerating
anything, so later runs reuse them. `scripts/solidity/build-prover-artifacts.sh --force`
rebuilds on purpose, which changes the verifying key (see "Prover artifacts and GPU
proving" in the root README).

## Run a test

From the repo root:

```bash
# Single test by full name (SUITE/CASE)
just test-e2e TestWithEthCosmosTestSuite/Test_ICS20TransferERC20TokenfromEthereumToCosmosAndBack

# Shortcut by suite (drops the TestWith...Suite/ prefix)
just test-e2e-eth-cosmos Test_Deploy
just test-e2e-relayer Test_5_RecvPacketToEth
just test-e2e-multichain Test_Deploy
```

The recipe sets `ETH_TESTNET_TYPE=pos` and `RELAYER_BINARY=$GOPATH/bin/relayer`, and wraps
the run in `scripts/local/run_prover.sh --`, which starts `proverd` from the prover checkout,
waits until it is listening, sets `PROVER_ADDR` for the relayer processes the suite spawns,
and stops it when the run ends.

## `EthCosmosTestSuite` daemon cases

| Test | What it verifies |
|------|------------------|
| `Test_Deploy` | contracts deployed, wasm-eth client + Spectre + AddClient + counterparty register all wired |
| `Test_ICS20TransferERC20TokenfromEthereumToCosmosAndBack` | full ETH→Cosmos→ETH round-trip (5 phases: recv, ack, return-send, return-recv, return-ack) |
| `Test_25_ICS20TransferERC20TokenfromEthereumToCosmosAndBack` | batched 25-packet ETH→Cosmos→ETH round-trip using auto-relay waits |
| `Test_50_ICS20TransferERC20TokenfromEthereumToCosmosAndBack` | batched 50-packet ETH→Cosmos→ETH round-trip using auto-relay waits |
| `Test_ICS20TransferERC20TokenFromEthereumToCosmosAndBackFails` | ETH→Cosmos vouchers are restored on Cosmos after a Cosmos→ETH return transfer writes an error ack |
| `Test_ICS20TransferNativeCosmosCoinsToEthereumAndBack` | full Cosmos→ETH→Cosmos round-trip for native coin |
| `Test_ICS20TransferLargeAmountFromEthereumToCosmosAndBack` | bigint round-trip (`transferAmount = 5×10²²`, half of `StartingERC20Balance`) |
| `Test_TimeoutPacketFromEth` | short-deadline ETH→Cosmos packets settle by either auto-delivery or timeout refund with balanced cross-chain accounting |
| `Test_TimeoutPacketFromCosmos` | short-deadline Cosmos→ETH packets settle by either auto-delivery or timeout refund with balanced cross-chain accounting |
| `Test_TimeoutPacketEthRemintsVouchers` | timed-out ETH→Cosmos voucher transfer remints/restores the ETH-side voucher balance |
| `Test_TimeoutPacketCosmosRemintsVouchers` | timed-out Cosmos→ETH voucher transfer remints/restores the Cosmos-side voucher balance |
| `Test_ErrorAckToEthereum` | Cosmos transfer module writes error ack → `MsgAck` on ETH → escrow refund |

## `RelayerTestSuite` daemon cases

`TestWithRelayerTestSuite` (`relayer_test.go`) is driven by the same daemon and
waits for it instead of pulling a relay transaction. Run one case with
`just test-e2e-relayer <name>`, or dispatch the manual "E2E — Relayer" workflow.

| Test | What it verifies |
|------|------------------|
| `Test_5_RecvPacketToEth`, `Test_10_RecvPacketToEth` | 5 / 10 transfers sent on Cosmos are delivered to Ethereum by the daemon |
| `Test_5_BatchedAckPacketToEth`, `Test_10_BatchedAckPacketToEth` | 5 / 10 ERC20 transfers sent from Ethereum are delivered to Cosmos and their acks relayed back; the commitments are removed |
| `Test_10_RecvPacketToCosmos` | 10 ERC20 transfers sent from Ethereum are delivered to Cosmos by the daemon |
| `Test_10_BatchedAckPacketToCosmos` | transfers sent on Cosmos are received on Ethereum and acknowledged on Cosmos; the commitments are removed |

## Out-of-scope cases

These still assume upstream-only flows or tooling and are not part of the Spectre
daemon matrix.

- `spectre_client_test.go` (standalone Spectre client/deployment flow) —
  fixture generation remains explicitly unsupported.

## Architecture note

Spectre's relayer is an **event-driven daemon**, not the upstream Rust gRPC
service. Tests therefore *wait* for the daemon to drive each step (via
`require.Eventually` polling the post-relay end state) instead of requesting a
relay tx over gRPC and broadcasting it themselves.

`s.EthRelayerSubmitter` is still funded in `ibc_eth_cosmos_test.go` and
`multichain_test.go`, but no test uses it.
