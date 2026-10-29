package client

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/cosmos/cosmos-sdk/types/tx"
	"github.com/cosmos/gogoproto/proto"
	ibcwasmtypes "github.com/cosmos/ibc-go/modules/light-clients/08-wasm/v10/types"
	clienttypes "github.com/cosmos/ibc-go/v10/modules/core/02-client/types"
	channeltypesv2 "github.com/cosmos/ibc-go/v10/modules/core/04-channel/v2/types"

	"relayer/chain/ethnative/spectrepb"
)

// The Rust light client's recorded fixtures are the shared input of both repos:
// the atomone module decodes their JSON with its own test decoders and writes the
// protobuf bytes it gets into testdata/spectre (TestWriteRelayerGolden there);
// this package converts the same JSON with the production converters and must
// produce the same bytes. Two independent implementations agreeing on every
// field is the contract that keeps the relayer's messages verifiable on chain.
const (
	fixtureDir = "../../packages/ethereum/light-client/src/test_utils/fixtures"
	goldenDir  = "testdata/spectre"
)

var goldenFixtures = []string{
	"Test_ICS20TransferERC20TokenfromEthereumToCosmosAndBack",
	"Test_TimeoutPacketFromCosmos",
}

type fixtureFile struct {
	Steps []struct {
		Name string          `json:"name"`
		Data json.RawMessage `json:"data"`
	} `json:"steps"`
}

func loadFixtureFile(t *testing.T, name string) fixtureFile {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(fixtureDir, name+".json"))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	var f fixtureFile
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatalf("parse fixture %s: %v", name, err)
	}
	return f
}

func readGolden(t *testing.T, fixture, file string) []byte {
	t.Helper()
	bz, err := os.ReadFile(filepath.Join(goldenDir, fixture, file))
	if err != nil {
		t.Fatalf("read golden %s/%s: %v", fixture, file, err)
	}
	return bz
}

func requireBytesEqual(t *testing.T, what string, got, want []byte) {
	t.Helper()
	if !bytes.Equal(got, want) {
		t.Fatalf("%s differs from the golden encoding (%d bytes vs %d); first difference at byte %d",
			what, len(got), len(want), firstDifference(got, want))
	}
}

func firstDifference(a, b []byte) int {
	for i := 0; i < len(a) && i < len(b); i++ {
		if a[i] != b[i] {
			return i
		}
	}
	return min(len(a), len(b))
}

// relayerPayloads holds what a fixture step's relayer transaction carried, in the
// relayer's JSON forms.
type relayerPayloads struct {
	headers, recvProofs, ackProofs, timeoutProofs [][]byte
}

// stepPayloads decodes the relayer transaction of a fixture step: the update
// client messages wrap the header JSON in an 08-wasm ClientMessage, and the
// packet messages carry the JSON storage proof directly.
func stepPayloads(t *testing.T, data json.RawMessage) relayerPayloads {
	t.Helper()
	var step struct {
		TxBody string `json:"relayer_tx_body"`
	}
	if err := json.Unmarshal(data, &step); err != nil {
		t.Fatalf("parse step: %v", err)
	}
	var out relayerPayloads
	if step.TxBody == "" {
		return out
	}
	txBytes, err := hex.DecodeString(step.TxBody)
	if err != nil {
		t.Fatalf("decode relayer tx body: %v", err)
	}
	var body tx.TxBody
	if err := proto.Unmarshal(txBytes, &body); err != nil {
		t.Fatalf("unmarshal tx body: %v", err)
	}
	for _, msg := range body.Messages {
		switch msg.TypeUrl {
		case "/ibc.core.client.v1.MsgUpdateClient":
			var uc clienttypes.MsgUpdateClient
			if err := proto.Unmarshal(msg.Value, &uc); err != nil {
				t.Fatalf("unmarshal MsgUpdateClient: %v", err)
			}
			var wrapped ibcwasmtypes.ClientMessage
			if err := wrapped.Unmarshal(uc.ClientMessage.Value); err != nil {
				t.Fatalf("unwrap client message: %v", err)
			}
			out.headers = append(out.headers, wrapped.Data)
		case "/ibc.core.channel.v2.MsgRecvPacket":
			var m channeltypesv2.MsgRecvPacket
			if err := proto.Unmarshal(msg.Value, &m); err != nil {
				t.Fatalf("unmarshal MsgRecvPacket: %v", err)
			}
			out.recvProofs = append(out.recvProofs, m.ProofCommitment)
		case "/ibc.core.channel.v2.MsgAcknowledgement":
			var m channeltypesv2.MsgAcknowledgement
			if err := proto.Unmarshal(msg.Value, &m); err != nil {
				t.Fatalf("unmarshal MsgAcknowledgement: %v", err)
			}
			out.ackProofs = append(out.ackProofs, m.ProofAcked)
		case "/ibc.core.channel.v2.MsgTimeout":
			var m channeltypesv2.MsgTimeout
			if err := proto.Unmarshal(msg.Value, &m); err != nil {
				t.Fatalf("unmarshal MsgTimeout: %v", err)
			}
			out.timeoutProofs = append(out.timeoutProofs, m.ProofUnreceived)
		}
	}
	return out
}

