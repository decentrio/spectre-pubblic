// SPDX-License-Identifier: MIT
pragma solidity ^0.8.28;

import { IERC20 } from "@openzeppelin-contracts/token/ERC20/IERC20.sol";
import { IERC20Metadata } from "@openzeppelin-contracts/token/ERC20/extensions/IERC20Metadata.sol";
import { IMintableAndBurnable } from "contracts/apps/ics20/interfaces/IMintableAndBurnable.sol";
import { IERC20LockboxAdapter } from "contracts/apps/ics20/interfaces/IERC20LockboxAdapter.sol";
import { IERC20LockboxAdapterErrors } from "contracts/apps/ics20/errors/IERC20LockboxAdapterErrors.sol";
import { IEscrow } from "contracts/apps/ics20/interfaces/IEscrow.sol";

import { AccessManaged } from "@openzeppelin-contracts/access/manager/AccessManaged.sol";
import { SafeERC20 } from "@openzeppelin-contracts/token/ERC20/utils/SafeERC20.sol";

using SafeERC20 for IERC20;

/// @title ERC20 Lockbox Adapter
/// @notice Lets an already-deployed, non-mintable ERC20 be the Ethereum-side representation of a
/// Cosmos-native IBC denom, by locking and unlocking a reserve instead of minting and burning.
///
/// @dev THIS CONTRACT IS NOT A USER-FACING ERC20. It is registered as the denom's token via
/// `ICS20Transfer.setCustomERC20`, so `ICS20Transfer.ibcERC20Contract(denom)` returns this address,
/// but users hold the *underlying* token and never a balance here. Peer-to-peer transfers and
/// approvals revert; `balanceOf` is meaningful only for registered escrows. A credit is normally
/// cleared in the same callback, but remains while a token is forwarded over a non-origin client.
/// Do not list it, price it, or integrate it as a token.
///
/// @dev Denomination: all IBC-facing amounts (`mint`, `burn`, `transfer`, `transferFrom`, packet
/// amounts, rate limits) are in *denom units* — the Cosmos denom's exponent. The underlying is moved
/// in *underlying units*, scaled by `scale`. A consequence is that amounts below one denom unit
/// cannot be bridged, since packets cannot express them.
///
/// @dev Flow:
///  - receive: ICS20 calls `mint(escrow, amt)` then the escrow calls `transfer(receiver, amt)`,
///    which releases `amt * scale` of the underlying from the reserve.
///  - send: ICS20 calls `transferFrom(user, escrow, amt)`, which pulls `amt * scale` of the
///    underlying into the reserve. When returning to origin, ICS20 asks the escrow to call
///    `burn(amt)`; when forwarding elsewhere, the escrow credit persists until return or refund.
///  - refund: same as receive, driven by `_refundTokens`.
///
/// @dev The reserve is inbound capacity, not collateral: users hold the underlying outright and
/// bridging back *adds* to the reserve. Each authenticated inbound release is matched by native
/// denom newly escrowed on Cosmos. The reserve has no operator deposit or withdrawal path: it is
/// seeded and topped up by direct underlying-token transfers and may leave only through
/// authenticated IBC releases. Direct funding should be a multiple of `scale`; sub-scale residue
/// cannot be represented in an IBC packet.
contract ERC20LockboxAdapter is
    IERC20Metadata,
    IMintableAndBurnable,
    IERC20LockboxAdapter,
    IERC20LockboxAdapterErrors,
    AccessManaged
{
    /// @notice The ICS20Transfer contract allowed to mint and pull on behalf of users
    address public immutable ics20;
    /// @notice The already-deployed ERC20 held in reserve and delivered to users
    IERC20 public immutable underlying;
    /// @notice Multiplier from denom units to underlying units
    uint256 public immutable scale;

    /// @dev The decimals of the IBC denom, reported as this contract's `decimals()`
    uint8 private immutable _denomDecimals;

    /// @notice Capacity level below which `ReserveLow` is emitted, in denom units; zero disables it
    uint256 public lowReserveThreshold;

    /// @dev Registered escrow contracts, the only addresses allowed to release the underlying
    mapping(address escrow => bool registered) private _escrows;
    /// @dev Escrow balances in denom units. Forwarded-token credits persist until return or refund.
    mapping(address account => uint256 balance) private _balances;
    /// @dev Sum of `_balances`
    uint256 private _totalSupply;

    /// @notice Deploys the adapter for one IBC denom
    /// @dev `underlyingDecimals_` must be at least `denomDecimals_`: the reverse would require
    /// dividing on the way in, truncating user funds. It is cross-checked against the underlying
    /// when the token exposes `decimals()`.
    /// @param ics20_ The ICS20Transfer contract address
    /// @param underlying_ The already-deployed ERC20 to lock and unlock
    /// @param denomDecimals_ The exponent of the Cosmos denom (for example 6 for `uatom`). This
    /// value cannot be verified on Ethereum and must be checked against counterparty bank metadata.
    /// @param underlyingDecimals_ The decimals of the underlying token
    /// @param authority_ The AccessManager authority governing the restricted functions
    constructor(
        address ics20_,
        address underlying_,
        uint8 denomDecimals_,
        uint8 underlyingDecimals_,
        address authority_
    )
        AccessManaged(authority_)
    {
        require(ics20_ != address(0), AdapterZeroAddress());
        require(underlying_ != address(0), AdapterZeroAddress());
        require(authority_ != address(0), AdapterZeroAddress());
        require(ics20_.code.length != 0, AdapterNotContract(ics20_));
        require(underlying_.code.length != 0, AdapterNotContract(underlying_));
        require(authority_.code.length != 0, AdapterNotContract(authority_));
        require(underlyingDecimals_ >= denomDecimals_, AdapterInvalidDecimals(denomDecimals_, underlyingDecimals_));

        uint256 exponent = uint256(underlyingDecimals_) - uint256(denomDecimals_);
        require(exponent <= 77, AdapterInvalidDecimals(denomDecimals_, underlyingDecimals_));

        try IERC20Metadata(underlying_).decimals() returns (uint8 reported) {
            require(reported == underlyingDecimals_, AdapterUnderlyingDecimalsMismatch(reported, underlyingDecimals_));
        } catch {
            // The underlying does not expose decimals(); the declared value stands.
        }

        ics20 = ics20_;
        underlying = IERC20(underlying_);
        scale = 10 ** exponent;
        _denomDecimals = denomDecimals_;
    }

    /// @inheritdoc IERC20LockboxAdapter
    function addEscrow(address escrow) external restricted {
        require(escrow != address(0), AdapterZeroAddress());
        require(escrow.code.length != 0, AdapterNotContract(escrow));

        address escrowICS20;
        try IEscrow(escrow).ics20() returns (address configuredICS20) {
            escrowICS20 = configuredICS20;
        } catch {
            revert AdapterInvalidEscrow(escrow);
        }
        require(escrowICS20 == ics20, AdapterEscrowICS20Mismatch(escrow, ics20, escrowICS20));

        _escrows[escrow] = true;
        emit EscrowRegistered(escrow);
    }

    /// @inheritdoc IERC20LockboxAdapter
    function setLowReserveThreshold(uint256 threshold) external restricted {
        lowReserveThreshold = threshold;
        emit LowReserveThresholdSet(threshold);
    }

    /// @inheritdoc IMintableAndBurnable
    /// @dev Credits the escrow's denom-unit balance. The underlying moves on the subsequent
    /// `transfer` from the escrow, not here.
    function mint(address mintAddress, uint256 amount) external {
        require(_msgSender() == ics20, AdapterUnauthorized(_msgSender()));
        require(_escrows[mintAddress], AdapterNotEscrow(mintAddress));

        _balances[mintAddress] += amount;
        _totalSupply += amount;

        emit Transfer(address(0), mintAddress, amount);
    }

    /// @inheritdoc IMintableAndBurnable
    /// @dev Clears the escrow's denom-unit balance. The underlying stays in the reserve: it was
    /// already pulled in by the preceding `transferFrom`.
    function burn(uint256 amount) external {
        address escrow = _msgSender();
        require(_escrows[escrow], AdapterNotEscrow(escrow));

        _balances[escrow] -= amount;
        _totalSupply -= amount;

        emit Transfer(escrow, address(0), amount);
    }

    /// @inheritdoc IERC20
    /// @dev Callable only by a registered escrow. Releases `amount * scale` of the underlying.
    function transfer(address to, uint256 amount) external returns (bool) {
        require(_escrows[_msgSender()], AdapterNotEscrow(_msgSender()));

        _balances[_msgSender()] -= amount;
        _totalSupply -= amount;

        emit Transfer(_msgSender(), to, amount);
        _release(to, amount);

        return true;
    }

    /// @inheritdoc IERC20
    /// @dev Callable only by the ICS20 contract, and only into a registered escrow. Pulls
    /// `amount * scale` of the underlying from `from` into the reserve. The external token call
    /// intentionally precedes the credit: a reentrant `transfer` cannot spend an uncreated balance,
    /// while `mint` and recursive `transferFrom` still require the authenticated ICS20 caller.
    function transferFrom(address from, address to, uint256 amount) external returns (bool) {
        require(_msgSender() == ics20, AdapterUnauthorized(_msgSender()));
        require(_escrows[to], AdapterNotEscrow(to));

        _lock(from, amount);

        _balances[to] += amount;
        _totalSupply += amount;

        emit Transfer(from, to, amount);

        return true;
    }

    /// @inheritdoc IERC20
    /// @dev Always reverts: this contract is not a user-transferable token.
    function approve(address, uint256) external pure returns (bool) {
        revert AdapterNotTransferable();
    }

    /// @inheritdoc IERC20LockboxAdapter
    function reserve() public view returns (uint256) {
        return underlying.balanceOf(address(this));
    }

    /// @inheritdoc IERC20LockboxAdapter
    function capacity() external view returns (uint256) {
        return reserve() / scale;
    }

    /// @inheritdoc IERC20LockboxAdapter
    function isEscrow(address account) external view returns (bool) {
        return _escrows[account];
    }

    /// @inheritdoc IERC20
    function totalSupply() external view returns (uint256) {
        return _totalSupply;
    }

    /// @inheritdoc IERC20
    /// @dev Non-zero only for a registered escrow; forwarding credits may persist across callbacks.
    function balanceOf(address account) external view returns (uint256) {
        return _balances[account];
    }

    /// @inheritdoc IERC20
    /// @dev Reports implicit approval for ERC20 compatibility; the core does not read allowance,
    /// because `transferFrom` authenticates the ICS20 caller directly. Everyone else has none.
    function allowance(address, address spender) external view returns (uint256) {
        return spender == ics20 ? type(uint256).max : 0;
    }

    /// @inheritdoc IERC20Metadata
    function name() external pure returns (string memory) {
        return "IBC Lockbox Adapter";
    }

    /// @inheritdoc IERC20Metadata
    function symbol() external pure returns (string memory) {
        return "IBCLOCKBOX";
    }

    /// @inheritdoc IERC20Metadata
    /// @dev The exponent of the IBC denom, not of the underlying token.
    function decimals() external view returns (uint8) {
        return _denomDecimals;
    }

    /// @notice Releases the underlying from the reserve to a receiver
    /// @param to The receiver of the underlying
    /// @param amount The amount in denom units
    function _release(address to, uint256 amount) private {
        uint256 required = amount * scale;
        uint256 available = reserve();
        // solhint-disable-next-line gas-strict-inequalities
        require(available >= required, AdapterInsufficientReserve(required, available));

        uint256 reserveAfter = _transferUnderlyingExact(to, required, available);

        uint256 threshold = lowReserveThreshold;
        if (threshold != 0) {
            uint256 capacityAfter = reserveAfter / scale;
            // solhint-disable-next-line gas-strict-inequalities
            if (available / scale >= threshold && capacityAfter < threshold) {
                emit ReserveLow(capacityAfter, threshold);
            }
        }
    }

    /// @notice Transfers underlying and requires exact balance deltas on both sides
    /// @param to The receiver of the underlying
    /// @param amount The amount in underlying units
    /// @param reserveBefore The adapter reserve immediately before the transfer
    /// @return reserveAfter The adapter reserve immediately after the transfer
    function _transferUnderlyingExact(
        address to,
        uint256 amount,
        uint256 reserveBefore
    )
        private
        returns (uint256 reserveAfter)
    {
        uint256 receiverBefore = underlying.balanceOf(to);
        underlying.safeTransfer(to, amount);

        reserveAfter = reserve();
        uint256 receiverAfter = underlying.balanceOf(to);
        uint256 debited = reserveBefore >= reserveAfter ? reserveBefore - reserveAfter : 0;
        uint256 credited = receiverAfter >= receiverBefore ? receiverAfter - receiverBefore : 0;
        require(debited == amount && credited == amount, AdapterUnexpectedUnderlyingTransfer(amount, debited, credited));
    }

    /// @notice Pulls the underlying from a sender into the reserve
    /// @param from The address to pull the underlying from
    /// @param amount The amount in denom units
    function _lock(address from, uint256 amount) private {
        uint256 required = amount * scale;

        uint256 balanceBefore = reserve();
        underlying.safeTransferFrom(from, address(this), required);
        uint256 received = reserve() - balanceBefore;
        require(received == required, AdapterUnexpectedUnderlyingBalance(required, received));
    }
}
