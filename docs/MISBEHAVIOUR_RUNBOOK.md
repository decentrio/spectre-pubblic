# Cosmos Equivocation Incident Runbook

Use this procedure for proof-backed, same-height Cosmos equivocation: two valid
commits for the same chain ID and height with different block hashes. A
successful submission freezes the configured SpectreClient and stops packet and
client-update operations routed through it.

The relayer does not detect equivocation by polling one RPC endpoint. A correct
CometBFT node serves the branch it committed, so evidence must come from
administratively independent peers, an evidence service, or existing Cosmos
tooling.

## 1. Confirm the incident

Typical signals are conflicting block hashes reported by independent validators,
duplicate-vote evidence, or independent explorers disagreeing at one finalized
height. Do not freeze based on an application response, an unverified block, or
two URLs backed by the same load-balanced node.

Choose a height `H` and query two independent peers:

```bash
curl -fsS "${NODE_A}/commit?height=${H}" > commit-a.json
curl -fsS "${NODE_B}/commit?height=${H}" > commit-b.json
jq -r '.result.signed_header.commit.block_id.hash' commit-a.json commit-b.json
jq -r '.result.signed_header.header | [.chain_id, .height] | @tsv' commit-a.json commit-b.json
```

Continue only when both peers identify the expected chain and height and the
block hashes differ. Collect the validator sets from the same peers, following
pagination until all validators are captured:

```bash
curl -fsS "${NODE_A}/validators?height=${H}&page=1&per_page=100" > validators-a.json
curl -fsS "${NODE_B}/validators?height=${H}&page=1&per_page=100" > validators-b.json
```

## 2. Produce standard IBC evidence

The command accepts protobuf JSON for
`ibc.lightclients.tendermint.v1.Misbehaviour`. Export that object from the
incident evidence collector or Cosmos/IBC tooling that observed both branches.
A `/commit` response by itself is not sufficient: each header must include its
validator set, trusted height, and the validator set trusted at that height.
Preserve the values exactly as observed; do not combine a commit from one peer
with validator data from another.

The following is a schema outline, not submit-ready evidence (the nested
objects and arrays are deliberately empty):

```json
{
  "client_id": "eth-main",
  "header_1": {
    "signed_header": { "header": {}, "commit": {} },
    "validator_set": { "validators": [], "proposer": {}, "total_voting_power": "0" },
    "trusted_height": { "revision_number": "1", "revision_height": "12344" },
    "trusted_validators": { "validators": [], "proposer": {}, "total_voting_power": "0" }
  },
  "header_2": {
    "signed_header": { "header": {}, "commit": {} },
    "validator_set": { "validators": [], "proposer": {}, "total_voting_power": "0" },
    "trusted_height": { "revision_number": "1", "revision_height": "12344" },
    "trusted_validators": { "validators": [], "proposer": {}, "total_voting_power": "0" }
  }
}
```

Do not submit that outline. The actual file must contain the complete two
headers emitted by the evidence tool. Do not hand-edit hashes, signatures,
voting powers, validator order, or trusted heights. The relayer rejects
malformed commits, unequal heights, equal hashes, client or chain mismatches,
trusted-validator hash mismatches, frozen clients, and headers without more
than two-thirds pinned-set quorum. Cross-height time-monotonicity evidence is
outside this standalone submission path.

## 3. Verify permissions

The submitting account comes from `MISBEHAVIOUR_PRIVATE_KEY`, not
`ETH_PRIVATE_KEY`. Permissions depend on the deployment topology:

- In production router-managed mode, the signer must hold AccessManager role
  `8` (`MISBEHAVIOUR_SUBMITTER_ROLE`) for
  `ICS26Router.submitMisbehaviour`. The ICS26Router itself holds
  `SpectreClient.MISBEHAVIOUR_SUBMITTER_ROLE`.
- In direct SpectreClient mode, the signer must hold the bytes32 role returned
  by `SpectreClient.MISBEHAVIOUR_SUBMITTER_ROLE()`, unless the role is
  intentionally permissionless.

For an AccessManager-controlled router, verify both permission hops before an
incident:

