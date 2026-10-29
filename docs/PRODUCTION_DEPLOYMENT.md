# Production deployment

`ProductionDeploy.s.sol` bootstraps the IBC proxies under an `AccessManager` and
removes the bootstrap account before returning the deployment addresses.
`ProductionVerify.s.sol` must be run against the same addresses and environment
configuration after deployment.

## Governance requirements

`GOVERNANCE_ADMIN` must be an OpenZeppelin `TimelockController`, or a compatible
contract exposing `getMinDelay()`, with a minimum delay at least equal to
`SECURITY_DELAY` (two days by default). The AccessManager `ADMIN_ROLE` has an
execution delay of zero by design, so the governance timelock is what protects
role and selector changes made by the administrator.

The `UPGRADER_ACCOUNT`, `UNPAUSER_ACCOUNT`, and `ID_CUSTOMIZER_ROLE` use the
AccessManager delay. The relayer, pausers, and misbehaviour watcher are
immediate operational roles.

## Escrow launch gate

`RATE_LIMITER_ROLE` is global: it can configure every escrow, including an
escrow created after deployment. Production deployment enables a permanent
launch gate and, for each configured client, creates its escrow, installs
non-zero limits for each configured ERC-20, then activates the escrow. Both
deployment and verification reject zero-address or non-contract token entries.

Every id in `PRODUCTION_ESCROW_CONFIG` must be a **custom** client id, not one
in the `client-` namespace (or `channel-`). The escrow is created and activated
before any client exists, and the default-ID `addClient` is permissionless, so
the first caller could take a launch id such as `client-0` and inherit its
activated escrow. Custom ids are disjoint from that namespace and can only be
registered by the `ID_CUSTOMIZER_ROLE` holder. Deployment and verification
reject any other id.

For a later client, preserve the same order: `createEscrow(clientId)` through
governance, install non-zero limits from the rate-limiter account, then call
`activateEscrow(clientId, tokens)` through governance. Packet processing
rejects a missing or inactive escrow, so the client cannot operate before its
listed tokens have the configured limits.

Activation is a point-in-time check of only the supplied token list. Removing a
limit later by setting it to `0` does not deactivate the escrow, and tokens not
included in the activation call remain uncapped. Governance and the rate-limiter
must therefore treat later token additions and limit changes as separate safety
decisions.

When upgrading an existing live deployment, activate every existing escrow with
its required token list **before** calling `enableEscrowLaunchGate()`. The active
mapping defaults to `false`, so enabling the permanent gate first would halt
packet processing for those clients. `activateEscrow` is available before the
gate is enabled, which permits this safe migration order.

## Light-client provisioning

The documented `./relayer create-clients-eth` flow submits `addClient` directly
from the relayer key. A production deployment grants the custom-ID overload,
`addClient(string clientId, CounterpartyInfo, address client)`, to governance
with an AccessManager delay, so that command is not a complete production
provisioning flow. The default-ID overload, `addClient(CounterpartyInfo,
address)`, is **not** access-controlled in any deployment: anyone can call it
and receive the next `client-<seq>` id. Do not rely on `client-N` ids for
anything that must be operator-controlled, and register launch clients under
their custom id. Deploy the light-client implementation separately, then
schedule and execute the custom-ID `addClient` calldata through the governance
and AccessManager timelocks before writing the resulting client address to
relayer configuration.

A `SpectreClient` is built with its UpdateClient, Membership and Misbehaviour
modules as immutables, and its constructor reverts with
`ModuleHasNoCode(module)` for an address that holds no code, `address(0)`
included, so deploy the three modules first. `./relayer create-clients-eth`
applies the same rule before it spends gas: it refuses to deploy unless
`update_client`, `membership` and `misbehaviour` are set and hold code on
`eth_rpc_url`.

