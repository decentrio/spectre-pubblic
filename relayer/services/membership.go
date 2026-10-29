package services

import (
	"context"
	"encoding/binary"
	"fmt"
	"math/big"
	contractICS26Router "relayer/bindings/ICS26Router"
	spectreContract "relayer/bindings/SpectreClient"
	client "relayer/client"
	"relayer/utils"

	channeltypesv2 "github.com/cosmos/ibc-go/v10/modules/core/04-channel/v2/types"
	ics23 "github.com/cosmos/ics23/go"
	ethcommon "github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
)

// Packet-state proofs and the ICS-24 paths they are keyed on, on both chains:
// Cosmos membership / non-membership proofs for the EVM side, and the EVM
// router's storage slots for the Cosmos side. Split from services.go (E6).

func HasPendingEthPacketCommitment(stdCtx context.Context, ctx EVMEndpoint, packet channeltypesv2.Packet) (bool, error) {
	return HasEthIBCPathValue(stdCtx, ctx, EthPath(packet.SourceClient, packet.Sequence, 1))
}

func HasEthPacketReceipt(stdCtx context.Context, ctx EVMEndpoint, packet channeltypesv2.Packet) (bool, error) {
	return HasEthIBCPathValue(stdCtx, ctx, EthPath(packet.DestinationClient, packet.Sequence, 2))
}

func HasEthIBCPathValue(stdCtx context.Context, ctx EVMEndpoint, path []byte) (bool, error) {
	if ctx.EthClient() == nil {
		return false, fmt.Errorf("eth client is nil")
	}
	if ctx.RouterContract() == nil {
		return false, fmt.Errorf("router contract address is nil")
	}

	callCtx, cancel := fetchCtx(stdCtx, defaultFetchTimeout)
	defer cancel()

	value, err := ctx.EthClient().StorageAt(callCtx, *ctx.RouterContract(), EthIBCStorageKey(path), nil)
	if err != nil {
		return false, fmt.Errorf("eth storage query failed: %w", err)
	}

	for _, b := range value {
		if b != 0 {
			return true, nil
		}
	}
	return false, nil
}

func EthIBCStorageKey(path []byte) ethcommon.Hash {
	pathHash := crypto.Keccak256(path)
	return crypto.Keccak256Hash(pathHash, ethcommon.HexToHash(ICS26_IBC_STORAGE_SLOT).Bytes())
}

func EthPath(clientID string, sequence uint64, pathType byte) []byte {
	seqBytes := make([]byte, 8)
	binary.BigEndian.PutUint64(seqBytes, sequence)
	path := append([]byte(clientID), pathType)
	return append(path, seqBytes...)
}

func ToEthPacket(packet channeltypesv2.Packet) contractICS26Router.IICS26RouterMsgsPacket {
	payloads := make([]contractICS26Router.IICS26RouterMsgsPayload, 0, len(packet.Payloads))
	for _, p := range packet.Payloads {
		payloads = append(payloads, contractICS26Router.IICS26RouterMsgsPayload{
			SourcePort: p.SourcePort,
			DestPort:   p.DestinationPort,
			Version:    p.Version,
			Encoding:   p.Encoding,
			Value:      p.Value,
		})
	}

	return contractICS26Router.IICS26RouterMsgsPacket{
		Sequence:         packet.Sequence,
		SourceClient:     packet.SourceClient,
		DestClient:       packet.DestinationClient,
		TimeoutTimestamp: packet.TimeoutTimestamp,
		Payloads:         payloads,
	}
}

func parseMerkleProof(proofs []*ics23.CommitmentProof, sequence uint64) (spectreContract.IMembershipMsgsMerkleProof, error) {
	merkleProof := spectreContract.IMembershipMsgsMerkleProof{
		Proofs: []spectreContract.IMembershipMsgsCommitmentProof{},
	}
	for _, p := range proofs {
		commitmentProof, err := client.ParseCommitmentProof(p)
		if err != nil {
			return merkleProof, fmt.Errorf("failed to parse commitment proof for seq=%d: %w", sequence, err)
		}
		merkleProof.Proofs = append(merkleProof.Proofs, *commitmentProof)
	}
	return merkleProof, nil
}

// cosmosReceiptPresentError preserves the already-received sentinel on the
// non-membership path. Without it the timeout scanner mistakes a delivered
// packet for a transient proof failure and defers it forever.
func cosmosReceiptPresentError(height int64, valueLen int) error {
	return fmt.Errorf("non-membership expected empty value at height=%d, got %d bytes: %w",
		height, valueLen, client.ErrPacketAlreadyReceived)
}

