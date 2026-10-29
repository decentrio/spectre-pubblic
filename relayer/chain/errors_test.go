package chain

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// The failure catalog: every place a packet's journey can fail, what kind of
// failure that is, and the test that injects it there.
//
// C1 CLASSIFIES failures into three kinds. This ENUMERATES them. Without the
// second, C1's rule only applies where someone remembered it, and "we handle
// transient failures correctly" is a claim about the cases that came to mind.
//
// The catalog is the spec and the test suite is the gate, so the two are kept in
// one place: a row names the test that proves it, and TestFailureCatalog fails if
// that test does not exist. A row with no test at all has to say why in Owed, in
// writing, which is the difference between a known gap and an oversight.
//
// It lives in chain/ because the three classes do (errors.go, retry.go): the
// enumeration of what gets classified belongs beside the classification.
type failureClass string

const (
	// Retry unchanged. Does not count against a retry budget.
	classTransient failureClass = "transient"
	// Deterministic. Count it, dead-letter at the end, and log loudly.
	classPermanent failureClass = "permanent"
	// Retry, but only after CHANGING a quantity -- a smaller batch, a wider gas
	// limit, a narrower range. Retrying unchanged is an infinite loop, not a retry.
	classNeedsChange failureClass = "needs change"
	// Not a failure: the packet's turn has not come. Must not be charged to any
	// budget, and must not be logged as an error.
	classWaiting failureClass = "waiting"
)

type failureRow struct {
	Stage string
	Point string // what fails, at the code that fails
	Class failureClass
	Doing string // what the relayer must do about it
	// Test injects THIS failure at THIS point. "Parent/subtest name" is allowed.
	Test string
	// Owed replaces Test when no test can exist yet, and says what blocks it.
	Owed string
}

