# Arbitrum signed-header fixtures

Fixtures consist of a canonical Arbitrum execution header and the router `eth_getProof` account
branch at that block, plus an indexed exact-threshold Ed25519 certificate over the canonical
164-byte Spectre statement. Shared deterministic `1-of-1`, `2-of-3`, `32-of-32`, and negative
vectors live under `test/fixtures/wasm-contracts/`. No L1 or BoLD proof is included.

Unsigned messages are retained only as a negative wire fixture and are not accepted by the current
`arbitrum_attestor_v1` program.