func CosmosNonMembership(stdCtx context.Context, ctx CosmosEndpoint, packet channeltypesv2.Packet, clientID string, pathType []byte, latestLightBlock *client.LightBlock) ([]byte, error) {
	height := latestLightBlock.BlockHeight
	ibcPath := utils.IBCPath(clientID, packet.Sequence, pathType)
	value, proof, err := client.ProvePath(stdCtx, ctx.CosmosClient(), height, ibcPath)
	if err != nil {
		return nil, err
	}
	if len(value) != 0 {
		return nil, cosmosReceiptPresentError(height, len(value))
	}

	merkleProof, err := parseMerkleProof(proof.Proofs, packet.Sequence)
	if err != nil {
		return nil, err
	}

	nonMembershipMsg := spectreContract.ILightClientMsgsMsgVerifyNonMembership{
		Height: spectreContract.IICS02ClientMsgsHeight{
			RevisionHeight: uint64(height),
			RevisionNumber: 0,
		},
		KvPairs: []spectreContract.IMembershipMsgsKVPair{
			{
				Path:  ibcPath,
				Value: value,
			},
		},
		MerkleProofs: []spectreContract.IMembershipMsgsMerkleProof{merkleProof},
		AppHash:      utils.BytesToBytes32(latestLightBlock.SignedHeader.AppHash),
		TrustedConsensusState: spectreContract.ISpectreTendermintMsgsConsensusState{
			Timestamp:          big.NewInt(latestLightBlock.SignedHeader.Header.Time.UnixNano()),
			Root:               utils.BytesToBytes32(latestLightBlock.SignedHeader.AppHash),
			NextValidatorsHash: utils.BytesToBytes32(latestLightBlock.SignedHeader.Header.NextValidatorsHash),
		},
		MembershipType: 0,
	}

	calldata, err := tendermintAbiJson.Pack("verifyNonMembership", nonMembershipMsg)
	if err != nil {
		return nil, fmt.Errorf("failed to ABI encode verifyNonMembership: %w", err)
	}
	return calldata[4:], nil
}

func CosmosMembership(stdCtx context.Context, ctx CosmosEndpoint, packet channeltypesv2.Packet, clientID string, pathType []byte, latestLightBlock *client.LightBlock) ([]byte, error) {
	height := latestLightBlock.BlockHeight
	ibcPath := utils.IBCPath(clientID, packet.Sequence, pathType)
	value, proof, err := client.ProvePath(stdCtx, ctx.CosmosClient(), height, ibcPath)
	if err != nil {
		return nil, err
	}
	if len(value) == 0 {
		// Already the right CLASS -- a bare error is transient -- but not the right
		// name. Wrapping the sentinel is what lets one test assert this row of the
		// failure catalog across all three sources instead of per adapter.
		return nil, fmt.Errorf("commitment empty at height=%d: %w", height, client.ErrCommitmentAbsent)
	}

	merkleProof, err := parseMerkleProof(proof.Proofs, packet.Sequence)
	if err != nil {
		return nil, err
	}

	membershipMsg := spectreContract.ILightClientMsgsMsgVerifyMembership{
		Height: spectreContract.IICS02ClientMsgsHeight{
			RevisionHeight: uint64(height),
			RevisionNumber: 0,
		},
		KvPairs: []spectreContract.IMembershipMsgsKVPair{
			{
				Path:  ibcPath,
				Value: value,
			},
		},
		MerkleProofs: []spectreContract.IMembershipMsgsMerkleProof{merkleProof},
		AppHash:      utils.BytesToBytes32(latestLightBlock.SignedHeader.AppHash),
		TrustedConsensusState: spectreContract.ISpectreTendermintMsgsConsensusState{
			Timestamp:          big.NewInt(latestLightBlock.SignedHeader.Header.Time.UnixNano()),
			Root:               utils.BytesToBytes32(latestLightBlock.SignedHeader.AppHash),
			NextValidatorsHash: utils.BytesToBytes32(latestLightBlock.SignedHeader.Header.NextValidatorsHash),
		},
		MembershipType: 0,
	}

	calldata, err := tendermintAbiJson.Pack("verifyMembership", membershipMsg)
	if err != nil {
		return nil, fmt.Errorf("failed to ABI encode verifyMembership: %w", err)
	}
	return calldata[4:], nil
}
