package cosmos

import (
	"context"
	"encoding/json"
	"errors"
	"relayer/chain"
	"relayer/chain/ethnative/spectrepb"
	relayerclient "relayer/client"
	"relayer/services"
	"strings"
	"testing"
	"time"

	clienttypes "github.com/cosmos/ibc-go/v10/modules/core/02-client/types"
	channeltypesv2 "github.com/cosmos/ibc-go/v10/modules/core/04-channel/v2/types"
)

type atomicTestTxHandler struct{ services.TransactionHandler }

// partialTestTxHandler returns whatever a split batch would: a plain failure, or
// a partial one carrying how many messages landed first.
type partialTestTxHandler struct {
	services.TransactionHandler
	err error
}

func (h *partialTestTxHandler) SendCosmosTxBatch(context.Context, services.CosmosEndpoint, []any) error {
	return h.err
}

func (*atomicTestTxHandler) SendCosmosTxBatchAtomic(context.Context, services.CosmosEndpoint, []any) error {
	return nil
}

func TestSupportsUpdatePacketFolding_RequiresAtomicSender(t *testing.T) {
	withoutAtomic := &Destination{worker: services.NewWorker(&struct{ services.TransactionHandler }{}, nil)}
	if withoutAtomic.SupportsUpdatePacketFolding() {
		t.Fatal("handler without no-split submission must not advertise folding")
	}
	withAtomic := &Destination{worker: services.NewWorker(&atomicTestTxHandler{}, nil)}
	if !withAtomic.SupportsUpdatePacketFolding() {
		t.Fatal("handler with no-split submission must advertise folding")
	}
}

// mustBeaconHeader returns the JSON of a beacon header that carries only what the
// destination reads and what converting it to the native Header requires.
func mustBeaconHeader(t *testing.T, signatureSlot string) []byte {
	t.Helper()
	beacon := func(slot string) relayerclient.BeaconBlockHeader {
		return relayerclient.BeaconBlockHeader{Slot: slot, ProposerIndex: "0"}
	}
	payload, err := json.Marshal(relayerclient.EthereumHeader{
		ActiveSyncCommittee: relayerclient.ActiveSyncCommittee{Current: &relayerclient.SyncCommittee{}},
		ConsensusUpdate: relayerclient.LightClientUpdate{
			SignatureSlot:   signatureSlot,
			AttestedHeader:  relayerclient.LightClientHeader{Beacon: beacon("0")},
			FinalizedHeader: relayerclient.LightClientHeader{Beacon: beacon(signatureSlot)},
		},
	})
	if err != nil {
		t.Fatalf("marshal header: %v", err)
	}
	return payload
}

// testProofJSON is a storage proof in the relayer's JSON form, enough to convert.
const testProofJSON = `{"account_proof":{"storage_root":"0x` +
	"0000000000000000000000000000000000000000000000000000000000000000" +
	`","proof":["0x01"]},"storage_proof":{"key":"0x` +
	"0000000000000000000000000000000000000000000000000000000000000001" +
	`","value":"0x2a","proof":["0x02"]}}`

func TestHighestBeaconSignatureSlot(t *testing.T) {
	t.Run("uses latest header", func(t *testing.T) {
		slot, err := highestBeaconSignatureSlot([][]byte{
			mustBeaconHeader(t, "123"),
			mustBeaconHeader(t, "130"),
			mustBeaconHeader(t, "127"),
		})
		if err != nil {
			t.Fatalf("highest slot: %v", err)
		}
		if slot != 130 {
			t.Fatalf("slot = %d, want 130", slot)
		}
	})

	t.Run("rejects malformed payload", func(t *testing.T) {
		if _, err := highestBeaconSignatureSlot([][]byte{[]byte("not-json")}); err == nil {
			t.Fatal("malformed header must fail")
		}
	})

	t.Run("rejects invalid slot", func(t *testing.T) {
		if _, err := highestBeaconSignatureSlot([][]byte{mustBeaconHeader(t, "not-a-slot")}); err == nil {
			t.Fatal("invalid signature slot must fail")
		}
	})
}