var failureCatalog = []failureRow{
	// ---- listening for events -------------------------------------------------
	{
		Stage: "listen", Point: "the Cosmos live subscription goes quiet",
		Class: classTransient,
		Doing: "reconnect, but only when the chain really stopped delivering -- a quiet chain is healthy",
		Test:  "TestLivePathStale",
	},
	{
		Stage: "listen", Point: "gap recovery fails while the live path is up",
		Class: classTransient,
		Doing: "do not advance the recovery cursor; the same range is rescanned next tick",
		Test:  "TestProcessLiveCosmosEvent/cursor survives a failed gap recovery",
	},
	{
		Stage: "listen", Point: "the provider refuses the ETH log range as too wide",
		Class: classNeedsChange,
		Doing: "halve the span and rescan from the same cursor",
		Test:  "TestScanEthRangeInChunks/narrows and resumes at the cursor",
	},
	{
		Stage: "listen", Point: "the provider refuses the L2 log range as too wide",
		Class: classNeedsChange,
		Doing: "halve the span and rescan the same range",
		Test:  "TestScanNarrowing/narrows when the provider refuses the span",
	},
	{
		Stage: "listen", Point: "the range cannot be narrowed any further",
		Class: classPermanent,
		Doing: "stop: one block is the smallest request, so a refusal there is not a sizing problem",
		Test:  "TestScanEthRangeInChunks/stops when the span cannot shrink further",
	},
	{
		Stage: "listen", Point: "a log scan fails for a reason that is not the range",
		Class: classTransient,
		Doing: "pass it through unnarrowed, and persist only the chunks that succeeded",
		Test:  "TestScanEthRangeInChunks/persists only successful chunks",
	},
	{
		Stage: "listen", Point: "an ordinary transport failure is mistaken for a range refusal",
		Class: classTransient,
		Doing: "do not narrow: shrinking for a reason unrelated to size ends at the ladder's bottom, reporting permanent for a reconnect",
		Test:  "TestScanNarrowing/does not narrow on an ordinary failure",
	},

	// ---- entering the queue ---------------------------------------------------
	{
		Stage: "enqueue", Point: "the durable pending-state write fails (#412)",
		Class: classTransient,
		Doing: "retry the event; it must never be counted as handled",
		Test:  "TestEnqueueEthSendPacket/retries when pending state cannot persist",
	},
	{
		Stage: "enqueue", Point: "a failed durable add would strand the recovery cursor",
		Class: classTransient,
		Doing: "keep the cursor where it was, so the event is seen again",
		Test:  "TestRecoverEthSendPackets/failed eth pending add keeps recovery cursor retryable",
	},
	{
		Stage: "enqueue", Point: "the relay engine cannot track a send durably",
		Class: classTransient,
		Doing: "re-queue the send and relay nothing: an untracked send has no timeout refund behind it",
		Test:  "TestHandleBatch/requeues send when pending tracking fails",
	},

	// ---- preconditions --------------------------------------------------------
	{
		Stage: "precondition", Point: "the packet's height is above what the source will prove",
		Class: classWaiting,
		Doing: "re-queue without building a proof or advancing the client",
		Test:  "TestHandleBatch/precondition skip",
	},
	{
		Stage: "precondition", Point: "the attestor has not reached the packet's height",
		Class: classWaiting,
		Doing: "report height 0 and wait; this is not an error and must not be logged as one",
		Test:  "TestRelayableHeight/not attested yet",
	},
	{
		Stage: "precondition", Point: "the attestor cannot be reached",
		Class: classTransient,
		Doing: "propagate; do not read an unreachable attestor as 'nothing attested'",
		Test:  "TestRelayableHeight/error propagates",
	},
	{
		Stage: "precondition", Point: "the attestor quorum can never be met for this height",
		Class: classPermanent,
		Doing: "say so rather than waiting forever on a quorum that cannot form",
		Test:  "TestQuorumAttestationFrontier_ClassifiesImpossiblePermanentQuorum",
	},

	// ---- building the client update -------------------------------------------
	{
		Stage: "build update", Point: "the update cannot be built or folded",
		Class: classTransient,
		Doing: "do not advance lastHeight or record the update: state must not move on a failure",
		Test:  "TestHandleBatch/fold failure does not advance state",
	},
	{
		Stage: "build update", Point: "the destination client cannot reach the packet's height",
		Class: classWaiting,
		Doing: "re-queue rather than prove against a state that lacks the packet",
		Test:  "TestHandleBatch/provability guard",
	},
	{
		Stage: "build update", Point: "the prover reports one of its six job states",
		Class: classTransient, // five of six; failed{internal} is permanent -- see the table on submitAndAwaitProof
		Doing: "classify per the error table on submitAndAwaitProof (relayer/services/proofclient.go); the relayer decides retries, the prover never does",
		Test:  "TestSubmitAndAwaitProof_RetryTable",
	},

	// ---- submitting the client update -----------------------------------------
	{
		Stage: "submit update", Point: "the update transaction runs out of gas",
		Class: classNeedsChange,
		Doing: "climb the gas ladder and resubmit at the rung it names",
		Test:  "TestExecuteWithRetryAndResubmission/out of gas escalates in process",
	},
	{
		Stage: "submit update", Point: "the gas ladder is exhausted",
		Class: classPermanent,
		Doing: "stop; more gas is not the answer once the finite headroom list ends",
		Test:  "TestExecuteWithRetryAndResubmission/out of gas ladder exhaustion is permanent",
	},
	{
		Stage: "submit update", Point: "the transaction sits at a nonce the account has not reached",
		Class: classTransient,
		Doing: "stop bumping -- more gas cannot mine a nonce with a gap under it -- and invalidate the cached nonce",
		Test:  "TestExecuteWithRetryAndResubmission/stuck future nonce stops bumping",
	},
	{
		Stage: "submit update", Point: "a Cosmos batch is too large to simulate or exceeds the block gas limit",
		Class: classNeedsChange,
		Doing: "split it in half and submit the halves in order, so the delivered count stays a prefix",
		Test:  "TestSplitCosmosBatchAfterDuplicateWith/around duplicate threads sequence and sums on success",
	},
	{
		Stage: "submit update", Point: "every retry ladder must actually change something and end",
		Class: classNeedsChange,
		Doing: "three calls, three different answers, and a bottom -- a one-rung ladder is an infinite loop",
		Test:  "TestLaddersChangeSomethingEachRungAndEnd",
	},

	// ---- building the packet proof --------------------------------------------
	{
		Stage: "prove packet", Point: "the commitment is absent at the proof height (3.8)",
		Class: classTransient,
		Doing: "report it as absent instead of proving zero; the height moves as the destination client advances",
		Test:  "TestGetEthMembershipProof",
	},
	{
		Stage: "prove packet", Point: "the L2 commitment slot is empty at the proof height (3.8)",
		Class: classTransient,
		Doing: "the same answer as the L1 path: one sentinel, so the class is asserted once",
		Test:  "TestL2StorageProof",
	},
	{
		Stage: "prove packet", Point: "the proof RPC never answers",
		Class: classTransient,
		Doing: "honour the caller's context: an unbounded proof call wedges the whole relay direction silently",
		Test:  "TestGetEthMembershipProof/honours context",
	},
	{
		Stage: "prove packet", Point: "the packet bytes do not decode",
		Class: classPermanent,
		Doing: "fail before dialing anything; a broken packet must not become a retry loop against the endpoint",
		Test:  "TestSource_MalformedPacketFailsBeforeAnyRPC",
	},
	{
		Stage: "prove packet", Point: "the packet's own timeout has already passed",
		Class: classPermanent,
		Doing: "drop it here and let the timeout scanner refund it; a receive can never be accepted after this",
		Test:  "TestSendPacketExpired",
	},
	{
		Stage: "prove packet", Point: "a proof was built at a height the destination client is not on",
		Class: classTransient,
		Doing: "refuse before sending; on chain this surfaces as an opaque trie error naming no height",
		Test:  "TestRequireProofsBuiltAt",
	},

	// ---- submitting the packets -----------------------------------------------
	{
		Stage: "submit packets", Point: "the batch commits part way and then fails (3.8)",
		Class: classTransient,
		Doing: "settle the delivered prefix and retry only the tail; re-queueing the head submits it twice",
		Test:  "TestHandleBatch/partial commit",
	},
	{
		Stage: "submit packets", Point: "one packet deterministically reverts a multi-packet batch",
		Class: classPermanent,
		Doing: "isolate: retry each alone so the poison drops by itself and its valid siblings still relay",
		Test:  "TestHandleBatch/isolates poison packet",
	},
	{
		Stage: "submit packets", Point: "an isolated packet fails transiently after the batch failed permanently",
		Class: classTransient,
		Doing: "re-queue that one; the batch's verdict is not each packet's verdict",
		Test:  "TestHandleBatch/isolation requeues transient",
	},
	{
		Stage: "submit packets", Point: "a single packet reverts deterministically",
		Class: classPermanent,
		Doing: "drop it and stop reporting a wait for it; the timeout scanner refunds it if it expires",
		Test:  "TestHandleBatch/drop permanent relay",
	},
	{
		Stage: "submit packets", Point: "the relay call fails for an infrastructure reason",
		Class: classTransient,
		Doing: "re-queue the whole batch: nothing landed",
		Test:  "TestHandleBatch/requeue transient relay",
	},

	// ---- keeping the client alive ---------------------------------------------
	{
		Stage: "refresh", Point: "an anti-expiry refresh attempt fails",
		Class: classTransient,
		Doing: "back off exponentially, capped far below the refresh margin, and say when the next attempt is",
		Test:  "TestRefreshLoop/backs off when refresh keeps failing",
	},
	{
		Stage: "refresh", Point: "the backoff outlives the outage that caused it",
		Class: classTransient,
		Doing: "a success clears it: otherwise a client that recovered last week waits the full cap for its next blip",
		Test:  "TestRefreshLoop/backoff does not outlive its cause",
	},

	// ---- reconciling ----------------------------------------------------------
	{
		Stage: "reconcile", Point: "the receipt check cannot answer (3.8)",
		Class: classTransient,
		Doing: "an error means DO NOT KNOW, never 'not delivered': stop rather than open the path to timing out a delivered packet",
		Test:  "TestDestination/refuses to answer HasPacketReceipt without a client",
	},
	{
		Stage: "reconcile", Point: "a timeout send fails for a reason that does not prove the packet's fate",
		Class: classTransient,
		Doing: "defer the retry instead of charging it; charging an RPC outage dead-letters an escrowed packet",
		Test:  "TestTimeoutOutcomeForError_DefersTransientFailures",
	},
	{
		Stage: "reconcile", Point: "a partial-batch failure loses the count of what landed",
		Class: classTransient,
		Doing: "read the delivered prefix out of the failure; it is the only thing that says how much to retry",
		Test:  "TestCommittedBeforeFailure",
	},

	// ---- shutting down --------------------------------------------------------
	{
		Stage: "shutdown", Point: "the run context is cancelled mid-batch",
		Class: classTransient,
		Doing: "re-queue everything and broadcast nothing: a transaction sent during teardown has no one left to observe it",
		Test:  "TestHandleBatch/cancelled context relays nothing",
	},
	{
		Stage: "shutdown", Point: "an adapter is asked for work after cancellation",
		Class: classTransient,
		Doing: "refuse at the adapter too, not only at the engine -- and at EVERY adapter, which is what the contract suite checks",
		Test:  "TestAdapterContract",
	},
	{
		Stage: "shutdown", Point: "the drain exceeds its bound",
		Class: classPermanent,
		Doing: "exit non-zero and name the worker that did not stop",
		Test:  "TestRun_ReportsASourceThatCouldNotStopItsOwnWorkers",
	},
}