func TestSpectreProtoMatchesTheAtomoneEncoding(t *testing.T) {
	for _, name := range goldenFixtures {
		t.Run(name, func(t *testing.T) {
			fixture := loadFixtureFile(t, name)
			var initial struct {
				ClientState    EthereumClientState    `json:"client_state"`
				ConsensusState EthereumConsensusState `json:"consensus_state"`
			}
			if err := json.Unmarshal(fixture.Steps[0].Data, &initial); err != nil {
				t.Fatalf("parse initial state: %v", err)
			}

			clientState, err := initial.ClientState.ToProto()
			if err != nil {
				t.Fatalf("client state to proto: %v", err)
			}
			bz, err := clientState.Marshal()
			if err != nil {
				t.Fatal(err)
			}
			requireBytesEqual(t, "client state", bz, readGolden(t, name, "client_state.pb"))

			consensusState, err := initial.ConsensusState.ToProto()
			if err != nil {
				t.Fatalf("consensus state to proto: %v", err)
			}
			bz, err = consensusState.Marshal()
			if err != nil {
				t.Fatal(err)
			}
			requireBytesEqual(t, "consensus state", bz, readGolden(t, name, "consensus_state.pb"))

			var headers, recv, acks, timeouts int
			for i := 1; i < len(fixture.Steps); i++ {
				payloads := stepPayloads(t, fixture.Steps[i].Data)
				for _, raw := range payloads.headers {
					var header EthereumHeader
					if err := json.Unmarshal(raw, &header); err != nil {
						t.Fatalf("parse header %d: %v", headers, err)
					}
					pb, err := header.ToProto()
					if err != nil {
						t.Fatalf("header %d to proto: %v", headers, err)
					}
					bz, err := pb.Marshal()
					if err != nil {
						t.Fatal(err)
					}
					requireBytesEqual(t, fmt.Sprintf("header %d", headers), bz, readGolden(t, name, fmt.Sprintf("header_%d.pb", headers)))
					headers++
				}
				for kind, group := range map[string]struct {
					proofs [][]byte
					n      *int
				}{
					"recv_proof":    {payloads.recvProofs, &recv},
					"ack_proof":     {payloads.ackProofs, &acks},
					"timeout_proof": {payloads.timeoutProofs, &timeouts},
				} {
					for _, raw := range group.proofs {
						bz, err := MembershipProofFromJSON(raw)
						if err != nil {
							t.Fatalf("%s %d: %v", kind, *group.n, err)
						}
						requireBytesEqual(t, fmt.Sprintf("%s %d", kind, *group.n), bz, readGolden(t, name, fmt.Sprintf("%s_%d.pb", kind, *group.n)))
						*group.n++
					}
				}
			}
			if headers == 0 {
				t.Fatal("the fixture produced no headers: the test would prove nothing")
			}
			if recv+acks+timeouts == 0 {
				t.Fatal("the fixture produced no proofs: the test would prove nothing")
			}
			// Every golden file was compared: nothing was written that the test skips.
			entries, err := os.ReadDir(filepath.Join(goldenDir, name))
			if err != nil {
				t.Fatal(err)
			}
			if want := 2 + headers + recv + acks + timeouts; len(entries) != want {
				t.Fatalf("%d golden files, %d compared", len(entries), want)
			}
		})
	}
}