func TestHighestBeaconFinalizedSlot_UsesLatestHeader(t *testing.T) {
	slot, err := highestBeaconFinalizedSlot([][]byte{
		mustBeaconHeader(t, "123"),
		mustBeaconHeader(t, "130"),
		mustBeaconHeader(t, "127"),
	})
	if err != nil {
		t.Fatalf("highest finalized slot: %v", err)
	}
	if slot != 130 {
		t.Fatalf("finalized slot = %d, want 130", slot)
	}
}

func TestBuildPacketMessages_UsesFoldedUpdateTargetSlot(t *testing.T) {
	pkt := channeltypesv2.Packet{Sequence: 7, SourceClient: "eth-router-0", DestinationClient: "08-wasm-0"}
	raw, err := pkt.Marshal()
	if err != nil {
		t.Fatalf("marshal packet: %v", err)
	}
	wantHeight := clienttypes.Height{RevisionNumber: 0, RevisionHeight: 130}
	msgs, err := buildPacketMessages("cosmos1signer", wantHeight, []chain.RelayPacket{{
		Type: chain.SendPacket, Packet: raw, Proof: []byte(testProofJSON), Height: 999,
	}})
	if err != nil {
		t.Fatalf("build packet messages: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("message count = %d, want 1", len(msgs))
	}
	msg, ok := msgs[0].(*channeltypesv2.MsgRecvPacket)
	if !ok {
		t.Fatalf("message type = %T, want *MsgRecvPacket", msgs[0])
	}
	if msg.ProofHeight != wantHeight {
		t.Fatalf("proof height = %v, want %v", msg.ProofHeight, wantHeight)
	}
}

// --- chain.Destination contract: the guards that run before any RPC ---
//
// These mirror the set chain/evm/destination_test.go already has. The Cosmos side
// had tests for its helper functions but none for the interface methods
// themselves, so every early-return below — the ones that decide whether a
// transaction is attempted at all — was unguarded.

// cancelledCtx returns a context that is already done, standing in for shutdown.
func cancelledCtx() context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return ctx
}

// Beacon headers must be converted one-for-one and stay in order: the light client
// applies them sequentially, and a dropped or reordered header leaves it at a slot
// nobody chose. Order is the property worth pinning here — length alone would pass
// a batch that shuffled.
//
// A payload that is not a header the native module could take is refused here, before
// anything is signed: converting it is deterministic, so it would never convert later
// either. What the header MEANS is checked on chain.
func TestBuildBeaconUpdateMessages_WrapsEveryPayloadInOrder(t *testing.T) {
	payloads := [][]byte{mustBeaconHeader(t, "101"), mustBeaconHeader(t, "102"), mustBeaconHeader(t, "103")}

	msgs, err := buildBeaconUpdateMessages("signer-1", "client-0", payloads)
	if err != nil {
		t.Fatalf("wrap payloads: %v", err)
	}
	if len(msgs) != len(payloads) {
		t.Fatalf("got %d messages for %d payloads", len(msgs), len(payloads))
	}
	for i, m := range msgs {
		msg, ok := m.(*clienttypes.MsgUpdateClient)
		if !ok {
			t.Fatalf("message %d is %T, want *MsgUpdateClient", i, m)
		}
		if msg.ClientId != "client-0" || msg.Signer != "signer-1" {
			t.Fatalf("message %d carries client=%q signer=%q", i, msg.ClientId, msg.Signer)
		}
		if msg.ClientMessage.TypeUrl != "/ibc.lightclients.spectre.v1.Header" {
			t.Fatalf("message %d carries %q, want the native Header", i, msg.ClientMessage.TypeUrl)
		}
		var header spectrepb.Header
		if err := header.Unmarshal(msg.ClientMessage.Value); err != nil {
			t.Fatalf("message %d: unmarshal header: %v", i, err)
		}
		if want := uint64(101 + i); header.ConsensusUpdate.SignatureSlot != want {
			t.Fatalf("message %d carries signature slot %d, want %d (order must be preserved)",
				i, header.ConsensusUpdate.SignatureSlot, want)
		}
	}

	if _, err := buildBeaconUpdateMessages("signer-1", "client-0", [][]byte{[]byte("not json")}); err == nil {
		t.Fatal("a payload that is not a header was wrapped")
	}
}

