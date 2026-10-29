package services

import (
	"errors"
	"fmt"
	"os"
	"regexp"
	client "relayer/client"
	relayerclient "relayer/client"
	"strings"
	"testing"
	"time"

	channeltypesv2 "github.com/cosmos/ibc-go/v10/modules/core/04-channel/v2/types"
)

func trackedPacket(seq uint64) channeltypesv2.Packet {
	return channeltypesv2.Packet{SourceClient: "client-0", Sequence: seq}
}

// TestNextTimeoutBackoff_GrowsAndCaps pins the schedule: 1m, 2m, 4m … capped at
// 30m. Without it a deterministically-failing timeout was re-attempted on the raw
// 30s scan cadence, and every attempt costs a Groth16 proof and a transaction.
func TestNextTimeoutBackoff_GrowsAndCaps(t *testing.T) {
	want := []time.Duration{
		1 * time.Minute, 2 * time.Minute, 4 * time.Minute, 8 * time.Minute,
		16 * time.Minute, 30 * time.Minute, 30 * time.Minute,
	}
	for i, w := range want {
		if got := nextTimeoutBackoff(i + 1); got != w {
			t.Fatalf("attempt %d: backoff = %s, want %s", i+1, got, w)
		}
	}
	// A very large attempt must still clamp rather than overflow the shift.
	if got := nextTimeoutBackoff(64); got != timeoutRetryBackoffMax {
		t.Fatalf("attempt 64: backoff = %s, want the %s cap (shift overflow guard)", got, timeoutRetryBackoffMax)
	}
}

// TestDeadLetteredTimeouts_LettersRemainVisibleUntilRetryRetentionExpires ensures a read-only
// inspection does not clear the gauge while the timeout remains suppressed.
func TestDeadLetteredTimeouts_LettersRemainVisibleUntilRetryRetentionExpires(t *testing.T) {
	tracker, packet := trackerWith(t, 1)
	deadAt := time.Now()
	for i := 0; i <= maxTimeoutAttempts; i++ {
		tracker.recordTimeoutFailureForTest(packet.SourceClient, packet.Sequence, deadAt)
	}
	if got := tracker.DeadLetteredTimeouts(); got != 1 {
		t.Fatalf("expected one dead-lettered timeout, got %d", got)
	}
	packets := tracker.DeadLetteredTimeoutPackets()
	if len(packets) != 1 {
		t.Fatalf("dead-lettered packets=%d, want 1", len(packets))
	}
	if tracker.deadLettered[0].DeadLetteredAt.IsZero() {
		t.Fatal("DeadLetteredAt must be stamped so the age is observable")
	}
	if got := tracker.DeadLetteredTimeouts(); got != 1 {
		t.Fatalf("inspection must not clear the timeout dead-letter gauge, got %d", got)
	}
}

// OldestDeadLetteredTimeout reports zero with nothing dead-lettered and a real
// age once something is — a count alone cannot distinguish a failure nobody has
// looked at from one that just happened.
func TestOldestDeadLetteredTimeout_LetterReportsAge(t *testing.T) {
	tracker := NewPendingPacketTracker()
	if age := tracker.OldestDeadLetteredTimeout(time.Now()); age != 0 {
		t.Fatalf("empty queue must report zero age, got %s", age)
	}

	packet := trackedPacket(1)
	tracker.Add(packet, 1)
	deadAt := time.Now()
	for i := 0; i <= maxTimeoutAttempts; i++ {
		tracker.recordTimeoutFailureForTest(packet.SourceClient, packet.Sequence, deadAt)
	}
	if age := tracker.OldestDeadLetteredTimeout(deadAt.Add(time.Hour)); age < time.Hour {
		t.Fatalf("timeout dead-letter age = %s, want at least an hour", age)
	}
}

