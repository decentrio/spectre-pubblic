// Package chaintest is the contract suite every chain adapter must pass.
//
// The dependency direction proves that relay/ does not know which chain it is
// driving. It does NOT prove the other half of "a new chain is a new package":
// that the new package keeps the promises relay/ relies on. Someone writing the
// fourth adapter reads chain.go, misreads one sentence, and nothing goes red.
//
// This is the missing half. One suite, written against the interfaces, run by
// every adapter on its own fixtures. Adding an adapter is then a package plus a
// three-line contract_test.go -- and breaking one of the promises documented in
// chain.go is red in that adapter, not in a review comment.
//
// It asserts what holds for an adapter with NO endpoints wired, which is what
// lets one suite cover three packages whose fakes have nothing in common. That
// sounds narrow and is not: an unwired adapter is exactly the shape that reveals
// whether a method answers when it cannot know, and whether it validates before
// it dials. Promises that need a live endpoint (RelayableHeight <= LatestHeight,
// the re-queue index contract of Subscribe) are named in Owed at the bottom.
package chaintest

import (
	"context"
	"testing"

	channeltypesv2 "github.com/cosmos/ibc-go/v10/modules/core/04-channel/v2/types"

	"relayer/chain"
)

// SourceFixture is what one adapter hands the suite.
type SourceFixture struct {
	Name string
	// Unwired is the adapter CONSTRUCTED but with no endpoints behind it: no RPC
	// client, no attestor. Anything the constructor sets (the chain type) is set;
	// anything that needs a network is not.
	//
	// Its methods must refuse rather than answer, and must refuse BEFORE reaching
	// for a client -- reaching panics on the nil, which is how the suite detects
	// validation that happens too late to be free.
	Unwired chain.Source
}

// DestinationFixture is the Destination half of the same idea.
type DestinationFixture struct {
	Name    string
	Unwired chain.Destination
}

// RunSourceContract runs every Source promise that holds without endpoints.
func RunSourceContract(t *testing.T, f SourceFixture) {
	t.Helper()
	t.Run(f.Name+"/source", func(t *testing.T) {
		src := f.Unwired

		t.Run("identifies its chain", func(t *testing.T) {
			if src.Chain() == "" {
				t.Fatal("Chain() is empty: the module labels logs and picks codecs by it")
			}
		})

		// Packet bytes that cannot decode are permanently broken. Discovering that
		// after a round trip turns a dead packet into a retry loop against the
		// endpoint and buries the real cause under a timeout.
		t.Run("rejects undecodable packet bytes before dialing", func(t *testing.T) {
			garbage := []byte{0xff, 0xff, 0xff, 0xff}

			t.Run("MembershipProof", func(t *testing.T) {
				err := mustNotPanic(t, "MembershipProof", func() error {
					_, err := src.MembershipProof(context.Background(), garbage, 10, chain.SendPacket)
					return err
				})
				if err == nil {
					t.Fatal("undecodable packet bytes accepted")
				}
			})

			t.Run("NonMembershipProof", func(t *testing.T) {
				err := mustNotPanic(t, "NonMembershipProof", func() error {
					_, err := src.NonMembershipProof(context.Background(), garbage, 10)
					return err
				})
				if err == nil {
					t.Fatal("undecodable packet bytes accepted")
				}
			})
		})

		// eventType selects which ICS-24 path is proven. An unknown one has no
		// path, so proving anything for it would prove the wrong thing.
		t.Run("rejects an event type it does not prove", func(t *testing.T) {
			err := mustNotPanic(t, "MembershipProof", func() error {
				_, err := src.MembershipProof(context.Background(), marshalPacket(t), 10, chain.EventType(0x7f))
				return err
			})
			if err == nil {
				t.Fatal("an unknown event type was accepted; some ICS-24 path was chosen for it")
			}
		})
	})
}