// mustMarshalCosmosPacket returns proto bytes for a minimal ETH-origin packet.
func mustMarshalCosmosPacket(t *testing.T) []byte {
	t.Helper()
	pkt := channeltypesv2.Packet{Sequence: 7, SourceClient: "eth-router-0", DestinationClient: "08-wasm-0"}
	raw, err := pkt.Marshal()
	if err != nil {
		t.Fatalf("marshal packet: %v", err)
	}
	return raw
}

// The chain.Destination contract on the Cosmos side: the guards that run before
// any RPC, and decide whether a transaction is attempted at all.
//
// The name is TestDestination, not TestCosmosDestination: E9 requires the prefix
// to be a production identifier, and the type here is Destination. chain/evm has
// a Destination too, so the two test names collide across packages -- which the
// plan calls correct, not a clash: same unit, same name.
func TestDestination(t *testing.T) {
	// Nothing may be submitted once the run context is cancelled. A transaction
	// started during shutdown is broadcast with no one left to observe the result:
	// the process exits, and the tracker never learns whether it landed.
	t.Run("refuses work after shutdown", func(t *testing.T) {
		d := &Destination{}
		packets := []chain.RelayPacket{{Type: chain.SendPacket, Packet: mustMarshalCosmosPacket(t)}}

		if _, err := d.RelayPackets(cancelledCtx(), packets); err == nil {
			t.Error("RelayPackets started a packet tx during shutdown")
		}
		if err := d.UpdateClient(cancelledCtx(), "", chain.ClientUpdate{Payloads: [][]byte{{1}}}); err == nil {
			t.Error("UpdateClient started a client-update tx during shutdown")
		}
		if err := d.RelayWithUpdate(cancelledCtx(), "", chain.ClientUpdate{Payloads: [][]byte{{1}}}, packets); err == nil {
			t.Error("RelayWithUpdate started a folded tx during shutdown")
		}
	})

	// SendCosmosTxBatch may split a batch, commit the leading sub-batches and then
	// fail. Those messages are on chain; dropping the count here is what made the
	// module re-queue -- and re-submit -- packets it had already delivered, and
	// report them as DROPPED on the permanent ending.
	t.Run("reports the prefix a split batch committed", func(t *testing.T) {
		for _, tc := range []struct {
			name      string
			err       error
			want      int
			permanent bool
		}{
			{
				name: "a partial failure reports what landed",
				err:  &services.BatchPartialError{SucceededCount: 2, Err: errors.New("node went away")},
				want: 2,
			},
			{
				name: "a whole-batch failure reports nothing",
				err:  errors.New("broadcast failed"),
				want: 0,
			},
			{
				// The prefix is a fact about the chain, not about the class of the
				// failure: a deterministic revert in the LAST sub-batch does not
				// un-commit the ones before it.
				name:      "a permanent partial failure still reports its prefix",
				err:       &services.BatchPartialError{SucceededCount: 2, Err: services.ErrPermanentRelayFailure},
				want:      2,
				permanent: true,
			},
			{
				name: "every message landed",
				err:  nil,
				want: 3,
			},
		} {
			t.Run(tc.name, func(t *testing.T) {
				d := &Destination{worker: services.NewWorker(&partialTestTxHandler{err: tc.err}, nil)}
				committed, err := d.sendPacketBatch(context.Background(), []any{1, 2, 3})
				if committed != tc.want {
					t.Fatalf("committed = %d, want %d", committed, tc.want)
				}
				if (err != nil) != (tc.err != nil) {
					t.Fatalf("error = %v, want an error: %v", err, tc.err != nil)
				}
				if got := chain.IsPermanent(err); got != tc.permanent {
					t.Fatalf("permanent = %v, want %v", got, tc.permanent)
				}
			})
		}
	})

	// An empty packet list is a no-op, not an error: the module can hand over a batch
	// that emptied out after filtering, and turning that into an error would fail a
	// relay cycle that had nothing wrong with it.
	t.Run("treats an empty packet list as a no-op", func(t *testing.T) {
		d := &Destination{}
		if committed, err := d.RelayPackets(context.Background(), nil); err != nil || committed != 0 {
			t.Fatalf("empty packet list must be a no-op, got %v", err)
		}
	})

	// An empty payload list means "client already current" on this side, and must be
	// a silent success — the beacon builder returns one whenever the on-chain client
	// already covers the target slot.
	//
	// This deliberately DIFFERS from chain/evm, where UpdateClient requires exactly
	// one payload and errors otherwise. The two are not inconsistent: a beacon update
	// is an ordered batch of headers that may legitimately be empty, while the groth16
	// path submits exactly one proof per update. Anyone harmonising the two signatures
	// should break this test and read this comment first.
	t.Run("treats no payloads as client-already-current", func(t *testing.T) {
		d := &Destination{}
		if err := d.UpdateClient(context.Background(), "client-0", chain.ClientUpdate{}); err != nil {
			t.Fatalf("empty payload list means the client is current, got %v", err)
		}
	})

	// Folding several messages into one transaction only works if the handler can
	// submit them atomically. Without that guarantee a split would land the client
	// update in one block and the packets in another, against a client state the
	// packets' proof height no longer matches — so this must be refused up front
	// rather than discovered after the first half is already on chain.
	t.Run("refuses folding without an atomic handler", func(t *testing.T) {
		update := chain.ClientUpdate{Payloads: [][]byte{mustBeaconHeader(t, "100")}}
		packets := []chain.RelayPacket{{Type: chain.SendPacket, Packet: mustMarshalCosmosPacket(t)}}

		tests := []struct {
			name string
			dest *Destination
		}{
			{"no worker at all", &Destination{}},
			{"worker without a transaction handler", &Destination{worker: services.NewWorker(nil, nil)}},
			{
				"handler that cannot submit atomically",
				&Destination{worker: services.NewWorker(&struct{ services.TransactionHandler }{}, nil)},
			},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				if err := tt.dest.RelayWithUpdate(context.Background(), "client-0", update, packets); err == nil {
					t.Fatal("folded relay proceeded without an atomic sender")
				}
			})
		}
	})

	// A folded relay with no client update is a programming error, not an empty
	// batch: the whole point of the call is to install the update the packets prove
	// against. Falling through would submit packets at a proof height the destination
	// client has not reached.
	relayWithUpdateProofHeightSubtests(t)

	t.Run("refuses folding with no client update", func(t *testing.T) {
		d := &Destination{worker: services.NewWorker(&atomicTestTxHandler{}, nil)}
		err := d.RelayWithUpdate(context.Background(), "client-0", chain.ClientUpdate{}, nil)
		if err == nil {
			t.Fatal("folded relay accepted an empty client update")
		}
	})

	// A receipt check with no Cosmos client behind it must report that loudly
	// rather than answering "no receipt", which a caller would read as "safe to
	// time this packet out" — refunding a packet that was in fact delivered.
	t.Run("refuses to answer HasPacketReceipt without a client", func(t *testing.T) {
		d := &Destination{}
		ok, err := d.HasPacketReceipt(context.Background(), nil)
		if err == nil {
			t.Fatal("a receipt check with nothing to query must error, not answer")
		}
		if ok {
			t.Fatal("a receipt check with nothing to query must never report a receipt")
		}
	})
}