func TestEthereumClientStateRoundTripsThroughProto(t *testing.T) {
	fixture := loadFixtureFile(t, goldenFixtures[0])
	var initial struct {
		ClientState EthereumClientState `json:"client_state"`
	}
	if err := json.Unmarshal(fixture.Steps[0].Data, &initial); err != nil {
		t.Fatal(err)
	}
	pb, err := initial.ClientState.ToProto()
	if err != nil {
		t.Fatal(err)
	}
	got := EthereumClientStateFromProto(pb)

	want := initial.ClientState
	// Fulu and Gloas are absent from the recorded state: they come back as the
	// unscheduled sentinel the chain stores.
	want.ForkParameters.Fulu = Fork{Epoch: ^uint64(0), Version: "0x00000000"}
	want.ForkParameters.Gloas = Fork{Epoch: ^uint64(0), Version: "0x00000000"}
	if got.ChainID != want.ChainID || got.LatestSlot != want.LatestSlot ||
		got.LatestExecutionBlockNumber != want.LatestExecutionBlockNumber ||
		got.SlotsPerEpoch != want.SlotsPerEpoch || got.SecondsPerSlot != want.SecondsPerSlot ||
		got.EpochsPerSyncCommitteePeriod != want.EpochsPerSyncCommitteePeriod ||
		got.SyncCommitteeSize != want.SyncCommitteeSize || got.GenesisTime != want.GenesisTime ||
		got.IsFrozen != want.IsFrozen {
		t.Fatalf("scalar fields changed:\n got  %+v\n want %+v", got, want)
	}
	if got.ForkParameters != want.ForkParameters {
		t.Fatalf("fork parameters changed:\n got  %+v\n want %+v", got.ForkParameters, want.ForkParameters)
	}
	if !bytes.EqualFold([]byte(got.GenesisValidatorsRoot), []byte(want.GenesisValidatorsRoot)) ||
		!bytes.EqualFold([]byte(got.IbcContractAddress), []byte(want.IbcContractAddress)) ||
		!bytes.EqualFold([]byte(got.IBCCommitmentSlot), []byte(want.IBCCommitmentSlot)) {
		t.Fatalf("hex fields changed:\n got  %+v\n want %+v", got, want)
	}
}

