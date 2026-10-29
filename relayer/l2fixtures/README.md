# L2 fixture capture

`capture-l2-fixture` records a deterministic L1/L2 proof bundle for a single
reviewed rollup configuration. It always queries numeric block tags, preserves the
full `eth_getProof` responses, and writes an adjacent SHA-256 provenance
manifest. It never records the RPC URLs used during capture.

The local JSON configuration must supply `config_id`,
`ethereum_source_revision`, `rollup_source_revision`, and `profile_sha256`
(the lowercase SHA-256 of the exact runtime-profile JSON), plus L1 and L2 RPC
URLs, a nonzero finalized L1 block number and beacon slot, a nonzero L2 block
number, and a list of uniquely named proof requests. Each request has `role`,
`chain` (`l1` or `l2`), an address, and optional 32-byte storage keys.

Run it only with archive-capable endpoints:

```text
just capture-l2-fixture /secure/base-capture.json packages/l2-op-stack/fixtures/base-sepolia
just capture-l2-fixture /secure/op-capture.json packages/l2-op-stack/fixtures/op-sepolia
```

The command creates `fixture.json` and a schema-v2 `provenance.json`
(`relayer/l2fixtures/capture.go`). Review the pinned source revisions, profile
digest, contract identities, block hashes, layouts, finality evidence, and
every proof role before committing them. A fixture is not a supported
configuration until the Rust verifier accepts it in offline tests. *(The
current `l2-client` crate has no `load_fixture` entry point that consumes
these files directly — unverified how the Rust side loads them; check
`packages/l2-client/src/` before relying on that path.)*

The generic capturer does not infer rollup finality. Arbitrum capture
configuration must explicitly request RollupCore/assertion proof roles; Base
and OP configurations must explicitly request factory, portal, game, and L2
IBC-account roles. This keeps contract-layout decisions in reviewed verifier
configuration instead of in relayer input.