// ethClientExpiry is what the module's anti-expiry refresh acts on. Reporting it
// too late lets the 08-wasm client lapse, which stops the ETH→Cosmos direction
// entirely; too early only costs an extra update.
func TestEthClientExpiry(t *testing.T) {
	// Mainnet-shaped geometry: 32 slots per epoch, 256 epochs per sync-committee
	// period, 12s slots -> one period is 98,304 seconds.
	const (
		genesisTime    = uint64(1_606_824_023)
		secondsPerSlot = uint64(12)
		onePeriod      = uint64(256 * 32 * 12)
	)
	base := func() *relayerclient.EthereumClientState {
		return &relayerclient.EthereumClientState{
			EpochsPerSyncCommitteePeriod: 256,
			SlotsPerEpoch:                32,
			SecondsPerSlot:               secondsPerSlot,
			GenesisTime:                  genesisTime,
			GenesisSlot:                  0,
			LatestSlot:                   1000,
		}
	}

	t.Run("one sync-committee period after the latest tracked slot", func(t *testing.T) {
		cs := base()
		got, _ := ethClientExpiry(cs)
		want := time.Unix(int64(genesisTime+1000*secondsPerSlot+onePeriod), 0)
		if !got.Equal(want) {
			t.Fatalf("expiry = %s, want %s", got, want)
		}
	})

	t.Run("measured from the tracked slot, not from now", func(t *testing.T) {
		// The clock that matters belongs to the slot the client last tracked. A
		// value derived from wall-clock time would report every client as healthy
		// forever, because it moves with the check.
		older, newer := base(), base()
		older.LatestSlot = 1000
		newer.LatestSlot = 2000

		newerExpiry, _ := ethClientExpiry(newer)
		olderExpiry, _ := ethClientExpiry(older)
		gap := newerExpiry.Sub(olderExpiry)
		want := time.Duration(1000*secondsPerSlot) * time.Second
		if gap != want {
			t.Fatalf("1000 slots of progress moved the expiry by %s, want %s", gap, want)
		}
	})

	t.Run("a misconfigured client state reports no expiry", func(t *testing.T) {
		// Zero means "never expires" to the module, which then falls back to the
		// periodic refresh. Returning a real-looking time computed from zeros
		// would instead put the client permanently in the refresh window and make
		// every tick submit an update.
		for name, mutate := range map[string]func(*relayerclient.EthereumClientState){
			"no epochs per period": func(c *relayerclient.EthereumClientState) { c.EpochsPerSyncCommitteePeriod = 0 },
			"no slots per epoch":   func(c *relayerclient.EthereumClientState) { c.SlotsPerEpoch = 0 },
			"no seconds per slot":  func(c *relayerclient.EthereumClientState) { c.SecondsPerSlot = 0 },
		} {
			t.Run(name, func(t *testing.T) {
				cs := base()
				mutate(cs)
				if got, _ := ethClientExpiry(cs); !got.IsZero() {
					t.Fatalf("expiry = %s, want the zero time", got)
				}
			})
		}
	})

	t.Run("a slot at or below genesis falls back to genesis time", func(t *testing.T) {
		// ComputeTimestampAtSlot's own guard, exercised through this path: a client
		// that has not advanced past its genesis slot must not compute a timestamp
		// from an underflowed slot difference.
		cs := base()
		cs.GenesisSlot = 500
		cs.LatestSlot = 100 // below genesis

		got, _ := ethClientExpiry(cs)
		want := time.Unix(int64(genesisTime+onePeriod), 0)
		if !got.Equal(want) {
			t.Fatalf("expiry = %s, want %s (genesis time plus one period)", got, want)
		}
	})
}