// A sequence is not an identity. packetIdentity pairs it with the source client
// precisely because a sequence is reused across clients, and after A1 a merged
// operator log carries several of them.
//
// The two lines that matter most are the ones telling an operator to act --
// STUCK and ATTENTION, both of which say funds are escrowed. Naming a sequence
// there without its client tells them something is wrong and not which packet
// it is. The rest of the scanner's lines are the trail leading to those two, so
// the rule covers the whole family rather than the two endpoints.
//
// Structural rather than behavioural: asserting on captured log output would pin
// the wording, and the invariant is about what the line identifies, not how it
// reads.
func TestTimeoutScanLogsIdentifyThePacketNotJustTheSequence(t *testing.T) {
	var offenders []string
	for _, file := range []string{"services.go", "timeout_retry.go", "pending.go", "batch.go"} {
		src, err := os.ReadFile(file)
		if err != nil {
			t.Fatalf("read %s: %v", file, err)
		}
		for i, line := range strings.Split(string(src), "\n") {
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "//") || !strings.Contains(line, "TimeoutScan]") {
				continue
			}
			if !strings.Contains(line, "seq=%d") || strings.Contains(line, "src=%s") {
				continue
			}
			offenders = append(offenders, fmt.Sprintf("%s:%d: %s", file, i+1, trimmed))
		}
	}
	if len(offenders) > 0 {
		t.Fatalf("timeout-scan log lines naming a sequence with no source client:\n%s\n\n"+
			"An operator reading a merged log cannot tell which client's packet this is.",
			strings.Join(offenders, "\n"))
	}
}

// A tag is interpolated into "[%sTimeoutScan]", so the value passed must be the
// CHAIN, not a name that already ends in Timeout.
//
// Found while merging main into #436: services.go passed "CosmosTimeout", which
// rendered as [CosmosTimeoutTimeoutScan] -- a fourth name for the scanner this
// branch exists to give one name. The label gate in cmd cannot see it, because
// the format string there is correct and only the argument is wrong.
func TestTimeoutScanTagsAreChainNamesNotWorkNames(t *testing.T) {
	src, err := os.ReadFile("services.go")
	if err != nil {
		t.Fatalf("read services.go: %v", err)
	}
	tagRe := regexp.MustCompile(`(?:tag:\s*|chargeTimeoutFailure\([^"]*)"([A-Za-z0-9]+)"`)
	for i, line := range strings.Split(string(src), "\n") {
		for _, m := range tagRe.FindAllStringSubmatch(line, -1) {
			if strings.Contains(m[1], "Timeout") || strings.Contains(m[1], "Scan") {
				t.Errorf("services.go:%d: tag %q becomes [%sTimeoutScan]; pass the chain alone",
					i+1, m[1], m[1])
			}
		}
	}
}

