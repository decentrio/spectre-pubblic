# ERC20 Lockbox Adapter

How to bridge a Cosmos-native denom into an ERC20 that **already exists on Ethereum** and cannot mint
or burn.

- Contract: [`contracts/apps/ics20/ERC20LockboxAdapter.sol`](../contracts/apps/ics20/ERC20LockboxAdapter.sol)
- Interface: [`contracts/apps/ics20/interfaces/IERC20LockboxAdapter.sol`](../contracts/apps/ics20/interfaces/IERC20LockboxAdapter.sol)
- Errors: [`contracts/apps/ics20/errors/IERC20LockboxAdapterErrors.sol`](../contracts/apps/ics20/errors/IERC20LockboxAdapterErrors.sol)
- Tests: [`test/apps/ics20/ERC20LockboxAdapterTest.t.sol`](../test/apps/ics20/ERC20LockboxAdapterTest.t.sol)

## When you need it

By default, the first packet carrying a new Cosmos denom makes `ICS20Transfer` deploy a fresh
`IBCERC20` voucher in `_getOrCreateIBCERC20`. That voucher mints on receive and burns on send —
elastic supply, always 1:1 with what the Cosmos side escrowed, zero operational burden. **Use it
unless you have a specific reason not to.**

The adapter exists for one situation: an ERC20 with holders, listings and integrations already
deployed on Ethereum, which you want to be the landing token for a Cosmos-native denom, and which has
no mint/burn you can grant to `ICS20Transfer`.

| | Default `IBCERC20` | Lockbox adapter |
|---|---|---|
| Token users hold | Auto-deployed voucher, one per denom | Your existing token, untouched |
| Supply mechanics | Mint on receive, burn on send | Locked/unlocked from a reserve |
| Inbound capacity | Unbounded | Capped by the reserve you fund |
| Registered token | The real, ERC20-compliant voucher | A shim that is deliberately not ERC20-compliant |
| Metadata | Raw denom path, fixable once via `setIBCERC20Metadata` | Your token's existing identity |
| Operational burden | None | Reserve seeding and capacity monitoring |
| New audit surface | None | The adapter |

If the token is not yet distributed, redeploying it with `IMintableAndBurnable` and registering that
is strictly simpler than this adapter. See `test/utils/RefImplIBCERC20.sol` for a
reference implementation.

## Why an adapter is needed at all

`onRecvPacket` uses the registered token address for two things at once:

```solidity
erc20Address = _getOrCreateIBCERC20(newDenom, address(escrow));
IMintableAndBurnable(erc20Address).mint(address(escrow), packetData.amount);
escrow.send(IERC20(erc20Address), receiver, packetData.amount);
```

The address must be mintable **and** be the token the receiver ends up holding. A fixed-supply ERC20
fails the first, so it cannot be registered directly — `setCustomERC20` would accept it because it
performs no compatibility check, and every packet would then fail forever.

The adapter satisfies both roles: it presents the minimal ERC20 surface the core calls, while the
real token moves in and out of a reserve the adapter itself holds. Holding the reserve in the
*adapter* rather than in the escrow is what makes this work without touching `ICS20Transfer` or
`Escrow` — the adapter moves its own tokens and never needs an allowance from the escrow.

## Mechanism

`S` is the adapter and `T` the underlying.

| Step | Core calls | Adapter does |
|---|---|---|
| **Receive** (Cosmos→ETH) | `S.mint(escrow, amt)` | credits `_balances[escrow]`; `T` does not move |
| | `escrow.send(S, receiver, amt)` → `S.transfer(receiver, amt)` | debits the credit; sends `amt · scale` of `T` from the reserve |
| **Return to origin** (ETH→Cosmos) | `S.transferFrom(user, escrow, amt)` | pulls `amt · scale` of `T` into the reserve; credits `_balances[escrow]` |
| | `escrow.burn(S, amt)` → `S.burn(amt)` | debits the calling escrow's credit; `T` stays in the reserve |
| **Forward elsewhere** | `S.transferFrom(user, otherEscrow, amt)` | locks `T`; the credit remains in `otherEscrow` until return or refund |
| **Refund** (error ack / timeout) | return-to-origin refunds mint first; all refunds end in `escrow.sendRefund(...)` | releases `T` and clears the escrow credit |

`balanceOf(escrow)` returns the credited amount, which is what the balance-snapshot check in
`ICS20Transfer._transferFrom` verifies. Receive, return-to-origin and refund callbacks clear their
temporary credit in the same transaction. Forwarding over a different client deliberately leaves a
non-zero credit in that client's escrow until the token returns or the packet is refunded; the
multi-hop tests pin both states.

### Not a user-facing token

`ICS20Transfer.ibcERC20Contract(denom)` returns the adapter, so anything that treats that return
value as the user's token will be wrong. The adapter:

