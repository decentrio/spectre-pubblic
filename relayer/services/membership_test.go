package services

import (
	"context"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"

	channeltypesv2 "github.com/cosmos/ibc-go/v10/modules/core/04-channel/v2/types"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/ethclient"
)

func TestHasEthIBCPathValue_HonorsCancellation(t *testing.T) {
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { <-release }))
	t.Cleanup(server.Close)
	t.Cleanup(func() { close(release) })
	client, err := ethclient.Dial(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(client.Close)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := HasEthIBCPathValue(ctx, EVMEndpoint{Client: client, Contracts: EVMContracts{Router: common.HexToAddress("0x1")}}, []byte("path"))
		done <- err
	}()
	time.Sleep(30 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("error = %v, want context.Canceled", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("storage query ignored cancellation")
	}
}

// An empty payload list must stay empty rather than becoming nil: the ABI encoder
// treats the two the same, but a nil slice where the caller expected a length is
// how an off-by-one creeps into batch building.
func TestToEthPacket_EmptyPayloads(t *testing.T) {
	got := ToEthPacket(channeltypesv2.Packet{Sequence: 1})
	if got.Payloads == nil {
		t.Fatal("payloads is nil, want an empty slice")
	}
	if len(got.Payloads) != 0 {
		t.Fatalf("payloads = %d, want 0", len(got.Payloads))
	}
}

// ToEthPacket copies a packet field by field between two type systems, the same
// shape of code as the misbehaviour header conversion — and with the same failure
// mode: a field left out does not fail to compile, it just arrives zero and the
// router rejects the packet on chain.
//
// So this walks both structs and compares VALUES, field by field, rather than
// listing fields by hand and repeating the omission risk one level up.
//
// The first version asserted non-zeroness plus a hand-written port check, and
// the fixture used "transfer" for both ports — which made that check read
// "transfer" != "transfer" and pass under any swap. Review caught it; a mutation
// swapping SourcePort and DestPort in ToEthPacket survived the whole suite.
// Every field below therefore carries a value distinct from every other field of
// its type, so a conversion that pairs the wrong two cannot come out equal.
func TestToEthPacket_CarriesEveryField(t *testing.T) {
	src := channeltypesv2.Packet{
		Sequence:          7,
		SourceClient:      "cosmos-client-0",
		DestinationClient: "eth-router-0",
		TimeoutTimestamp:  1_700_000_000,
		Payloads: []channeltypesv2.Payload{{
			SourcePort:      "transfer-src",
			DestinationPort: "transfer-dst",
			Version:         "ics20-2",
			Encoding:        "application/x-solidity-abi",
			Value:           []byte{0x01, 0x02},
		}},
	}
	// Guard the fixture itself: a field added to the proto packet that this test
	// does not set would leave the conversion of it unchecked.
	assertNoZeroFields(t, reflect.ValueOf(src), "source packet")

	got := ToEthPacket(src)

	// ToEthPacket renames two fields on the way across, so the walk is told the
	// mapping rather than being loosened to skip them.
	assertFieldsCarriedAcross(t, reflect.ValueOf(src), reflect.ValueOf(got), "Packet", map[string]string{
		"DestinationClient": "DestClient",
		"DestinationPort":   "DestPort",
	})
}