```bash
MISBEHAVIOUR_SELECTOR="$(cast sig 'submitMisbehaviour(string,bytes)')"
cast call "${ACCESS_MANAGER}" \
  'canCall(address,address,bytes4)(bool,uint32)' \
  "${WATCHER_ADDRESS}" "${ICS26_ROUTER}" "${MISBEHAVIOUR_SELECTOR}" \
  --rpc-url "${EVM_RPC_URL}"

SPECTRE_ROLE="$(cast call "${SPECTRE_CLIENT}" \
  'MISBEHAVIOUR_SUBMITTER_ROLE()(bytes32)' --rpc-url "${EVM_RPC_URL}")"
cast call "${SPECTRE_CLIENT}" 'hasRole(bytes32,address)(bool)' \
  "${SPECTRE_ROLE}" "${ICS26_ROUTER}" --rpc-url "${EVM_RPC_URL}"
```

The first call must report `true` with zero delay and the second must report
`true`. If the watcher grant is missing, governance must execute the following
calldata against the AccessManager before the incident; production governance
is a timelock contract, so do not assume there is a governance private key:

```bash
cast calldata 'grantRole(uint64,address,uint32)' 8 "${WATCHER_ADDRESS}" 0
```

For direct SpectreClient mode, its role manager grants the bytes32 value
returned by `MISBEHAVIOUR_SUBMITTER_ROLE()` to the watcher address.

## 4. Generate and inspect calldata

Proof generation is expensive. A dry-run does not read either private key. Start
the prover (`scripts/local/run_prover.sh`, which serves the keys that pair with the
deployed verifier) and point the relayer at it, then generate calldata first. These
steps assume the release binary was built and verified during deployment with
`go build -o relayer ./cmd`:

```bash
export PROVER_ADDR='127.0.0.1:50060'

cd relayer
./relayer submit-misbehaviour \
  --config config.json \
  --evidence evidence.json \
  --dry-run
```

Dry-run prints JSON containing `to`, full `data`, `value`, `chain_id`, the
selected router/direct method, client ID, height, and both hashes. Submit that
calldata through a multisig or hardware wallet when policy does not permit a
hot key.

## 5. Submit

Load the dedicated key only for direct submission:

```bash
export MISBEHAVIOUR_PRIVATE_KEY='<dedicated-key>'

cd relayer
./relayer submit-misbehaviour \
  --config config.json \
  --evidence evidence.json
```

Success is a confirmed transaction receipt and prints the client ID, height,
and conflicting hashes. A transaction hash or queued transaction is not success.

## 6. Verify the freeze

Confirm the `ClientFrozen` event and read the client state:

```bash
cast logs --rpc-url "${EVM_RPC_URL}" \
  --address "${SPECTRE_CLIENT}" \
  'ClientFrozen()'

cast call "${SPECTRE_CLIENT}" 'getClientState()(bytes)' \
  --rpc-url "${EVM_RPC_URL}"
```

Decode the returned client state and confirm `isFrozen == true`. Packet relay
and client updates through this client should now fail closed.

## 7. Recovery

Do not unfreeze automatically. Selecting the canonical branch is a governance
action.

In the production topology, `ICS26Router.unfreezeClient(clientId)` is not
mapped to `UNPAUSER_ROLE`; AccessManager therefore restricts it to the default
`ADMIN_ROLE`. `GOVERNANCE_ADMIN` (the governance timelock) holds that role and
must schedule and execute the router call. The router then calls
`SpectreClient.unfreeze()` using the `DEFAULT_ADMIN_ROLE` it holds on the
client. `UNPAUSER_ACCOUNT` can call `unpause()` but cannot unfreeze a client.
Confirm these addresses against the production deployment receipt before the
incident; do not infer them from the relayer key. In direct role-managed mode,
the configured SpectreClient `DEFAULT_ADMIN_ROLE` holder must call
`unfreeze()`. In permissionless mode (`roleManager == address(0)`), no account
can call `unfreeze()`; recovery requires governance migration to a replacement
client.

Record both pieces of evidence, the submission transaction hash, the selected
branch, and the governance decision in the incident report.