- reverts `approve` with `AdapterNotTransferable`
- reverts `transfer` and `burn` from anyone but a registered escrow
- reverts `transferFrom` and `mint` from anyone but `ICS20Transfer`
- reports `allowance` as `type(uint256).max` for `ICS20Transfer` and `0` for everyone else; the core
  does not read this value, because `transferFrom` authenticates `ICS20Transfer` directly. Users
  approve only the adapter on the underlying and never approve the adapter token itself
- reports `name()` "IBC Lockbox Adapter" / `symbol()` "IBCLOCKBOX" so it is recognisable in a block
  explorer as not-the-real-token

Do not list it, price it, or integrate it.

`sendTransferWithPermit2` is not supported. Permit2 calls the adapter's `transferFrom` directly, but
that entry point accepts only `ICS20Transfer`; Permit2 therefore returns `TRANSFER_FROM_FAILED` and
the transaction leaves the user's underlying untouched. Approve the **adapter on the underlying**
and call `sendTransfer` instead.

## Denomination and scaling

All IBC-facing amounts — packet amounts, `mint`/`burn`/`transfer` arguments, rate limits, `capacity()`
— are in **denom units** (the Cosmos exponent, e.g. 6 for `uatom`). The underlying moves in
**underlying units**, multiplied by `scale`:

```
scale = 10 ** (underlyingDecimals - denomDecimals)
```

`decimals()` reports the *denom* exponent, not the underlying's.

The constructor requires `underlyingDecimals >= denomDecimals` (`AdapterInvalidDecimals`) — the
reverse would need division on the inbound path and would truncate user funds. The exponent
difference may not exceed 77, the largest power of ten representable by `uint256`. The constructor
also cross-checks the declared value against the token's own `decimals()` when the token exposes one
(`AdapterUnderlyingDecimalsMismatch`); a token that does not expose it is accepted on the declared
value.

**Ethereum cannot verify `denomDecimals`.** Confirm it manually against the counterparty chain's
bank metadata before deployment. A wrong value scales every transfer by a power of ten without an
on-chain signal, and `setCustomERC20` cannot be undone.

Because packets carry denom units, **amounts below one denom unit cannot be bridged**. With
`scale = 1e12`, a user holding `1.0000000000005e18` of the underlying can move `1.000000` and no more.
There is no rounding anywhere: every conversion is a multiplication.

## The reserve

**The reserve is inbound capacity, not collateral.** Users hold the underlying outright and have no
claim on the adapter; bridging back to Cosmos *adds* to the reserve rather than drawing from it. The
backing for Ethereum-side holders released by the bridge lives in the Cosmos-side escrow. A direct
reserve top-up adds future inbound capacity; it does not by itself create a claim or alter the
Cosmos-side escrow balance.

Capacity is read live as `underlying.balanceOf(address(this))` (`reserve()`) rather than tracked.
Initial seeding and later capacity additions are plain transfers of the underlying token to the
adapter. There is deliberately no `deposit` function, operator-principal accounting, or direct
withdrawal function.

Underlying can leave the reserve only when a registered escrow spends credit created by the
authenticated ICS20 flow. This removes the operator custody path and preserves the invariant that
every normal release corresponds to native tokens escrowed on Cosmos.

For an emergency reserve evacuation, governance may send a standard, backed ICS20 transfer from the
Nyx governance account to an Ethereum Safe. Native NYM is escrowed on Nyx and the normal receive path
releases the matching reserve to the Safe. This needs no custom IBC or chain code. It is limited by
governance's spendable native NYM, adapter capacity, and the escrow rate limit; an unbacked send must
never be used.

## Deployment

Order matters. `setCustomERC20` and enabling the launch gate are irreversible.