// TestFailureCatalog is the gate. The catalog is only a spec while something
// checks it, and the check that matters is that each row's test exists: a
// renamed or deleted test otherwise leaves a row pointing at nothing, and the
// catalog quietly becomes a list of intentions.
func TestFailureCatalog(t *testing.T) {
	tests, subtests := repoTestNames(t)

	t.Run("every row is classified and says what to do", func(t *testing.T) {
		known := map[failureClass]bool{
			classTransient: true, classPermanent: true, classNeedsChange: true, classWaiting: true,
		}
		for _, row := range failureCatalog {
			if !known[row.Class] {
				t.Errorf("%s / %s: class %q is not one of the four", row.Stage, row.Point, row.Class)
			}
			if strings.TrimSpace(row.Doing) == "" {
				t.Errorf("%s / %s: a class without a behaviour does not tell anyone what to write", row.Stage, row.Point)
			}
		}
	})

	t.Run("every row names a test that exists, or says what blocks it", func(t *testing.T) {
		for _, row := range failureCatalog {
			switch {
			case row.Test == "" && row.Owed == "":
				t.Errorf("%s / %s: no test and no reason; a failure point with neither is an oversight, not a gap",
					row.Stage, row.Point)
				continue // saying it twice does not say it better
			case row.Test != "" && row.Owed != "":
				t.Errorf("%s / %s: both a test and an Owed reason -- one of the two is stale", row.Stage, row.Point)
			case row.Test == "":
				continue // owed, with a reason
			}

			parent, sub, hasSub := strings.Cut(row.Test, "/")
			if !tests[parent] {
				t.Errorf("%s / %s: names %q, which no test file defines", row.Stage, row.Point, parent)
				continue
			}
			if hasSub && !subtests[parent][sub] {
				t.Errorf("%s / %s: names subtest %q of %s, which that test does not declare",
					row.Stage, row.Point, sub, parent)
			}
		}
	})

	t.Run("no two rows lean on the same test", func(t *testing.T) {
		// One point, one test. Two rows sharing one test means at least one of
		// them is not actually injected anywhere -- the shape this catalog exists
		// to make visible.
		seen := map[string]failureRow{}
		for _, row := range failureCatalog {
			if row.Test == "" {
				continue
			}
			if first, dup := seen[row.Test]; dup {
				t.Errorf("%q covers both %q and %q; at least one of the two is not injected on its own",
					row.Test, first.Point, row.Point)
				continue
			}
			seen[row.Test] = row
		}
	})
}