// The storage slot is keccak256(keccak256(path) ‖ IBCSTORE_STORAGE_SLOT) — the
// standard Solidity mapping-slot derivation against the ERC-7201 store root in
// contracts/core/store/IBCStoreUpgradeable.sol:26.
func TestEthIBCStorageKey(t *testing.T) {
	t.Run("the storage root matches the Solidity constant", func(t *testing.T) {
		// Transcribed from IBCStoreUpgradeable.sol:26. If this fails, the two
		// languages disagree about where the IBC store lives and every ETH proof
		// is taken at the wrong root.
		const solidityIBCStoreSlot = "0x1260944489272988d9df285149b5aa1b0f48f2136d6f416159f840a3e0747600"
		if ICS26_IBC_STORAGE_SLOT != solidityIBCStoreSlot {
			t.Fatalf("Go storage slot %s does not match Solidity %s", ICS26_IBC_STORAGE_SLOT, solidityIBCStoreSlot)
		}
	})

	t.Run("distinct paths address distinct slots", func(t *testing.T) {
		// The three path types share a client id and a sequence, so if the type
		// byte were dropped from the hash all three would collide -- a receipt
		// proof would answer a commitment query.
		seen := map[string]string{}
		for _, pathType := range []byte{1, 2, 3} {
			key := EthIBCStorageKey(EthPath("client-0", 42, pathType)).Hex()
			if prev, dup := seen[key]; dup {
				t.Fatalf("path type %d addresses the same slot as %s", pathType, prev)
			}
			seen[key] = string(rune('0' + pathType))
		}
		if len(seen) != 3 {
			t.Fatalf("3 path types produced %d distinct slots", len(seen))
		}
	})

	t.Run("derives the slot Solidity would", func(t *testing.T) {
		// A known-answer test, because the two structural checks below hold even
		// with the derivation gutted: dropping the keccak of the path, or leaving
		// the store root out of the second hash, still yields distinct and
		// deterministic slots. Mutation caught exactly that.
		//
		// The expected value is computed independently with Foundry's cast, not by
		// this package's own code:
		//
		//	P=636c69656e742d3001000000000000002a          # client-0 ‖ 0x01 ‖ BE(42)
		//	H=$(cast keccak "0x$P")
		//	cast keccak "${H}1260944489272988d9df285149b5aa1b0f48f2136d6f416159f840a3e0747600"
		const want = "0x0596148b513b849040f1f6302b374a4442916c30ef7088676dc098b3a0a706b5"
		got := EthIBCStorageKey(EthPath("client-0", 42, 1)).Hex()
		if got != want {
			t.Fatalf("storage key = %s, want %s\n(recompute with cast before changing this: the value is the contract with Solidity)", got, want)
		}
	})

	t.Run("the same path is always the same slot", func(t *testing.T) {
		// Not a tautology: the derivation reads a package-level constant, and a
		// caller-order dependence there would make proofs intermittently wrong.
		a := EthIBCStorageKey(EthPath("client-0", 42, 1))
		b := EthIBCStorageKey(EthPath("client-0", 42, 1))
		if a != b {
			t.Fatalf("the same path derived two slots: %s and %s", a.Hex(), b.Hex())
		}
	})
}

// The layout is abi.encodePacked(clientId, uint8(pathType), uint64BigEndian(seq))
// — ICS24Host.sol:25-88. Path types come from the same three functions:
//
//	1  packetCommitmentPathCalldata
//	2  packetReceiptCommitmentPathCalldata
//	3  packetAcknowledgementCommitmentPathCalldata
func TestEthPath(t *testing.T) {
	const (
		pathCommitment = byte(1)
		pathReceipt    = byte(2)
		pathAck        = byte(3)
	)

	t.Run("matches the Solidity packed layout", func(t *testing.T) {
		tests := []struct {
			name     string
			clientID string
			sequence uint64
			pathType byte
			want     string // hex, transcribed from the Solidity encoding
		}{
			{
				name: "commitment", clientID: "client-0", sequence: 1, pathType: pathCommitment,
				// "client-0" = 636c69656e742d30, then 01, then 8-byte BE 1
				want: "636c69656e742d30" + "01" + "0000000000000001",
			},
			{
				name: "receipt", clientID: "client-0", sequence: 1, pathType: pathReceipt,
				want: "636c69656e742d30" + "02" + "0000000000000001",
			},
			{
				name: "acknowledgement", clientID: "client-0", sequence: 1, pathType: pathAck,
				want: "636c69656e742d30" + "03" + "0000000000000001",
			},
			{
				name: "big-endian sequence", clientID: "c", sequence: 0x0102030405060708, pathType: pathCommitment,
				// Byte order is the whole point: little-endian would address a
				// completely different slot and still look like a valid proof.
				want: "63" + "01" + "0102030405060708",
			},
			{
				name: "max sequence", clientID: "c", sequence: ^uint64(0), pathType: pathCommitment,
				want: "63" + "01" + "ffffffffffffffff",
			},
			{
				name: "empty client id", clientID: "", sequence: 7, pathType: pathReceipt,
				want: "" + "02" + "0000000000000007",
			},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				got := hex.EncodeToString(EthPath(tt.clientID, tt.sequence, tt.pathType))
				if got != tt.want {
					t.Fatalf("path = %s, want %s", got, tt.want)
				}
			})
		}
	})

	t.Run("does not alias the caller's client id", func(t *testing.T) {
		// EthPath appends to []byte(clientID). If that conversion ever stopped
		// copying, a second call could scribble over the first call's path — and
		// the two calls in a recv/ack pair happen microseconds apart.
		first := EthPath("client-0", 1, 1)
		second := EthPath("client-0", 2, 1)
		if hex.EncodeToString(first) == hex.EncodeToString(second) {
			t.Fatal("two paths at different sequences are identical")
		}
		if got, want := hex.EncodeToString(first), "636c69656e742d30010000000000000001"; got != want {
			t.Fatalf("the first path changed after a second call: %s, want %s", got, want)
		}
	})
}