```solidity
string memory clientId = "eth-main";
string memory denom = "transfer/eth-main/uatom";

// 1. Pre-create the client escrow through production governance.
address escrow = ics20Transfer.getEscrow(clientId);
if (escrow == address(0)) {
    escrow = ics20Transfer.createEscrow(clientId);
}

// 2. Deploy.
ERC20LockboxAdapter adapter = new ERC20LockboxAdapter(
    address(ics20Transfer),   // the ICS20Transfer proxy
    address(underlying),      // the already-deployed ERC20
    6,                        // denom decimals   (uatom)
    18,                       // underlying decimals
    address(accessManager)    // same authority as the rest of the stack
);

// 3. Bind the adapter to the real escrow. Do not fund it yet.
adapter.addEscrow(escrow);

// 4. Install a non-zero production rate limit before registration and activation.
// The caller must hold the global, zero-delay RATE_LIMITER_ROLE.
Escrow(escrow).setRateLimit(address(adapter), 50_000e6);
adapter.setLowReserveThreshold(100_000e6);

// 5. Verify every value, then register the exact denom — ONE-SHOT,
// requires ERC20_CUSTOMIZER_ROLE.
require(adapter.ics20() == address(ics20Transfer));
require(address(adapter.underlying()) == address(underlying));
require(adapter.decimals() == 6);
require(adapter.scale() == 1e12);
require(adapter.isEscrow(escrow));
require(Escrow(escrow).getRateLimit(address(adapter)) != 0);
ics20Transfer.setCustomERC20(denom, address(adapter));
require(ics20Transfer.ibcERC20Contract(denom) == address(adapter));
require(keccak256(bytes(ics20Transfer.ibcERC20Denom(address(adapter)))) == keccak256(bytes(denom)));

// 6. Only now fund the reserve by plain ERC20 transfer and verify the exact balance.
require(INITIAL_RESERVE % adapter.scale() == 0);
uint256 reserveBefore = adapter.reserve();
underlying.transfer(address(adapter), INITIAL_RESERVE);
require(adapter.reserve() == reserveBefore + INITIAL_RESERVE);

// 7. Activate last, after every adapter invariant, mapping and balance is verified.
address[] memory tokens = new address[](1);
tokens[0] = address(adapter);
ics20Transfer.activateEscrow(clientId, tokens);
```

### Scripted deployment

[`scripts/ERC20LockboxAdapterDeploy.s.sol`](../scripts/ERC20LockboxAdapterDeploy.s.sol) performs the steps above in
that order. It reads its configuration from the environment:

| Variable | Meaning |
|---|---|
| `ICS20_TRANSFER`, `UNDERLYING` | the `ICS20Transfer` proxy and the existing ERC20 |
| `CLIENT_ID` | the router client id on this chain the denom arrives on; must be a custom identifier (no `client-` / `channel-` prefix, no `/`, 4 to 128 characters), the same rule as the production launch config |
| `BASE_DENOM` | the denom exactly as the counterparty puts it in the packet: `uatom`, or the full trace such as `transfer/channel-536/uusdc` for a voucher |
| `DENOM_DECIMALS` | the Cosmos exponent; **not verifiable on Ethereum**, check the bank metadata first |
| `UNDERLYING_DECIMALS` | optional; read from `decimals()` when unset |
| `RATE_LIMIT` | optional; required behind the launch gate or with `ACTIVATE_ESCROW`; denom units |
| `LOW_RESERVE_THRESHOLD` | optional; denom units |
| `RESERVE_AMOUNT` | optional; underlying units, a multiple of `scale`, transferred **last** from the broadcaster |
| `ACTIVATE_ESCROW` | optional; calls `activateEscrow` after everything else is verified |
| `ALLOW_ACTIVE_CLIENT` | optional; acknowledges a client that is already active (pause packet processing first) |

```bash
ICS20_TRANSFER=<proxy> UNDERLYING=<erc20> CLIENT_ID=eth-main BASE_DENOM=uatom DENOM_DECIMALS=6 \
RESERVE_AMOUNT=<n * scale> \
  forge script scripts/ERC20LockboxAdapterDeploy.s.sol:ERC20LockboxAdapterDeploy \
  --rpc-url $RPC --broadcast --sender <deployer> --private-key <key>
```

The script builds the denom key itself (`transfer/<CLIENT_ID>/<BASE_DENOM>`, the same construction as
`onRecvPacket`), so the exact-string hazard above cannot come from a typo in a pasted denom. It refuses to run if
the denom already has a registered token, and its preflight runs **before** anything is sent: if the broadcaster lacks
a role it reverts and the escrow is not even created. It needs, with zero execution delay, `ADMIN_ROLE`,
`ERC20_CUSTOMIZER_ROLE` (for `setCustomERC20`) and, when `RATE_LIMIT` is set, `RATE_LIMITER_ROLE`. An `AccessManager`
that sits behind a timelock has to execute the same calls through governance instead; the script is not a way
around that.

It prints a JSON receipt (`adapter`, `underlying`, `escrow`, `denom`, `scale`, `reserve`, `capacity`). It does not
check `DENOM_DECIMALS` against the counterparty, and it does not enable the launch gate.

**Do not pre-fund the adapter.** It intentionally has no direct withdrawal or rescue function. If
the escrow, decimals or denom mapping is wrong, a balance sent before the checks above may be
unreachable even through the emergency IBC path. Verify the underlying token's transfer behaviour
separately, complete the on-chain preflight and confirm both mappings before transferring the
material reserve.

**The denom string must be exact.** `onRecvPacket` builds it as
`"transfer/" + destinationClientId + "/" + baseDenom`, using `ICS20Lib.getDenomPrefix`. A wrong
string means the adapter is never found, a fresh `IBCERC20` is deployed for the real path, and neither
mapping can be overwritten — the denom is bricked short of an `ICS20Transfer` upgrade.

