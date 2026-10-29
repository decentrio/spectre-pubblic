package l2rollup

import (
	"errors"
	"fmt"
	"log"

	"relayer/chain"
)

// This file is the whose-fault-is-it table for the attestor. Every failure the
// attestor can hand back gets a class, and two of them get an alarm.
//
// Before it, the relayer distinguished exactly ONE attestor failure —
// Unimplemented on VerifyStateRoot — and every other one arrived as a bare
// wrapped error. chain.Classify defaults an unclassified error to transient, so
// "your src_chain does not exist" and "your request is malformed" were retried on
// every flush, forever, at the same log line. That is not a hypothetical shape:
// it is the same disease as the 748 identical queue lines in the 6h13m production
// log, and neither is fixed by reading harder.
//
// The classes are carried by SENTINELS rather than by gRPC codes, so this table
// stays on the relayer's side of the transport. attestorgrpc maps codes onto
// them; a fake attestor in a test returns them directly; a future non-gRPC
// attestor maps its own failures the same way. The port stays testable with a
// small fake, which is why AttestorClient was defined consumer-side to begin with.

var (
	// ErrAttestorBadRequest: the attestor refused the REQUEST, not the block.
	// gRPC InvalidArgument. Cause is on this side — an input that is not 32 bytes,
	// a run_mode the daemon does not accept — so it is a relayer bug or a relayer
	// config error and retrying is pure waste. PERMANENT.
	ErrAttestorBadRequest = errors.New("attestor rejected the request as malformed")

	// ErrAttestorUnknownRoute: the attestor serves no such src_chain or resource.
	// gRPC NotFound, which since 04-Attestor B2 carries this ONE meaning — the
	// Nitro-has-not-seen-the-block case was moved to Unavailable precisely so this
	// code could be classified without inspecting the request. PERMANENT: the route
	// is misconfigured, and A6's startup checks are where that should have been
	// caught, not the first packet.
	ErrAttestorUnknownRoute = errors.New("attestor does not serve this source chain")

	// ErrAttestorReplicaBehind: the attestor's replica has not reached that height
	// yet. gRPC FailedPrecondition. TRANSIENT, and ordinary — it is the same
	// "not yet" as a RelayableHeight that has not caught up.
	ErrAttestorReplicaBehind = errors.New("attestor replica has not reached this height")

	// ErrAttestorUnavailable: the attestor could not be reached or could not
	// answer. gRPC Unavailable and DeadlineExceeded. TRANSIENT, and explicitly NOT
	// to be read as disagreement: an attestor that cannot answer has said nothing
	// about the block, and treating silence as a "no" would fail good headers
	// whenever the sidecar restarts.
	ErrAttestorUnavailable = errors.New("attestor is unavailable")

	// ErrAttestorUnimplemented: the daemon does not serve this RPC at all. gRPC
	// Unimplemented. After the cut-over this is an INVARIANT VIOLATION, not a
	// degradation: startup is supposed to reject an attestor that cannot sign, so
	// reaching a header build means a check did not run. PERMANENT, and alarmed.
	ErrAttestorUnimplemented = errors.New("attestor does not implement this RPC")

	// ErrAttestorDivergence: the attestor's replica does not recognise the block
	// the relayer is about to package. This is `valid=false`, not an RPC failure.
	//
	// TRANSIENT — the everyday cause is the replica trailing the L2 RPC by a block,
	// or a reorg it has not re-derived — but it is alarmed, because the same answer
	// is what a REAL divergence looks like, and retrying a real one in silence
	// hides the one condition the attestor exists to detect. The class says what to
	// do; the alarm says what to look at.
	ErrAttestorDivergence = errors.New("attestor replica disagrees about this block")

	// ErrAttestorSignature: the attestor answered valid=true and signed, but the
	// signature does not verify against the public key and head pinned in
	// rollup_profile.common.
	//
	// PERMANENT, and alarmed. Both of its causes are: either attestor_public_key
	// in the profile does not belong to the daemon actually answering -- a config
	// error no retry can fix -- or the daemon is signing something other than the
	// block identity it was asked about, which is the attestor failing at its one
	// job. Neither becomes true on the next flush, and retrying either in silence
	// leaves the path relaying nothing while looking merely slow.
	ErrAttestorSignature = errors.New("attestor signature does not verify against the pinned profile")
)

// ErrVerifyStateRootUnsupported is the VerifyStateRoot-specific face of
// ErrAttestorUnimplemented, kept as its own sentinel because callers already
// test for it by name. It wraps the general one, so errors.Is answers yes for
// both and the table below needs only the general row.
var ErrVerifyStateRootUnsupported = fmt.Errorf("%w: VerifyStateRoot", ErrAttestorUnimplemented)

// classifyAttestorFailure is the table. tag names the caller for the log line.
//
// The alarm is emitted HERE, not left to the two call sites, and that is
// deliberate: the doc's requirement for `valid=false` is "transient AND alarmed",
// and a requirement split across a returned class and a log line the caller must
// remember to write is a requirement that will be half-implemented the first time
// a third call site appears.
//
// An unrecognised failure is transient. That is Classify's default and the right
// one: the packet is kept, and the cost of being wrong is a retry rather than a
// locked escrow.
// tag names the work that hit the failure (Attest, Frontier, RelayableHeight);
// the direction is fixed, because only the L2->Cosmos path talks to an attestor.
func classifyAttestorFailure(tag string, err error) chain.Retry {
	switch {
	case errors.Is(err, ErrAttestorUnimplemented):
		log.Printf("ERROR [l2->cosmos %s][ATTENTION] the attestor does not implement an RPC this relayer requires: %v; "+
			"startup is supposed to reject such a daemon, so this relay path is running unverified — "+
			"check the attestor build matches the relayer", tag, err)
		return chain.Permanent(err)

	case errors.Is(err, ErrAttestorDivergence):
		log.Printf("WARN [l2->cosmos %s][ATTENTION] the attestor replica disagrees with the L2 RPC: %v; "+
			"this retries, but if it does not clear within a few blocks the two are genuinely diverged "+
			"and no header from this L2 RPC is attested state", tag, err)
		return chain.Transient(err)

	case errors.Is(err, ErrAttestorSignature):
		log.Printf("ERROR [l2->cosmos %s][ATTENTION] the attestor signature does not verify against the pinned profile: %v; "+
			"either rollup_profile.common.attestor_public_key is not this daemon's key, or the daemon is not "+
			"signing the block identity it was asked about — no retry resolves either", tag, err)
		return chain.Permanent(err)

	case errors.Is(err, ErrAttestorBadRequest):
		return chain.Permanent(err)

	case errors.Is(err, ErrAttestorUnknownRoute):
		return chain.Permanent(fmt.Errorf("%w; the configured source chain is not one this attestor serves — "+
			"fix the config rather than waiting, no retry can make the route appear", err))

	case errors.Is(err, ErrAttestorReplicaBehind), errors.Is(err, ErrAttestorUnavailable):
		return chain.Transient(err)
	}
	return chain.Transient(err)
}
