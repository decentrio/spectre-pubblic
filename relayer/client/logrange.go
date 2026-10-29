package client

import "strings"

// This file answers one question about an eth_getLogs failure: did the provider
// refuse because we asked for too much?
//
// It matters because the answer decides between two opposite actions. A provider
// that is momentarily unavailable is fixed by waiting; a provider that caps the
// span is fixed only by asking for less, and waiting on it repeats the identical
// rejected request forever. Both arrive as an opaque JSON-RPC error.

// KnobBlocksPerLogRange names the quantity a log-range ladder changes, in the
// units an operator reads in a log line.
const KnobBlocksPerLogRange = "blocks per log range"

// logRangeRejectionPhrases are the ways providers say "that range, or its result
// set, was too large". They are matched case-insensitively against the error text
// because no provider reports this in a structured field -- the comment on
// l2rollup's logScanChunk already records the shape: a plain 400 that names
// neither the setting nor a workable value.
var logRangeRejectionPhrases = []string{
	"query returned more than", // geth, erigon, infura
	"logs matched by query exceeds",
	"log response size exceeded", // alchemy
	"block range",                // alchemy ("up to a 2K block range"), most gateways ("block range too large")
	"range limit",                // quicknode
	"range is too large",
	"range too large",
	"range too wide",
	"too many blocks",
}

// rateLimitPhrases are the neighbouring failure this must NOT be confused with.
//
// Several providers answer a rate limit with the same JSON-RPC code (-32005) they
// use for a range limit, which is why the code is never consulted here. A rate
// limit read as a range rejection would shrink the scan to a single block and
// then, at the bottom of the ladder, be reported permanent -- turning "wait a
// moment" into a recovery scan that is abandoned. The asymmetry is deliberate:
// these are checked first and win.
var rateLimitPhrases = []string{
	"rate limit",
	"rate-limit",
	"too many requests",
	"429",
	"capacity exceeded",
	"compute unit",
}

// prunedHistoryPhrases are the ways a node says the blocks a log query starts at
// are below its history cutoff. Geth's history expiry answers "pruned history
// unavailable" (code 4444) for any fromBlock under the cutoff, however narrow the
// range.
var prunedHistoryPhrases = []string{
	"pruned history unavailable",
	"history has been pruned",
}

// IsPrunedHistory reports whether err is a node refusing a log query because the
// range reaches below the history it still keeps.
//
// It is a different failure from IsLogRangeRejection and must not be read as one:
// asking for a narrower span starting at the same block is refused the same way.
// The fix is a HIGHER lower bound, not a smaller width.
func IsPrunedHistory(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	for _, phrase := range prunedHistoryPhrases {
		if strings.Contains(msg, phrase) {
			return true
		}
	}
	return false
}

// IsLogRangeRejection reports whether err is a provider refusing a log query
// because the span, or the result set it produced, was too large.
//
// It is deliberately conservative: an error it does not recognise is not a range
// rejection, which leaves it classified transient and retried unchanged. That is
// the safe direction — the cost of missing one is a scan that keeps failing
// visibly, while the cost of a false positive is a scan narrowed to nothing and
// then abandoned.
func IsLogRangeRejection(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	for _, phrase := range rateLimitPhrases {
		if strings.Contains(msg, phrase) {
			return false
		}
	}
	for _, phrase := range logRangeRejectionPhrases {
		if strings.Contains(msg, phrase) {
			return true
		}
	}
	return false
}

// LogSpan is the block span one eth_getLogs asks for, narrowed in place when a
// provider refuses it.
//
// It is a pointer passed down a scan loop rather than a return value, so a
// narrowing SURVIVES the tick that discovered it. Rediscovering the provider's
// cap every tick would also work, but it spends a rejected request each time and
// hides the cap from the log after the first line. Each scan loop owns its own
// LogSpan on one goroutine, so it carries no lock; that ownership is stated at
// each declaration.
//
// Chunk is 0 for "one call for the whole range", which is what a provider that
// caps nothing wants.
//
// Floor is the lowest block the provider still serves logs for, learned from a
// pruned-history refusal (see IsPrunedHistory); 0 means none has been seen. A
// lookup that walks back stops there instead of asking for blocks the node has
// already said it no longer has.
type LogSpan struct {
	Chunk uint64
	Floor uint64
}

// Width is the span this policy would actually ask for over [from,to].
//
// It reports the FULL range when Chunk is 0 or wider than the range, which is
// what makes narrowing work for a source nobody configured: the ladder anchors on
// the span the provider really refused, not on a configured number that was never
// used.
func (p *LogSpan) Width(from, to uint64) uint64 {
	if to < from {
		return 0
	}
	full := to - from + 1
	if p.Chunk == 0 || p.Chunk > full {
		return full
	}
	return p.Chunk
}
