# Attestor operations and divergence runbook

## Supported transport boundary

Direct `AttestorService` gRPC is a same-host sidecar interface. Native
deployments bind it to `127.0.0.1`; Docker deployments publish both gRPC and
`/metrics` to a loopback host address. The listener inside a container may use
`0.0.0.0` only because the Docker publish rule is the enforcement point.

Direct plaintext gRPC across hosts is not a supported topology. A deployment
that needs it must place the sidecar behind an operator-managed, authenticated
TLS/mTLS or identity-aware proxy, restrict network ingress to the relayer, and
review that deployment boundary separately. Do not expose the raw attestor
port through a public load balancer or a general cluster service.

Check the local boundary after a deployment:

```sh
grpcurl -plaintext -d '{}' 127.0.0.1:3001 attestor.AttestorService/Info
curl -fsS http://127.0.0.1:9090/metrics | grep spectre_.*_attestor
```

Every production gRPC request must include the configured `src_chain`; an
empty value is rejected rather than inferred from a single feed.

A failed request returns the code of the error's class and one classified
message (for example `read replica finalized head`), never the upstream error
behind it. That error quotes the RPC endpoint URL, and a provider's API key can
be in its path or query, so any peer that reaches the unauthenticated API would
read it out of an upstream outage. The cause is in the attestor's own log as
`attestor gRPC <method> failed (<message>): <cause>`. Every URL in that line is
reduced to scheme and host, as it is in the other lines that log an upstream
failure (`attestation pass failed`, `Arbitrum assertion attestation failed`,
`attestor runtime refresh failed`, the Nitro new-head subscription lines, the OP
`DisputeGameCreated` hint line), in dial and configuration errors at startup,
and in the fatal error that ends the process. A key that is part of the host
name is not removed.

## What to watch

The Arbitrum endpoint exports `spectre_arbitrum_attestor_*`, labeled only by
`chain` (never block or assertion hash). The important signals are:

| Signal | Meaning | First action |
| --- | --- | --- |
| `nitro_{unsafe,safe,finalized}_head` stops moving | Nitro RPC/WS or runtime reconciliation is stalled | Check Nitro HTTP and WebSocket health, then `runtime_refresh_failures_total` |
| `runtime_refresh_failures_total` increases | Nitro unavailable/rate-limited | Inspect RPC error and backoff log; reduce backfill concurrency or use a dedicated endpoint |
| `assertion_next_l1_block` and `assertions_pending` increase | Finalized-L1 ingestion/reconciliation is behind | Check L1 finalized head, RollupCore RPC, and block-range settings |
| `assertion_ingest_head` stops moving | No new successful assertion ingest snapshot | Check `l1_ingest_head`, selected L1 tag, hash-based status RPC support and `assertion_sync_failures_total`; the durable cursor still tracks finalized logs only |
| `assertion_sync_failures_total` increases and the log says `finalized L1 block N is behind the assertion cursor M` | The L1 RPC answered a finalized-block read from behind, as a lagging backend behind a load balancer or a failover does. The pass is refused before it changes anything, so no assertion is pruned | Normally clears once the backend catches up. If it persists, the endpoint is permanently behind, or serves a different chain than the one `attestor_state_path` was built against |
| `assertion_sync_failures_total` increases and the log says `<call> exceeded its … deadline` (for example `L1 AssertionStatus`) | That L1 or Nitro call did not return within `assertion_call_timeout` and was cut, which fails the pass. What was committed before the cut is kept, and the next pass retries from the durable state | Check the endpoint that stopped answering (a WebSocket connection that stays open but drops a response, for example) |
| gRPC health is NOT_SERVING while `nitro_*_head` still moves | Neither the assertion loop nor any of its L1 or Nitro calls has made progress for `assertion_call_timeout` plus two `assertion_poll_interval`s | Look at the last `Arbitrum assertion attestation failed` line; a loop that logs nothing is blocked somewhere the call bound does not reach (a stalled disk write, for example), so restart it |
| `assertion_revocations_total` increases | An assertion was withdrawn before L1 finality | Inspect L1 reorg/head regression or policy-change logs; inspect Cosmos state before retrying packets |
| `roots_provisional` remains high | Finalized recheck is waiting | Compare Nitro finalized head with the provisional heights |
| `finalized_divergence_total`, `finalized_reorg_total`, or `mismatch_records` increases | Earlier evidence disagreed with the authoritative recheck | Follow the divergence procedure below |
| `persistence_failures_total` increases | New feed state was not made durable | Stop relay progression, repair storage, and restart only after state-file inspection |