// unreachableCosmos is an endpoint whose queries fail rather than panic, so a test
// can run PAST a check and observe that the check was not what stopped it.
func unreachableCosmos(t *testing.T) services.CosmosEndpoint {
	t.Helper()
	client, err := relayerclient.DialCosmosRPC("http://127.0.0.1:1", "/websocket", 200*time.Millisecond)
	if err != nil {
		t.Fatalf("dial unreachable cosmos: %v", err)
	}
	return services.CosmosEndpoint{Client: client}
}

// The proof block and the consensus height a batch declares are chosen in two
// different places, and nothing but this check ties them together. Getting it
// wrong does not fail locally: the packet submits, and the light client rejects it
// with "get trie node failed: Invalid state root" -- a message from inside CosmWasm
// that names neither height.
func TestRequireProofsBuiltAt(t *testing.T) {
	cases := []struct {
		name      string
		packets   []chain.RelayPacket
		execBlock uint64
		wantErr   bool
		why       string
	}{
		{
			name:      "matching heights pass",
			packets:   []chain.RelayPacket{{Sequence: 1, Height: 100}},
			execBlock: 100,
			wantErr:   false,
			why:       "the proof was built at the block this batch declares",
		},
		{
			name:      "a proof built below the declared height is refused",
			packets:   []chain.RelayPacket{{Sequence: 1, Height: 99}},
			execBlock: 100,
			wantErr:   true,
			why:       "this is the folded case: the update installs block 100 while the client still reported 99 when the proof was built",
		},
		{
			name:      "a proof built above the declared height is refused",
			packets:   []chain.RelayPacket{{Sequence: 1, Height: 101}},
			execBlock: 100,
			wantErr:   true,
			why:       "a proof ahead of the declared height verifies against a root the client does not have either",
		},
		{
			name:      "one bad packet in a batch fails the batch",
			packets:   []chain.RelayPacket{{Sequence: 1, Height: 100}, {Sequence: 2, Height: 97}},
			execBlock: 100,
			wantErr:   true,
			why:       "every message in the tx names the same consensus height, so one mismatch dooms the whole atomic batch",
		},
		{
			name:      "no packets is not a violation",
			packets:   nil,
			execBlock: 100,
			wantErr:   false,
			why:       "an empty batch has nothing to prove and must not become an error",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := requireProofsBuiltAt(tc.packets, tc.execBlock)
			if (err != nil) != tc.wantErr {
				t.Fatalf("requireProofsBuiltAt(...) error = %v, want error %v: %s", err, tc.wantErr, tc.why)
			}
		})
	}

	// The failing message must name BOTH heights. The whole cost of this bug was
	// that the on-chain error named neither, so the mismatch had to be
	// reconstructed from the block explorer.
	t.Run("the message names both heights", func(t *testing.T) {
		err := requireProofsBuiltAt([]chain.RelayPacket{{Sequence: 42, Height: 11581600}}, 11581694)
		if err == nil {
			t.Fatal("a mismatch must be an error")
		}
		for _, want := range []string{"42", "11581600", "11581694"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error must name %q so an operator can see the mismatch without a block explorer; got: %v", want, err)
			}
		}
	})
}