A client deployed before this check, with a misbehaviour module that has no
code, treats a call to that module as a verified proof. Whoever may submit
misbehaviour (anyone, when the client was deployed with
`roleManager == address(0)`) can then freeze it with two identical headers and
a quorum in the proof metadata. The module addresses are private immutables
with no getter, so they have to be read from the constructor arguments. For a
client created by a plain contract-creation transaction, which is what
`./relayer create-clients-eth` sends, the ABI-encoded arguments follow the
creation code in that transaction's input data, and their first three 32-byte
words are the UpdateClient, Membership and Misbehaviour module addresses, in
that order. A client created through a factory or with `CREATE2` has no such
transaction: its arguments are in the factory call's input or in the internal
call trace of the transaction that created it. Migrate any client whose
misbehaviour module has no code (see Governed client migration).

`addClient` (both overloads) rejects a light-client address that holds no code
and a counterparty whose Merkle prefix has no elements, so the scheduled
custom-ID call reverts at execution if the light client has not been deployed
by then. A prefix with a single empty element is valid; an empty list is not.

The router does not deduplicate clients. Two local clients that name the same
counterparty client id share that counterparty's receipt and acknowledgement
paths (`ICS24Host.packetReceiptCommitmentPathCalldata` and
`packetAcknowledgementCommitmentPathCalldata`). A packet sent through an
unofficial duplicate can therefore be acknowledged with the official packet's
acknowledgement when their sequences coincide, and it can never time out. Only
funds a user chose to send through the duplicate are affected, but the
default-ID overload is permissionless, so anyone can create one. Wallets,
front ends and relayer configurations must use the launch ids listed in
`PRODUCTION_ESCROW_CONFIG`, never a `client-N` id they have not checked against
`getClient` and `getCounterparty`.

Once the launch clients are registered, re-run `ProductionVerify.s.sol` with
`PRODUCTION_LIGHT_CLIENTS='{"lightClients":["0x..", ..]}'`: one light-client
address per id in `PRODUCTION_ESCROW_CONFIG` `.clients`, in the same order.
Verification then requires `getClient(<launch id>)` to return exactly that
address, so a launch id registered to any other contract fails. The variable is
optional because the clients do not exist when the bootstrap is first verified;
when it is unset the script logs that registration was not verified and passes,
so a pass without it says nothing about the launch clients.

### Governed client migration

Migration is scoped by `LIGHT_CLIENT_MIGRATOR_ROLE_<clientId>`. No deployment
script grants it: the bootstrap cannot grant it for a `client-N` id, which does
not exist yet. For a custom launch id the role id is derived from the id alone,
so `getLightClientMigratorRole(clientId)` already answers and the grant can be
made before the client is registered.

A frozen client rejects `verifyMembership` and `verifyNonMembership`
(`SpectreClient.sol`, `notFrozen`), so acknowledgements and timeouts, and with
them every refund of an escrowed transfer, stop until the client is unfrozen
(a governance action, see `MISBEHAVIOUR_RUNBOOK.md`) or replaced by a migration.
Without a migrator in place, a migration also has to wait for the role grant to
clear the governance delay before its own wait of at least 48 hours can start.
Grant the role, with a non-zero delay, for every launch client **before** its
escrow is activated, so that recovering from a freeze costs only the proposal
wait.

1. Read the role ID from `getLightClientMigratorRole(clientId)`.
2. Through governance, grant that role to the approved migrator with a
   **non-zero** AccessManager execution delay. A zero-delay grant cannot create a
   proposal.
3. The granted migrator calls `proposeClientMigration` directly with the exact
   client ID, counterparty information, and replacement light-client address.
4. Verify the emitted proposal or `getClientMigration(clientId)`, then wait
   until `executeAfter`.
5. During the execution window, any account may submit the exact committed
   values to `executeClientMigration`.
6. Revoke the per-client role after the approved migration is complete.

The contract enforces a wait of `max(role execution delay, 48 hours)` and
re-checks at execution that the original proposer still holds the role, and
still with a non-zero execution delay. The proposal must preserve the client's
existing counterparty client ID and Merkle prefix; changing either binding is
rejected before the delay begins. Both `proposeClientMigration` and
`executeClientMigration` reject a replacement client address that holds no code,
so a migration can neither unregister a client nor leave it unable to verify.