OP uses the analogous `spectre_op_attestor_*` metrics; its failed passes, including
those caused by a call cut at `call_timeout_seconds`, count in `pass_failures_total`. `Info` is a
useful point-in-time diagnostic but is not a replacement for these histories.

## Opt-in early Arbitrum assertions

Keep `l1_ingest_head: finalized` for the conservative default. To evaluate early
ingestion on testnet, use `disable_derived_roots: true` to isolate the assertion
feed, choose `safe` or `latest`, and enable the relayer's `include_provisional`
policy explicitly. Keep `attestation_head` consistent with the relayer's Nitro
`head_kind`; it is a separate L2 policy, not the L1 ingest selector.

Compare the successful ingest head, finalized cursor and assertion-backed
frontier under each mode. The full unfinalized window is reread every
poll, so a finality stall can increase RPC load. Unsupported tags/hash reads
cause retries, never fallback to another ingest head.

Before gRPC starts, persisted early assertions at or beyond the finalized
cursor are durably dropped locally. This step needs no RPC or backfill; storage
failure still aborts startup. The background loop rebuilds the early window on
its first pass and retries transient RPC failures. Consumers using
`include_provisional` may briefly see a lower frontier after restart. Deployment
identity checks still require RPC access at startup. Returning to `finalized`
withdraws early assertions and leaves them out of subsequent ingestion.

Early provisional relay admits L1 reorg and assertion challenge risk. Feed
withdrawal cannot undo Cosmos consensus states or packets already accepted.
The Wasm client authenticates signatures, rejects conflicting same-height
updates, and can freeze on explicit authenticated misbehaviour; it has no
automatic provisional-root replacement or rollback. A root the attestor replaces
after signing it is itself such evidence, because the signed statement does not
say whether the root was provisional; see "Provisional attestations" in
`docs/L2_CLIENTS.md`.

## `VerifyStateRoot` mismatch or divergence

`valid=false` is not a transport error: the relayer candidate and the
attestor's canonical replica disagree. It is not an instruction for the
attestor to submit an on-chain challenge.

1. Stop or pause the affected relayer route. In particular, disable
   `include_provisional` until the source of the disagreement is understood.
2. Save evidence before restarting anything: source chain, requested height,
   expected root/hash, attestor response, `Info`, relevant L1 finalized block,
   and the exact OP game or Arbitrum assertion hash from structured logs.
3. Confirm both endpoints have the configured L1/L2 chain IDs and use the
   same `src_chain`, attestation head, public key, and deployment contract.
   A wrong route is a configuration incident; a lagging endpoint is a
   liveness incident; do not classify either as a settlement mismatch.
4. Compare the relayer L2 header with the attestor replica at the same height.
   For Arbitrum, verify the hash lookup and canonical height lookup both agree;
   for OP, check the DisputeGameFactory object at the recorded finalized L1
   block and the replica output root.
5. If a normal reorg is still converging, let the background recheck correct
   or revoke provisional state and retry only after heads converge. If the
   finalized evidence remains contradictory, keep relay paused and escalate
   with the preserved evidence.

Do not delete the persistent state as a first response. If a controlled
re-bootstrap is approved, stop the daemon, make an immutable backup of its
state file, record the deployment identity and start cursor, then launch with
an explicitly reviewed empty state path. The old path must never be reused for
a different source chain, L1/L2 identity, or settlement contract.
