package chain

// HalvingLadder halves a quantity on each attempt, down to 1.
//
// Two unrelated-looking resource failures in this relayer have the same remedy --
// a batch the chain would not execute, and a block range a provider would not
// serve -- and both are answered by asking for half as much. Sharing the ladder
// is not tidiness: it means a fix to how the search terminates lands on both,
// instead of on whichever one someone remembered.
//
// Halving rather than stepping down by a constant is what bounds that search. The
// caps involved differ by three orders of magnitude between providers -- the
// comment on l2rollup's logScanChunk records Alchemy's free tier at 10 blocks and
// drpc at 10_000 -- so a linear walk from a configured 10_000 down to a real 10
// would spend a thousand rejected calls discovering it. Halving spends ten.
//
// It stops at 1 rather than 0: one message, or one block, is the smallest request
// that still asks for something. A rejection there is not a sizing problem any
// more, and Climb is what turns it permanent.
func HalvingLadder(what string, from uint64) ChangeFunc {
	return func(attempt int) (Attempt, bool) {
		if attempt < 0 || from <= 1 {
			return Attempt{}, false
		}
		size := from
		for i := 0; i <= attempt; i++ {
			size /= 2
			if size < 1 {
				return Attempt{}, false
			}
		}
		return Attempt{What: what, To: size}, true
	}
}
