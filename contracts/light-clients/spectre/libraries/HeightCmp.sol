// SPDX-License-Identifier: MIT
pragma solidity ^0.8.28;

import { IICS02ClientMsgs } from "contracts/core/messages/IICS02ClientMsgs.sol";

library HeightCmp {
    // Enum to represent ordering results (equivalent to Rust's Ordering)
    enum Ordering {
        Less,
        Equal,
        Greater
    }

    /**
     * @dev Compare two Height structs
     * @param self First Height struct
     * @param other Second Height struct
     * @return Ordering result (Less, Equal, or Greater)
     */
    function cmp(
        IICS02ClientMsgs.Height memory self,
        IICS02ClientMsgs.Height memory other
    )
        internal
        pure
        returns (Ordering)
    {
        if (self.revisionNumber < other.revisionNumber) {
            return Ordering.Less;
        } else if (self.revisionNumber > other.revisionNumber) {
            return Ordering.Greater;
        } else if (self.revisionHeight < other.revisionHeight) {
            return Ordering.Less;
        } else if (self.revisionHeight > other.revisionHeight) {
            return Ordering.Greater;
        } else {
            return Ordering.Equal;
        }
    }

    /**
     * @dev Check if self is less than other
     */
    function lt(IICS02ClientMsgs.Height memory self, IICS02ClientMsgs.Height memory other)
        internal
        pure
        returns (bool)
    {
        return cmp(self, other) == Ordering.Less;
    }

    /**
     * @dev Check if self is less than or equal to other
     */
    function le(IICS02ClientMsgs.Height memory self, IICS02ClientMsgs.Height memory other)
        internal
        pure
        returns (bool)
    {
        Ordering result = cmp(self, other);
        return result == Ordering.Less || result == Ordering.Equal;
    }

    /**
     * @dev Check if self is greater than other
     */
    function gt(IICS02ClientMsgs.Height memory self, IICS02ClientMsgs.Height memory other)
        internal
        pure
        returns (bool)
    {
        return cmp(self, other) == Ordering.Greater;
    }

    /**
     * @dev Check if self is greater than or equal to other
     */
    function ge(IICS02ClientMsgs.Height memory self, IICS02ClientMsgs.Height memory other)
        internal
        pure
        returns (bool)
    {
        Ordering result = cmp(self, other);
        return result == Ordering.Greater || result == Ordering.Equal;
    }

    /**
     * @dev Check if self is equal to other
     */
    function eq(IICS02ClientMsgs.Height memory self, IICS02ClientMsgs.Height memory other)
        internal
        pure
        returns (bool)
    {
        return cmp(self, other) == Ordering.Equal;
    }

    /**
     * @dev Check if self is not equal to other
     */
    function ne(IICS02ClientMsgs.Height memory self, IICS02ClientMsgs.Height memory other)
        internal
        pure
        returns (bool)
    {
        return cmp(self, other) != Ordering.Equal;
    }
}