func TestForkParametersToProto(t *testing.T) {
	valid := ForkParameters{
		GenesisForkVersion: "0x00000001",
		Altair:             Fork{Version: "0x01000001"},
		Bellatrix:          Fork{Version: "0x02000001"},
		Capella:            Fork{Version: "0x03000001"},
		Deneb:              Fork{Version: "0x04000001"},
		Electra:            Fork{Version: "0x05000001"},
		Fulu:               Fork{Version: "0x06000001", Epoch: 7},
		Gloas:              Fork{Version: "0x07000001", Epoch: 9},
	}

	t.Run("scheduled forks keep their epoch and version", func(t *testing.T) {
		pb, err := valid.toProto()
		if err != nil {
			t.Fatal(err)
		}
		if pb.Gloas.Epoch != 9 || hex.EncodeToString(pb.Gloas.Version) != "07000001" ||
			pb.Fulu.Epoch != 7 || hex.EncodeToString(pb.Fulu.Version) != "06000001" {
			t.Fatalf("fulu/gloas = %+v / %+v", pb.Fulu, pb.Gloas)
		}
	})

	t.Run("an unscheduled fork is epoch 2^64-1 with the zero version", func(t *testing.T) {
		unscheduled := valid
		unscheduled.Fulu = Fork{Version: "0x00000000", Epoch: ^uint64(0)}
		unscheduled.Gloas = Fork{Version: "0x00000000", Epoch: ^uint64(0)}
		pb, err := unscheduled.toProto()
		if err != nil {
			t.Fatal(err)
		}
		if pb.Fulu.Epoch != ^uint64(0) || pb.Gloas.Epoch != ^uint64(0) || len(pb.Gloas.Version) != 4 {
			t.Fatalf("fulu/gloas = %+v / %+v", pb.Fulu, pb.Gloas)
		}
	})

	t.Run("a state without fulu or gloas is unscheduled, not scheduled at genesis", func(t *testing.T) {
		old := valid
		old.Fulu, old.Gloas = Fork{}, Fork{}
		pb, err := old.toProto()
		if err != nil {
			t.Fatal(err)
		}
		if pb.Fulu.Epoch != ^uint64(0) || pb.Gloas.Epoch != ^uint64(0) {
			t.Fatalf("an omitted fork was scheduled: fulu=%+v gloas=%+v", pb.Fulu, pb.Gloas)
		}
	})

	t.Run("a required fork must carry a version", func(t *testing.T) {
		bad := valid
		bad.Electra = Fork{}
		if _, err := bad.toProto(); err == nil {
			t.Fatal("an electra fork without a version converted")
		}
	})

	t.Run("a fork scheduled at genesis stays scheduled", func(t *testing.T) {
		genesis := valid
		genesis.Fulu = Fork{Version: "0x06000001", Epoch: 0}
		pb, err := genesis.toProto()
		if err != nil {
			t.Fatal(err)
		}
		if pb.Fulu.Epoch != 0 || hex.EncodeToString(pb.Fulu.Version) != "06000001" {
			t.Fatalf("fulu = %+v", pb.Fulu)
		}
	})
}

func TestEthereumHeaderToProto(t *testing.T) {
	committee := func() *SyncCommittee {
		return &SyncCommittee{Pubkeys: []string{"0x" + string(bytes.Repeat([]byte("a"), 96))}, AggregatePubkey: "0x" + string(bytes.Repeat([]byte("b"), 96))}
	}
	base := func() EthereumHeader {
		beacon := BeaconBlockHeader{Slot: "10", ProposerIndex: "3", ParentRoot: "0x01", StateRoot: "0x02", BodyRoot: "0x03"}
		return EthereumHeader{
			ActiveSyncCommittee: ActiveSyncCommittee{Current: committee()},
			TrustedSlot:         8,
			ConsensusUpdate: LightClientUpdate{
				AttestedHeader:  LightClientHeader{Beacon: beacon, ExecutionBlockHash: "0x" + string(bytes.Repeat([]byte("c"), 64)), ExecutionBranch: []string{"0x04"}},
				FinalizedHeader: LightClientHeader{Beacon: beacon, ExecutionBlockHash: "0x" + string(bytes.Repeat([]byte("d"), 64)), ExecutionBranch: []string{"0x05"}},
				FinalityBranch:  []string{"0x06"},
				SyncAggregate:   SyncAggregate{SyncCommitteeBits: "0xff", SyncCommitteeSignature: "0x07"},
				SignatureSlot:   "12",
			},
			ExecutionHeaderRLP: "0xc0",
		}
	}

	t.Run("gloas format: block hash and execution header rlp, no payload header", func(t *testing.T) {
		header := base()
		pb, err := header.ToProto()
		if err != nil {
			t.Fatal(err)
		}
		if pb.ConsensusUpdate.FinalizedHeader.Execution != nil || len(pb.ConsensusUpdate.FinalizedHeader.ExecutionBlockHash) != 32 {
			t.Fatalf("finalized header = %+v", pb.ConsensusUpdate.FinalizedHeader)
		}
		if !bytes.Equal(pb.ExecutionHeaderRlp, []byte{0xc0}) {
			t.Fatalf("execution header rlp = %x", pb.ExecutionHeaderRlp)
		}
		if pb.IsNextCommittee || pb.TrustedSlot != 8 || pb.ConsensusUpdate.SignatureSlot != 12 {
			t.Fatalf("header = %+v", pb)
		}
	})

	t.Run("the next committee is flagged", func(t *testing.T) {
		header := base()
		header.ActiveSyncCommittee = ActiveSyncCommittee{Next: committee()}
		pb, err := header.ToProto()
		if err != nil {
			t.Fatal(err)
		}
		if !pb.IsNextCommittee {
			t.Fatal("a Next committee was not flagged")
		}
	})

	t.Run("exactly one of Current and Next", func(t *testing.T) {
		neither := base()
		neither.ActiveSyncCommittee = ActiveSyncCommittee{}
		if _, err := neither.ToProto(); err == nil {
			t.Fatal("a header without an active committee converted")
		}
		both := base()
		both.ActiveSyncCommittee = ActiveSyncCommittee{Current: committee(), Next: committee()}
		if _, err := both.ToProto(); err == nil {
			t.Fatal("a header with both committees converted")
		}
	})

	t.Run("malformed fields are errors, not silent zeros", func(t *testing.T) {
		badSlot := base()
		badSlot.ConsensusUpdate.SignatureSlot = "twelve"
		if _, err := badSlot.ToProto(); err == nil {
			t.Fatal("a non-numeric signature slot converted")
		}
		badHex := base()
		badHex.ConsensusUpdate.FinalityBranch = []string{"0xzz"}
		if _, err := badHex.ToProto(); err == nil {
			t.Fatal("a non-hex branch node converted")
		}
	})
}