// TestTimeoutOutcomeForError_DefersTransientFailures is the second half of the
// "only charge what the packet caused" rule, on the per-packet send path rather
// than the shared client-update one.
//
// SendEthTx returns untyped errors for everything before a transaction exists
// (nonce lookup, fee estimation, broadcast) and for a receipt wait that timed
// out after the transaction may already have been accepted. Charging those let
// an RPC outage burn all maxTimeoutAttempts and dead-letter an escrowed packet
// whose timeout transaction was never confirmed — the property this change set
// out to remove, reappearing one layer down.
func TestTimeoutOutcomeForError_DefersTransientFailures(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		err  error
		want timeoutSendOutcome
	}{
		{"nonce lookup failed", errors.New("failed to get pending nonce"), timeoutDeferred},
		{"broadcast failed", errors.New("dial tcp: connection refused"), timeoutDeferred},
		{"receipt wait timed out", errors.New("transaction wait mined timed out after 5 attempts"), timeoutDeferred},
		{"rate limited", errors.New("429 Too Many Requests"), timeoutDeferred},

		{"on-chain revert is attributable", ErrPermanentRelayFailure, timeoutFailed},
		{"wrapped permanent failure", fmt.Errorf("tx reverted: %w", ErrPermanentRelayFailure), timeoutFailed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := timeoutOutcomeForError(tc.err); got != tc.want {
				t.Fatalf("timeoutOutcomeForError(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// A proof build fails in several ways, and only one of them is evidence about
// the packet. Getting this split wrong is expensive in both directions: charge
// an endpoint's failure and an escrowed packet is dead-lettered for someone
// else's downtime; defer a packet that was already received and the scanner
// rebuilds a proof that can never succeed, forever, at the 30-minute cap.
func TestTimeoutOutcomeForError_ChargesOnlyPacketAttributableProofFailures(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		err  error
		want timeoutSendOutcome
	}{
		{
			"receipt slot already holds a value",
			fmt.Errorf("failed to get ETH non-membership proof: storage slot not empty at key 0xabc: value=0x1: %w",
				relayerclient.ErrPacketAlreadyReceived),
			// Delivered, not failed: it leaves the tracker without charging.
			timeoutAlreadyReceived,
		},
		{"eth_getProof transport error", errors.New("eth_getProof failed: connection refused"), timeoutDeferred},
		{"answer carried no storage proof", errors.New("eth_getProof returned no storage proofs"), timeoutDeferred},
		{"proof answered at the wrong height", errors.New("proof height mismatch"), timeoutDeferred},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := timeoutOutcomeForError(tc.err); got != tc.want {
				t.Fatalf("timeoutOutcomeForError(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// The ETH direction's mirror of TestPendingPacketTracker/received packet is dropped not dead lettered.
//
// scanForEthTimeouts checks the ETH commitment, then builds a Cosmos
// non-membership proof. When the receipt landed between those two steps the
// proof cannot be built -- and that is not a failure, it is delivery. Reading it
// as a generic proof error defers the packet at the backoff cap forever and ends
// in a STUCK alert about a packet whose funds moved normally.
//
// This was a one-sided fix: the Cosmos scanner recognised the sentinel and the
// ETH scanner did not.
func TestPacketIsDone_EthTimeoutTreatsAReceivedPacketAsDone(t *testing.T) {
	t.Parallel()

	received := fmt.Errorf("build non-membership proof: %w", relayerclient.ErrPacketAlreadyReceived)

	// Built by the production path, not hand-assembled: this is what catches the
	// sentinel being dropped from cosmosNonMembership's receipt check.
	if !errors.Is(cosmosReceiptPresentError(1234, 32), relayerclient.ErrPacketAlreadyReceived) {
		t.Fatal("a Cosmos receipt at the non-membership path must carry ErrPacketAlreadyReceived; " +
			"without it the packet is deferred forever and raises a false STUCK alert")
	}

	got := timeoutOutcomeForError(received)
	if got != timeoutAlreadyReceived {
		t.Fatalf("timeoutOutcomeForError(already-received) = %v, want timeoutAlreadyReceived", got)
	}
	if !got.packetIsDone() {
		t.Fatal("an already-received packet must leave the tracker, not be retried")
	}
	if got == timeoutFailed {
		t.Fatal("an already-received packet must not charge the retry budget")
	}

	// The outcomes that are genuinely unfinished must not claim to be done.
	for _, o := range []timeoutSendOutcome{timeoutNotDue, timeoutDeferred, timeoutFailed} {
		if o.packetIsDone() {
			t.Fatalf("outcome %v reported done; the packet still needs timeout work", o)
		}
	}
	if !timeoutSent.packetIsDone() {
		t.Fatal("a landed timeout must be done")
	}
}

func TestApplyTimeoutOutcome_DistinguishesDeferralFailureAndCompletion(t *testing.T) {
	tracker := NewPendingPacketTracker()
	now := time.Now()
	packet := timeoutRetryPacket(2)
	tracker.Add(packet, 10)
	info := tracker.GetAll()[0]

	applyTimeoutOutcome(tracker, info, timeoutDeferred, now, "test")
	if due := tracker.GetDue(now); len(due) != 0 {
		t.Fatalf("deferred packet remained due: %d", len(due))
	}
	applyTimeoutOutcome(tracker, info, timeoutNotDue, now, "test")
	if got := tracker.GetAll()[0].Deferrals; got != 2 {
		t.Fatalf("not-due packet deferrals = %d, want 2", got)
	}
	if due := tracker.GetDue(now); len(due) != 0 {
		t.Fatalf("not-due packet remained immediately eligible: %d", len(due))
	}
	applyTimeoutOutcome(tracker, info, timeoutAlreadyReceived, now, "test")
	if tracker.Len() != 0 {
		t.Fatal("already-received packet remained in timeout tracker")
	}

	if got := timeoutOutcomeForError(client.ErrPacketAlreadyReceived); got != timeoutAlreadyReceived {
		t.Fatalf("already received outcome = %d", got)
	}
	if got := timeoutOutcomeForError(ErrPermanentRelayFailure); got != timeoutFailed {
		t.Fatalf("permanent failure outcome = %d", got)
	}
	if got := timeoutOutcomeForError(errors.New("rpc unavailable")); got != timeoutDeferred {
		t.Fatalf("transient failure outcome = %d", got)
	}
}