// signingTestTxHandler is atomicTestTxHandler plus the signer lookup
// RelayWithUpdate does before it reaches the guard.
type signingTestTxHandler struct{ services.TransactionHandler }

func (*signingTestTxHandler) SendCosmosTxBatchAtomic(context.Context, services.CosmosEndpoint, []any) error {
	return nil
}

func (*signingTestTxHandler) CosmosSignerAddress() (string, error) {
	return "cosmos1test", nil
}

func relayWithUpdateProofHeightSubtests(t *testing.T) {
	// The guard has to be WIRED into the folded path, not merely defined. This is
	// the exact shape of the production failure: an update installing one
	// execution block, carrying a packet proven at an earlier one.
	t.Run("refuses a proof from another block", func(t *testing.T) {
		d := &Destination{worker: services.NewWorker(&signingTestTxHandler{}, nil)}
		update := chain.ClientUpdate{Height: 11581694, Payloads: [][]byte{mustBeaconHeader(t, "11009952")}}
		packets := []chain.RelayPacket{{
			Type:     chain.SendPacket,
			Sequence: 7,
			Height:   11581600, // the block the client reported BEFORE this update
			Packet:   mustMarshalCosmosPacket(t),
		}}

		err := d.RelayWithUpdate(context.Background(), "08-wasm-8", update, packets)
		if err == nil {
			t.Fatal("the folded relay submitted a packet proven against a different block")
		}
		if !strings.Contains(err.Error(), "11581600") || !strings.Contains(err.Error(), "11581694") {
			t.Fatalf("expected the proof-height mismatch, got a different failure: %v", err)
		}
	})

	// The control. Without it a guard that rejected every folded batch would pass
	// the case above -- so this pins that a matching height gets PAST the check. It
	// then fails further on for want of a Cosmos endpoint, which is the point: the
	// guard is no longer what stops it.
	t.Run("accepts a proof from the update's own block", func(t *testing.T) {
		d := &Destination{worker: services.NewWorker(&signingTestTxHandler{}, nil), cosmos: unreachableCosmos(t)}
		update := chain.ClientUpdate{Height: 11581694, Payloads: [][]byte{mustBeaconHeader(t, "11009952")}}
		packets := []chain.RelayPacket{{
			Type:     chain.SendPacket,
			Sequence: 7,
			Height:   11581694,
			Packet:   mustMarshalCosmosPacket(t),
			Proof:    []byte(testProofJSON),
		}}

		err := d.RelayWithUpdate(context.Background(), "08-wasm-8", update, packets)
		if err != nil && strings.Contains(err.Error(), "was built at execution block") {
			t.Fatalf("the guard rejected a proof built at the update's own block: %v", err)
		}
	})
}