func TestMembershipProofFromJSON(t *testing.T) {
	zero32 := "0x" + string(bytes.Repeat([]byte("0"), 64))

	t.Run("value is a 32-byte big-endian integer", func(t *testing.T) {
		raw := fmt.Sprintf(`{"account_proof":{"storage_root":%q,"proof":["0x01","0x0203"]},"storage_proof":{"key":%q,"value":"0x2a","proof":["0x04"]}}`, zero32, zero32)
		bz, err := MembershipProofFromJSON([]byte(raw))
		if err != nil {
			t.Fatal(err)
		}
		var proof spectrepb.MembershipProof
		if err := proof.Unmarshal(bz); err != nil {
			t.Fatal(err)
		}
		if len(proof.StorageProof.Value) != 32 || proof.StorageProof.Value[31] != 0x2a || proof.StorageProof.Value[0] != 0 {
			t.Fatalf("value = %x", proof.StorageProof.Value)
		}
		if len(proof.AccountProof.Proof) != 2 || !bytes.Equal(proof.AccountProof.Proof[1], []byte{0x02, 0x03}) {
			t.Fatalf("account proof nodes = %x", proof.AccountProof.Proof)
		}
	})

	t.Run("a non-membership proof carries the zero value", func(t *testing.T) {
		raw := fmt.Sprintf(`{"account_proof":{"storage_root":%q,"proof":["0x01"]},"storage_proof":{"key":%q,"value":"0x0","proof":["0x04"]}}`, zero32, zero32)
		bz, err := MembershipProofFromJSON([]byte(raw))
		if err != nil {
			t.Fatal(err)
		}
		var proof spectrepb.MembershipProof
		if err := proof.Unmarshal(bz); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(proof.StorageProof.Value, make([]byte, 32)) {
			t.Fatalf("value = %x, want 32 zero bytes", proof.StorageProof.Value)
		}
	})

	for name, raw := range map[string]string{
		"not json":            `proof`,
		"short storage root":  `{"account_proof":{"storage_root":"0x01","proof":[]},"storage_proof":{"key":"` + zero32 + `","value":"0x1","proof":[]}}`,
		"value above uint256": `{"account_proof":{"storage_root":"` + zero32 + `","proof":[]},"storage_proof":{"key":"` + zero32 + `","value":"0x1` + string(bytes.Repeat([]byte("0"), 64)) + `","proof":[]}}`,
		"non-hex proof node":  `{"account_proof":{"storage_root":"` + zero32 + `","proof":["0xnope"]},"storage_proof":{"key":"` + zero32 + `","value":"0x1","proof":[]}}`,
	} {
		t.Run("rejects "+name, func(t *testing.T) {
			if _, err := MembershipProofFromJSON([]byte(raw)); err == nil {
				t.Fatal("converted a malformed proof")
			}
		})
	}
}