**All preparation must land before the first inbound packet.** Registration after the fact is
impossible; funding after the fact merely bounces transfers in the meantime. For a new inactive
client behind the launch gate, activation last provides that boundary. For an already-active client,
pause packet processing or use an equivalent atomic maintenance procedure while installing the
adapter; otherwise an inbound packet can create the default voucher before `setCustomERC20` lands.

**The escrow must already exist.** Use governance's `createEscrow`; do not bootstrap production by
sending an unrelated token. Once the permanent launch gate is enabled, packet processing rejects an
unprovisioned or inactive client. `activateEscrow` also rejects the adapter until its escrow rate
limit is non-zero.

`setCustomERC20` requires `ERC20_CUSTOMIZER_ROLE`. `Escrow.setRateLimit` requires the global
`RATE_LIMITER_ROLE` with zero execution delay. The adapter's own restricted functions (`addEscrow`,
`setLowReserveThreshold`) are gated by the `AccessManager` passed at construction. Because every
adapter is a new AccessManager target, selector-role wiring does not carry over automatically; the
AccessManager admin can perform the initial setup.

## Operations

### Monitoring

`capacity()` is the number to alert on — remaining inbound headroom,
already in denom units. `setLowReserveThreshold` arms a `ReserveLow(capacity, threshold)` event
emitted on the downward crossing only, so the alert does not repeat on every subsequent transfer.

Alert **proactively**. Once the reserve cannot cover a transfer, `_release` reverts with
`AdapterInsufficientReserve`, but the outcome depends on the packet path:

- On a new receive, `ICS26Router.onRecvPacket` catches the revert and writes an error
  acknowledgement. The sender's funds unlock on Cosmos, and topping up later does not retry the
  transfer; the user must send again.
- On an error-acknowledgement or timeout refund, `ICS26Router.onAcknowledgementPacket` and
  `onTimeoutPacket` do not catch the revert. The relayer transaction keeps reverting, with the
  sender's funds still escrowed on Cosmos, until the reserve is topped up and the refund succeeds.

The reserve is a shared pool and does not earmark capacity for pending refunds. Set the threshold
and top-up response time to keep enough headroom for both new receives and late refunds.

### Top-up

Transfer the underlying token directly to the adapter. `reserve()` and `capacity()` read the live
balance, so a separate adapter call or privileged key is unnecessary. Fund in multiples of `scale`:
packets contain whole denom units, so `reserve() % scale` is unusable dust unless a later top-up
completes the next whole unit. With no direct withdrawal function, residual dust cannot otherwise be
recovered.

### Rate limits

Escrow rate limits key on the *registered* token address, i.e. the adapter, and are denominated in
denom units. Setting one caps how fast the reserve can drain and gives the top-up runbook a guaranteed window
(`test_success_rateLimitAppliesToAdapter`).

## Limits and known behaviours

**One adapter per denom.** `setCustomERC20` rejects a token that is already mapped, so the same base
denom arriving through a second client id — which produces a different denom path — needs a second
adapter with its own separate reserve. This does not prevent an existing adapter token from being
forwarded over another registered client escrow.

**Escrows are a set, not a single address.** `addEscrow` can be called more than once so the token can
be forwarded onward over a client other than the one in its denom path. Each escrow must be a
contract whose `ics20()` binding matches the adapter's immutable ICS20 address. A forwarding send
leaves a non-zero adapter balance in that secondary escrow until return or refund. There is no
removal function.

**A re-created client silently diverges.** If the same Cosmos chain is later connected under a new
client id, inbound packets carry a different denom path with no adapter registered, and
`_getOrCreateIBCERC20` deploys an ordinary voucher. Register an adapter for each client id before
opening it.

**Fee-on-transfer and rebasing underlyings are not supported.** `_lock` verifies the adapter's
incoming balance delta. IBC releases verify both the adapter's exact debit and the receiver's exact
credit; any mismatch reverts the complete transaction. Direct reserve seeding must verify the actual
post-transfer adapter balance before the denom is activated.

**Capacity is capped by design.** The Cosmos-side supply of the denom can exceed the reserve
indefinitely; the bridge simply stops accepting inbound transfers at that point. Decide who backstops
the reserve before launch.

## Testing

```bash
forge test --match-contract ERC20LockboxAdapterTest -vvv
```

Tests cover both directions, decimal scaling, refunds on error-ack and timeout, reserve
exhaustion, exact underlying transfers, launch-gate configuration, rate-limit refund credits,
multi-hop forwarding, direct reserve funding, amount-scaling overflow, absence of the legacy
operator API, Permit2 rejection, and access control. The suite drives the real `ICS20Transfer` with
a mocked `ICS26Router` caller, so the call sequences above are exercised as the router would produce
them.