`migrateClient` remains an ABI-compatible alias for
`executeClientMigration`; it executes a mature matching proposal rather than
failing unconditionally. New operator tooling should use the explicit
`executeClientMigration` name.

Cancellation requires `PAUSER_ROLE` with AccessManager execution delay
**exactly zero**. Such a pauser may call `cancelClientMigration` even while the
router target is closed.

`executeClientMigration` is not gated by the router's pause. It checks only that
the router target is not closed in the AccessManager, so a mature proposal can
still be executed by anyone while the router is paused. During an incident the
control is `cancelClientMigration`, together with revoking the migrator role.

An authorized migrator can occupy a client ID with an unwanted proposal for the
full proposal window. Revoking that migrator blocks execution, but does not free
the proposal slot until it expires. For an unwanted proposal, revoke the role
and have a zero-delay pauser cancel it before granting a replacement migrator.

### Migrating the Ethereum 08-wasm client on Cosmos

The Ethereum client that Cosmos runs (`programs/cw-ics08-wasm-eth`, an ICS-08 wasm
client) is not covered by the router procedure above. It is migrated by a
governance `MsgMigrateContract` whose `migration` is `code_only`, `reinstantiate`
or `update_fork_parameters`. Every migration needs a wasm checksum the client does
not already run, because ibc-go refuses a migration to the current checksum
(`ErrWasmCodeExists`, `modules/light-clients/08-wasm/v10@v10.3.0/keeper/keeper.go:154-156`).