var (
	testFuncRe = regexp.MustCompile(`(?m)^func (Test[A-Za-z0-9_]+)\(`)
	subtestRe  = regexp.MustCompile(`t\.Run\("([^"]+)"`)
)

// repoTestNames collects every Test function in the module and, FOR EACH ONE,
// the literal t.Run names declared inside it. Reading the files is deliberate:
// asking the toolchain would only see this package, and the catalog spans all of
// them.
//
// The subtests are keyed by parent, and that is the whole point. Reported by
// @DongLieu: they used to be one flat set for the module, so a row naming
// "TestDestination/a populated slot produces a proof" passed as long as SOME
// test declared that subtest -- even a different one, in a different package.
// The gate exists to fail when a test is renamed or deleted, and a check that
// accepts any subtest anywhere cannot do that. He demonstrated it by rewriting a
// row to a subtest of an unrelated parent; the gate stayed green.
//
// A parent is whatever Test function the t.Run textually sits below, which is
// what a reader would say too. Nested t.Run attributes to the same parent: a
// catalog row naming a nested case is still naming that test.
func repoTestNames(t *testing.T) (tests map[string]bool, subtests map[string]map[string]bool) {
	t.Helper()
	tests, subtests = map[string]bool{}, map[string]map[string]bool{}

	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatalf("resolve module root: %v", err)
	}
	err = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			// Generated bindings carry no tests and are large.
			if name := d.Name(); name == "bindings" || name == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, "_test.go") {
			return nil
		}
		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		// One pass in source order, so each t.Run is attributed to the Test
		// function above it. Two passes over independent match lists is what lost
		// the association in the first place.
		body := string(src)
		type mark struct {
			at     int
			name   string
			parent bool
		}
		marks := make([]mark, 0, 64)
		for _, m := range testFuncRe.FindAllStringSubmatchIndex(body, -1) {
			marks = append(marks, mark{at: m[0], name: body[m[2]:m[3]], parent: true})
		}
		for _, m := range subtestRe.FindAllStringSubmatchIndex(body, -1) {
			marks = append(marks, mark{at: m[0], name: body[m[2]:m[3]]})
		}
		sort.Slice(marks, func(i, j int) bool { return marks[i].at < marks[j].at })

		current := ""
		for _, mk := range marks {
			if mk.parent {
				current = mk.name
				tests[current] = true
				if subtests[current] == nil {
					subtests[current] = map[string]bool{}
				}
				continue
			}
			if current == "" {
				continue // a t.Run above every Test function: a helper, not a case
			}
			subtests[current][mk.name] = true
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	if len(tests) == 0 {
		t.Fatalf("found no test functions under %s; the walk is looking in the wrong place", root)
	}
	return tests, subtests
}
