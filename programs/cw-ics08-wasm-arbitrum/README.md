# Arbitrum authenticated L2 client

This ICS-08 Wasm client accepts only `arbitrum_attestor_v1` headers carrying an exact-threshold,
strictly indexed Ed25519 certificate over the canonical L2 chain/router/set/block statement.
It authenticates every certificate before traversing the router account proof; query and sudo use
the same verifier. Unsigned messages fail with `UnsupportedUnsignedHeader`.

The active, sorted public-key set and threshold live in `ClientState.attestors`. Its governance-only
migration entry point can keep or rotate that set on an existing authenticated client. It rejects
missing-attestor state without writes; create a fresh authenticated client for legacy state.

This validates the Wasm artifact only. Deployment also requires a compatible signed message
producer plus custody, compromise-response, availability, and governance review.