// RunDestinationContract runs every Destination promise that holds without
// endpoints.
func RunDestinationContract(t *testing.T, f DestinationFixture) {
	t.Helper()
	t.Run(f.Name+"/destination", func(t *testing.T) {
		dst := f.Unwired

		t.Run("identifies its chain", func(t *testing.T) {
			if dst.Chain() == "" {
				t.Fatal("Chain() is empty: the module labels logs and picks codecs by it")
			}
		})

		// A batch can empty out after filtering. Failing a relay cycle that had
		// nothing wrong with it is worse than doing nothing.
		t.Run("an empty batch is a no-op, not an error", func(t *testing.T) {
			committed, err := mustNotPanic2(t, "RelayPackets", func() (int, error) {
				return dst.RelayPackets(context.Background(), nil)
			})
			if err != nil {
				t.Fatalf("empty batch returned %v", err)
			}
			if committed != 0 {
				t.Fatalf("committed = %d for an empty batch, want 0", committed)
			}
		})

		// Nothing may be submitted once the run context is cancelled: a
		// transaction broadcast during teardown has no one left to observe it, and
		// the tracker never learns whether it landed.
		t.Run("refuses to submit after cancellation", func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			packets := []chain.RelayPacket{{Type: chain.SendPacket, Packet: marshalPacket(t)}}

			committed, err := mustNotPanic2(t, "RelayPackets", func() (int, error) {
				return dst.RelayPackets(ctx, packets)
			})
			if err == nil {
				t.Error("RelayPackets started a packet tx during shutdown")
			}
			if committed != 0 {
				t.Errorf("committed = %d after refusing to submit, want 0", committed)
			}
		})

		// The rule this exists for: an error means DO NOT KNOW, never "not
		// delivered". Reading a failure as "no receipt" is the open path to proving
		// a timeout for a packet that was in fact delivered.
		t.Run("cannot answer the receipt check without an endpoint", func(t *testing.T) {
			var received bool
			err := mustNotPanic(t, "HasPacketReceipt", func() error {
				var err error
				received, err = dst.HasPacketReceipt(context.Background(), marshalPacket(t))
				return err
			})
			if err == nil {
				t.Fatal("an unwired receipt check answered instead of erroring; 'false, nil' reads as 'safe to time out'")
			}
			if received {
				t.Fatal("reported a receipt AND an error; the caller cannot use either")
			}
		})

		t.Run("refuses a client update after cancellation", func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			err := mustNotPanic(t, "UpdateClient", func() error {
				return dst.UpdateClient(ctx, "client-0", chain.ClientUpdate{Height: 9, Payloads: [][]byte{{1}}})
			})
			if err == nil {
				t.Error("UpdateClient started a client-update tx during shutdown")
			}
		})
	})
}

// mustNotPanic reports a panic as a contract violation rather than letting it
// take the run down. On an unwired adapter a panic means the method reached for
// a client before validating its input -- which on a WIRED adapter is a round
// trip spent discovering something it already knew.
func mustNotPanic(t *testing.T, method string, call func() error) (err error) {
	t.Helper()
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("%s panicked on an unwired adapter (%v): it reached for a client before validating its input", method, r)
		}
	}()
	return call()
}

func mustNotPanic2(t *testing.T, method string, call func() (int, error)) (n int, err error) {
	t.Helper()
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("%s panicked on an unwired adapter (%v): it reached for a client before validating its input", method, r)
		}
	}()
	return call()
}

func marshalPacket(t *testing.T) []byte {
	t.Helper()
	pkt := channeltypesv2.Packet{
		Sequence:          7,
		SourceClient:      "client-src",
		DestinationClient: "client-dst",
		TimeoutTimestamp:  1 << 40, // far future: not the timed-out path
		Payloads: []channeltypesv2.Payload{{
			SourcePort:      "transfer",
			DestinationPort: "transfer",
			Version:         "ics20-1",
			Encoding:        "application/x-solidity-abi",
			Value:           []byte{0x01},
		}},
	}
	raw, err := pkt.Marshal()
	if err != nil {
		t.Fatalf("marshal packet: %v", err)
	}
	return raw
}

// Owed, and deliberately not faked into existence here:
//
//   - RelayableHeight <= LatestHeight, and Subscribe re-queueing exactly the
//     indices the handler returned. Both need a wired endpoint, and a fake shared
//     across three adapters whose endpoints have nothing in common would be a
//     fourth implementation of each chain rather than a test of the three.
//   - ClientExpiresAt's zero-means-no-expiry, for the same reason.
//   - "committed never exceeds the batch": asserting it means submitting, which
//     an unwired adapter cannot do. chain/cosmos covers it directly instead
//     (TestDestination/reports the prefix a split batch committed).
//
// They belong in a Wired fixture each adapter supplies, once there is a stub for
// all three endpoint families. Naming them here is the point: the suite says what
// it does not cover, so a reader does not mistake passing it for conformance.