- `code_only` swaps the code and writes no client or consensus state.
- `update_fork_parameters` replaces the fork schedule. The genesis parameters and
  any fork the client has already reached (its epoch is at or before the epoch of
  the client's latest slot) cannot change, because the fork version selects the
  signing domain. A fork that has not activated can be scheduled or moved, but only
  to an epoch after the client's latest one, and activation epochs must not
  decrease from Altair to Gloas. A client created with a wrong version for an
  active fork is repaired with `reinstantiate`, not with this variant.
- `reinstantiate` is the only way to unfreeze the client. It deletes **every**
  stored consensus state, then writes the new client state and its bootstrap
  consensus state. Nothing else removes a consensus state, so before this the set
  only grows. Membership proofs verify against stored consensus states only: a
  proof built at a height older than the bootstrap stops verifying, and packets in
  flight must be proven again at the bootstrap height or later. The work grows with
  the number of stored consensus states, so check that it fits the proposal's gas
  limit before relying on it for a recovery.

`reinstantiate` rejects a bootstrap whose client state is already frozen, has
`sync_committee_size` of zero, has `min_sync_committee_participants` outside
`1..=sync_committee_size`, or has fork epochs that decrease. It also rejects a
consensus state without `next_sync_committee`, because a client that does not know
its next committee cannot cross into the next sync-committee period.
`create-clients-cosmos` always sets it; a hand-written bootstrap must too.

## Pausing

The router and ICS20Transfer have separate pause switches and they do not stop
the same things.

Pausing the router stops `sendPacket` and `recvPacket` (`ICS26Router.sol`,
`whenNotPaused`). `ackPacket` and `timeoutPacket` stay open, so refunds of
packets that were already sent are still processed. A relayed `recvPacket`
reverts as a whole before any receipt is written, so the packet is not consumed
and can still be delivered after the router is unpaused.

Pausing ICS20Transfer alone stops `sendTransfer`, `sendTransferWithPermit2`,
`sendTransferWithSender` and `onRecvPacket`, but not acknowledgements and
timeouts (`ICS20Transfer.sol`). The router does not know about it: it still
accepts `recvPacket`, writes the receipt, calls the paused application and
commits the resulting `EnforcedPause()` failure as an error acknowledgement.
Every inbound packet relayed in that window is refunded on its source chain and
**can never be delivered**, not even after ICS20Transfer is unpaused.

Pause and unpause in this order, so that the router is never open while
ICS20Transfer is closed:

1. Pause the router.
2. Pause ICS20Transfer, if it has to be paused as well.
3. Unpause ICS20Transfer.
4. Unpause the router last.

## Escrow upgrade compatibility

The token-layer correctness upgrade changes the `Escrow.recvCallback` and
`Escrow.sendRefund` ABI. Upgrade the Escrow beacon before, or atomically with,
the ICS20Transfer implementation; activating the new transfer implementation
against the old escrow implementation makes sends revert.

Refund-credit records exist only for packets sent after this upgrade. Refunds
of packets already in flight restore zero rate-limit usage, so operators must
account for that one-time transition when monitoring escrow limits.

The burn-signature change adds a second constraint of the same shape, in the
same direction. `Escrow` gains `burn(IMintableAndBurnable,uint256)` and
ICS20Transfer starts calling it instead of burning through the token directly,
so the Escrow beacon must again be upgraded before, or atomically with, the
ICS20Transfer implementation. A new transfer implementation against an old
escrow reverts on any send that returns a voucher to its source chain — the
function it calls does not exist there.

The token side changes too: `IMintableAndBurnable.burn` becomes `burn(uint256)`,
burning the caller's own balance and restricted to the escrow, replacing
`burn(address,uint256)`.

The two ends of the burn therefore have to flip together. Working through the
combinations, an old ICS20Transfer burns through the token directly and needs
the old `burn(address,uint256)`; a new one burns through the escrow and needs
the new `burn(uint256)`. Upgrading the IBCERC20 beacon first breaks the old
transfer implementation for the same reason upgrading ICS20Transfer first
breaks against the old escrow.

The Escrow beacon is the exception: its new `burn` is unreachable until
ICS20Transfer calls it, so it can be upgraded on its own at any point
beforehand.

So either upgrade all three atomically — the recommended path — or upgrade the
Escrow beacon first and then the IBCERC20 beacon **and** ICS20Transfer
atomically with each other. No send returning a voucher to its source chain may
be relayed while those two are out of step.

Any custom ERC20 registered through `setCustomERC20` must also implement the
new `burn(uint256)` and restrict it to its escrow. A token still exposing
`burn(address,uint256)` will revert once ICS20Transfer routes the burn through
the escrow. `mint(address,uint256)` is unchanged.

### Migrating existing `RefImplIBCERC20` proxies

The updated `RefImplIBCERC20` appends its escrow address to the existing
ERC-7201 storage namespace. Proxies initialized by the older reference
implementation have only the preserved `_ics20` field, so their owner must
migrate the new escrow field atomically with the implementation upgrade:

```solidity
proxy.upgradeToAndCall(
    address(newRefImplIBCERC20),
    abi.encodeCall(RefImplIBCERC20.initializeEscrow, (escrowAddress))
);
```

`escrowAddress` must be non-zero. `initializeEscrow` is an owner-only
`reinitializer(2)` and additionally rejects a proxy whose escrow is already
configured, so it cannot replace an escrow or run twice. Perform this call
before allowing return-to-source voucher sends; those sends route the burn
through the configured escrow.

## Upgrading a deployment that predates the AccessManager

Each of these contracts has a one-time `initializeV2` that installs the
AccessManager as its authority. Run them in this order:

1. `ICS20Transfer.initializeV2(authority)`. It authenticates the caller through
   the router's legacy `isAdmin`, which a router running the current
   implementation no longer exposes (`ICS20Transfer.sol`, `initializeV2`), so
   ICS20Transfer has to be initialized while the router is still on the legacy
   implementation.
2. `ICS26Router.initializeV2(authority)`. It requires a legacy administrator
   and clears the legacy administrators.
3. `Escrow.initializeV2()` on every escrow. Anyone can call it, and it copies
   the authority from ICS20Transfer, so it has to follow step 1. Called earlier
   it reverts with `AccessManagedInvalidAuthority(address(0))`: an escrow
   initialized without an authority would keep none, and its rate limit could
   never be set.
