package bold

import (
	"context"
	"errors"
	"math/big"
	"strings"
	"testing"

	arbitrum "attestor/arbitrum"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/rpc"
)

func TestParseAssertionCreatedRecomputesAssertionHash(t *testing.T) {
	data := make([]byte, assertionCreatedDataWords*abiWordSize)
	l2BlockHash := common.HexToHash("0x1234")
	copy(data[13*abiWordSize:14*abiWordSize], l2BlockHash[:])
	data[17*abiWordSize+abiWordSize-1] = 1
	afterInboxBatchAcc := common.HexToHash("0x5678")
	copy(data[19*abiWordSize:20*abiWordSize], afterInboxBatchAcc[:])
	parentHash := common.HexToHash("0xabcd")
	afterStateHash := crypto.Keccak256Hash(data[13*abiWordSize : 19*abiWordSize])
	assertionHash := crypto.Keccak256Hash(
		parentHash[:],
		afterStateHash[:],
		afterInboxBatchAcc[:],
	)

	proposal, err := parseAssertionCreated(types.Log{
		Topics:      []common.Hash{assertionCreatedTopic, assertionHash, parentHash},
		Data:        data,
		BlockNumber: 99,
	})
	if err != nil {
		t.Fatalf("parse AssertionCreated: %v", err)
	}
	if proposal.AssertionHash != assertionHash ||
		proposal.ParentHash != parentHash ||
		proposal.L2BlockHash != l2BlockHash ||
		proposal.InboxAccumulator != afterInboxBatchAcc ||
		proposal.L1BlockNumber != 99 {
		t.Fatalf("parsed proposal: %+v", proposal)
	}

	badTopics := []common.Hash{assertionCreatedTopic, common.HexToHash("0xdead"), parentHash}
	if _, err := parseAssertionCreated(types.Log{
		Topics: badTopics,
		Data:   data,
	}); err == nil {
		t.Fatal("expected assertion hash mismatch")
	}
}

func TestRollupCoreSourceReadsStatusAndFinalizedLogs(t *testing.T) {
	rollupAddress := common.HexToAddress("0x0000000000000000000000000000000000001234")
	data := make([]byte, assertionConfirmedDataWords*abiWordSize)
	l2BlockHash := common.HexToHash("0xbeef")
	copy(data[:abiWordSize], l2BlockHash[:])
	assertionHash := common.HexToHash("0xface")
	client := &sourceTestL1Client{
		logs: []types.Log{{
			Address:     rollupAddress,
			Topics:      []common.Hash{assertionConfirmedTopic, assertionHash},
			Data:        data,
			BlockNumber: 100,
		}},
		storageOutput: make([]byte, abiWordSize),
	}
	const statusOffset = uint8(25)
	client.storageOutput[abiWordSize-1-int(statusOffset)] = assertionStatusConfirmed
	mappingSlot := common.HexToHash("0x76")

	source, err := NewRollupCoreSource(client, rollupAddress, mappingSlot, statusOffset)
	if err != nil {
		t.Fatalf("create source: %v", err)
	}
	_, confirmations, err := source.Assertions(context.Background(), 90, 100)
	if err != nil {
		t.Fatalf("read assertion logs: %v", err)
	}
	if len(confirmations) != 1 ||
		confirmations[0].AssertionHash != assertionHash ||
		confirmations[0].L2BlockHash != l2BlockHash {
		t.Fatalf("confirmations: %+v", confirmations)
	}
	status, err := source.AssertionStatus(context.Background(), arbitrum.ProposedAssertion{
		AssertionHash: assertionHash,
	}, 100)
	if err != nil {
		t.Fatalf("read assertion status: %v", err)
	}
	if status != assertionStatusConfirmed {
		t.Fatalf("status: got %d want %d", status, assertionStatusConfirmed)
	}
	statusPreimage := append(assertionHash.Bytes(), mappingSlot.Bytes()...)
	if client.storageKey != crypto.Keccak256Hash(statusPreimage) ||
		client.storageBlock.Uint64() != 100 {
		t.Fatalf(
			"status storage lookup: key=%s block=%v",
			client.storageKey,
			client.storageBlock,
		)
	}
	if client.filterQuery.FromBlock.Uint64() != 90 ||
		client.filterQuery.ToBlock.Uint64() != 100 ||
		len(client.filterQuery.Addresses) != 1 ||
		client.filterQuery.Addresses[0] != rollupAddress {
		t.Fatalf("filter query: %+v", client.filterQuery)
	}
}

func TestRollupCoreSourceSkipsZeroHashAssertionCreated(t *testing.T) {
	rollupAddress := common.HexToAddress("0x0000000000000000000000000000000000001234")
	data := make([]byte, assertionCreatedDataWords*abiWordSize)
	data[17*abiWordSize+abiWordSize-1] = 1
	client := &sourceTestL1Client{
		logs: []types.Log{{
			Address: rollupAddress,
			Topics: []common.Hash{
				assertionCreatedTopic,
				common.HexToHash("0x75"),
				common.HexToHash("0x50"),
			},
			Data:        data,
			BlockNumber: 100,
		}},
	}
	source, err := NewRollupCoreSource(
		client,
		rollupAddress,
		common.HexToHash("0x76"),
		25,
	)
	if err != nil {
		t.Fatalf("create source: %v", err)
	}
	proposals, confirmations, err := source.Assertions(
		context.Background(),
		90,
		100,
	)
	if err != nil {
		t.Fatalf("read assertion logs: %v", err)
	}
	if len(proposals) != 0 || len(confirmations) != 0 {
		t.Fatalf(
			"unexpected assertions: proposals=%+v confirmations=%+v",
			proposals,
			confirmations,
		)
	}
}

func TestNewRollupCoreSourceFailureMatrix(t *testing.T) {
	address := common.HexToAddress("0x0000000000000000000000000000000000001234")
	client := &sourceTestL1Client{}
	for _, tc := range []struct {
		name    string
		client  L1Client
		address common.Address
		offset  uint8
		want    string
	}{
		{name: "nil client", address: address, want: "client"},
		{name: "zero address", client: client, want: "address"},
		{name: "status offset outside word", client: client, address: address, offset: abiWordSize, want: "outside"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewRollupCoreSource(tc.client, tc.address, common.Hash{}, tc.offset)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("NewRollupCoreSource error = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestRollupCoreSourceFinalizedBlockFailureMatrix(t *testing.T) {
	for _, tc := range []struct {
		name   string
		client *sourceTestL1Client
		want   string
	}{
		{name: "not found", client: &sourceTestL1Client{headerErr: ethereum.NotFound}, want: "not found"},
		{name: "nil header", client: &sourceTestL1Client{returnNilHeader: true}, want: "not found"},
		{name: "rpc failure", client: &sourceTestL1Client{headerErr: errors.New("L1 unavailable")}, want: "L1 unavailable"},
		{name: "nil number", client: &sourceTestL1Client{header: &types.Header{}}, want: "not a uint64"},
		{name: "overflow number", client: &sourceTestL1Client{header: &types.Header{Number: new(big.Int).Lsh(big.NewInt(1), 64)}}, want: "not a uint64"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			source := newTestRollupCoreSource(t, tc.client)
			_, err := source.FinalizedBlockNumber(context.Background())
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("FinalizedBlockNumber error = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestRollupCoreSourceChainIDsFailureMatrix(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*sourceTestL1Client)
		want   string
	}{
		{name: "L1 chain ID error", mutate: func(c *sourceTestL1Client) { c.chainIDErr = errors.New("chain ID unavailable") }, want: "chain ID unavailable"},
		{name: "nil L1 chain ID", mutate: func(c *sourceTestL1Client) { c.returnNilChainID = true }, want: "chain ID is nil"},
		{name: "RollupCore call error", mutate: func(c *sourceTestL1Client) { c.callErr = errors.New("contract unavailable") }, want: "contract unavailable"},
		{name: "short RollupCore output", mutate: func(c *sourceTestL1Client) { c.callOutput = make([]byte, abiWordSize-1) }, want: "returned 31 bytes"},
		{name: "long RollupCore output", mutate: func(c *sourceTestL1Client) { c.callOutput = make([]byte, abiWordSize+1) }, want: "returned 33 bytes"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := &sourceTestL1Client{callOutput: make([]byte, abiWordSize)}
			tc.mutate(client)
			source := newTestRollupCoreSource(t, client)
			_, _, err := source.ChainIDs(context.Background(), 100)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("ChainIDs error = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestRollupCoreSourceAssertionsFailureMatrix(t *testing.T) {
	rollupAddress := common.HexToAddress("0x0000000000000000000000000000000000001234")
	for _, tc := range []struct {
		name   string
		from   uint64
		to     uint64
		mutate func(*sourceTestL1Client)
		want   string
	}{
		{name: "reversed range", from: 101, to: 100, want: "invalid assertion log range"},
		{name: "filter error", from: 100, to: 100, mutate: func(c *sourceTestL1Client) { c.filterErr = errors.New("logs unavailable") }, want: "logs unavailable"},
		{name: "removed finalized log", from: 100, to: 100, mutate: func(c *sourceTestL1Client) { c.logs = []types.Log{{Removed: true, BlockNumber: 100}} }, want: "marked removed"},
		{name: "missing topic", from: 100, to: 100, mutate: func(c *sourceTestL1Client) { c.logs = []types.Log{{BlockNumber: 100}} }, want: "no event topic"},
		{name: "unexpected topic", from: 100, to: 100, mutate: func(c *sourceTestL1Client) {
			c.logs = []types.Log{{Topics: []common.Hash{common.HexToHash("0x99")}, BlockNumber: 100}}
		}, want: "unexpected"},
		{name: "invalid created event", from: 100, to: 100, mutate: func(c *sourceTestL1Client) {
			c.logs = []types.Log{{Topics: []common.Hash{assertionCreatedTopic}, BlockNumber: 100}}
		}, want: "has 1 topics"},
		{name: "invalid confirmation event", from: 100, to: 100, mutate: func(c *sourceTestL1Client) {
			c.logs = []types.Log{{Topics: []common.Hash{assertionConfirmedTopic}, BlockNumber: 100}}
		}, want: "has 1 topics"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := &sourceTestL1Client{}
			if tc.mutate != nil {
				tc.mutate(client)
			}
			source, err := NewRollupCoreSource(client, rollupAddress, common.Hash{}, 0)
			if err != nil {
				t.Fatalf("NewRollupCoreSource: %v", err)
			}
			_, _, err = source.Assertions(context.Background(), tc.from, tc.to)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Assertions error = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestRollupCoreSourceAssertionStatusFailureMatrix(t *testing.T) {
	proposal := arbitrum.ProposedAssertion{AssertionHash: common.HexToHash("0x42")}
	for _, tc := range []struct {
		name   string
		mutate func(*sourceTestL1Client)
		want   string
	}{
		{name: "storage error", mutate: func(c *sourceTestL1Client) { c.storageErr = errors.New("storage unavailable") }, want: "storage unavailable"},
		{name: "short storage word", mutate: func(c *sourceTestL1Client) { c.storageOutput = make([]byte, abiWordSize-1) }, want: "returned 31 bytes"},
		{name: "unknown status", mutate: func(c *sourceTestL1Client) {
			c.storageOutput = make([]byte, abiWordSize)
			c.storageOutput[abiWordSize-1] = assertionStatusConfirmed + 1
		}, want: "unknown status"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := &sourceTestL1Client{storageOutput: make([]byte, abiWordSize)}
			tc.mutate(client)
			source := newTestRollupCoreSource(t, client)
			_, err := source.AssertionStatus(context.Background(), proposal, 100)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("AssertionStatus error = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestParseAssertionEventFailureMatrix(t *testing.T) {
	for _, tc := range []struct {
		name   string
		parse  func(types.Log) error
		mutate func(*types.Log)
		want   string
	}{
		{
			name:   "created topic count",
			parse:  func(event types.Log) error { _, err := parseAssertionCreated(event); return err },
			mutate: func(event *types.Log) { event.Topics = event.Topics[:2] },
			want:   "has 2 topics",
		},
		{
			name:   "created data length",
			parse:  func(event types.Log) error { _, err := parseAssertionCreated(event); return err },
			mutate: func(event *types.Log) { event.Data = event.Data[:len(event.Data)-1] },
			want:   "data bytes",
		},
		{
			name:   "created machine status not uint8",
			parse:  func(event types.Log) error { _, err := parseAssertionCreated(event); return err },
			mutate: func(event *types.Log) { event.Data[17*abiWordSize] = 1 },
			want:   "not a uint8",
		},
		{
			name:   "created unfinished machine",
			parse:  func(event types.Log) error { _, err := parseAssertionCreated(event); return err },
			mutate: func(event *types.Log) { event.Data[17*abiWordSize+abiWordSize-1] = 0 },
			want:   "unfinished",
		},
		{
			name:   "created unknown machine status",
			parse:  func(event types.Log) error { _, err := parseAssertionCreated(event); return err },
			mutate: func(event *types.Log) { event.Data[17*abiWordSize+abiWordSize-1] = maxMachineStatus + 1 },
			want:   "unknown machine status",
		},
		{
			name:   "created zero L2 block hash",
			parse:  func(event types.Log) error { _, err := parseAssertionCreated(event); return err },
			mutate: func(event *types.Log) { copy(event.Data[13*abiWordSize:14*abiWordSize], make([]byte, abiWordSize)) },
			want:   "zero L2 block hash",
		},
		{
			name:   "created assertion hash mismatch",
			parse:  func(event types.Log) error { _, err := parseAssertionCreated(event); return err },
			mutate: func(event *types.Log) { event.Topics[1] = common.HexToHash("0xdead") },
			want:   "hash mismatch",
		},
		{
			name:   "confirmed topic count",
			parse:  func(event types.Log) error { _, err := parseAssertionConfirmed(event); return err },
			mutate: func(event *types.Log) { event.Topics = event.Topics[:1] },
			want:   "has 1 topics",
		},
		{
			name:   "confirmed data length",
			parse:  func(event types.Log) error { _, err := parseAssertionConfirmed(event); return err },
			mutate: func(event *types.Log) { event.Data = event.Data[:len(event.Data)-1] },
			want:   "data bytes",
		},
		{
			name:   "confirmed zero L2 block hash",
			parse:  func(event types.Log) error { _, err := parseAssertionConfirmed(event); return err },
			mutate: func(event *types.Log) { copy(event.Data[:abiWordSize], make([]byte, abiWordSize)) },
			want:   "zero L2 block hash",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			event := validCreatedAssertionLog()
			if strings.HasPrefix(tc.name, "confirmed") {
				event = validConfirmedAssertionLog()
			}
			tc.mutate(&event)
			err := tc.parse(event)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("parse error = %v, want %q", err, tc.want)
			}
		})
	}
}

func validCreatedAssertionLog() types.Log {
	data := make([]byte, assertionCreatedDataWords*abiWordSize)
	l2BlockHash := common.HexToHash("0x1234")
	copy(data[13*abiWordSize:14*abiWordSize], l2BlockHash[:])
	data[17*abiWordSize+abiWordSize-1] = 1
	afterInboxBatchAcc := common.HexToHash("0x5678")
	copy(data[19*abiWordSize:20*abiWordSize], afterInboxBatchAcc[:])
	parentHash := common.HexToHash("0xabcd")
	afterStateHash := crypto.Keccak256Hash(data[13*abiWordSize : 19*abiWordSize])
	assertionHash := crypto.Keccak256Hash(parentHash[:], afterStateHash[:], afterInboxBatchAcc[:])
	return types.Log{
		Topics:      []common.Hash{assertionCreatedTopic, assertionHash, parentHash},
		Data:        data,
		BlockNumber: 99,
	}
}

func validConfirmedAssertionLog() types.Log {
	l2BlockHash := common.HexToHash("0x1234")
	data := make([]byte, assertionConfirmedDataWords*abiWordSize)
	copy(data[:abiWordSize], l2BlockHash[:])
	return types.Log{
		Topics:      []common.Hash{assertionConfirmedTopic, common.HexToHash("0xabcd")},
		Data:        data,
		BlockNumber: 99,
	}
}

func newTestRollupCoreSource(t *testing.T, client *sourceTestL1Client) *RollupCoreSource {
	t.Helper()
	source, err := NewRollupCoreSource(
		client,
		common.HexToAddress("0x0000000000000000000000000000000000001234"),
		common.HexToHash("0x76"),
		0,
	)
	if err != nil {
		t.Fatalf("NewRollupCoreSource: %v", err)
	}
	return source
}

type sourceTestL1Client struct {
	logs          []types.Log
	callOutput    []byte
	storageOutput []byte
	storageKey    common.Hash
	storageBlock  *big.Int
	filterQuery   ethereum.FilterQuery

	header           *types.Header
	headerErr        error
	returnNilHeader  bool
	chainID          *big.Int
	chainIDErr       error
	returnNilChainID bool
	filterErr        error
	callErr          error
	storageErr       error
}

func (c *sourceTestL1Client) HeaderByNumber(
	context.Context,
	*big.Int,
) (*types.Header, error) {
	if c.returnNilHeader || c.headerErr != nil {
		return nil, c.headerErr
	}
	if c.header != nil {
		return c.header, nil
	}
	return &types.Header{Number: big.NewInt(100)}, nil
}

func (c *sourceTestL1Client) ChainID(context.Context) (*big.Int, error) {
	if c.returnNilChainID || c.chainIDErr != nil {
		return nil, c.chainIDErr
	}
	if c.chainID != nil {
		return c.chainID, nil
	}
	return big.NewInt(1), nil
}

func (c *sourceTestL1Client) FilterLogs(
	_ context.Context,
	query ethereum.FilterQuery,
) ([]types.Log, error) {
	c.filterQuery = query
	return append([]types.Log(nil), c.logs...), c.filterErr
}

func (c *sourceTestL1Client) CallContract(
	context.Context,
	ethereum.CallMsg,
	*big.Int,
) ([]byte, error) {
	return append([]byte(nil), c.callOutput...), c.callErr
}

func (c *sourceTestL1Client) StorageAt(
	_ context.Context,
	_ common.Address,
	key common.Hash,
	block *big.Int,
) ([]byte, error) {
	c.storageKey = key
	c.storageBlock = new(big.Int).Set(block)
	return append([]byte(nil), c.storageOutput...), c.storageErr
}

func (c *sourceTestL1Client) StorageAtHash(_ context.Context, _ common.Address, key common.Hash, hash common.Hash) ([]byte, error) {
	c.storageKey = key
	return append([]byte(nil), c.storageOutput...), c.storageErr
}

type headRecordingL1Client struct {
	sourceTestL1Client
	tag        *big.Int
	statusHash common.Hash
}

func (c *headRecordingL1Client) HeaderByNumber(ctx context.Context, n *big.Int) (*types.Header, error) {
	c.tag = new(big.Int).Set(n)
	return c.sourceTestL1Client.HeaderByNumber(ctx, n)
}

func (c *headRecordingL1Client) StorageAtHash(ctx context.Context, address common.Address, key, hash common.Hash) ([]byte, error) {
	c.statusHash = hash
	return c.sourceTestL1Client.StorageAtHash(ctx, address, key, hash)
}

func TestRollupSourceHeadTagsAndHashPinnedStatus(t *testing.T) {
	c := &headRecordingL1Client{sourceTestL1Client: sourceTestL1Client{storageOutput: make([]byte, abiWordSize)}}
	c.storageOutput[31] = assertionStatusPending
	s, err := NewRollupCoreSource(c, common.HexToAddress("0x1"), common.Hash{}, 0)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for _, tc := range []struct {
		head arbitrum.L1IngestHead
		tag  rpc.BlockNumber
	}{
		{arbitrum.L1IngestFinalized, rpc.FinalizedBlockNumber},
		{arbitrum.L1IngestSafe, rpc.SafeBlockNumber},
		{arbitrum.L1IngestLatest, rpc.LatestBlockNumber},
	} {
		head, err := s.Head(ctx, tc.head)
		if err != nil {
			t.Fatal(err)
		}
		if c.tag.Int64() != int64(tc.tag) || head.Number != 100 || head.Hash == (common.Hash{}) {
			t.Fatalf("head %s: %+v, tag %v", tc.head, head, c.tag)
		}
		status, err := s.AssertionStatusAtHash(ctx, earlyProposal(1, 110), head.Hash)
		if err != nil || status != assertionStatusPending || c.statusHash != head.Hash {
			t.Fatalf("hash status: %d %v", status, err)
		}
	}
	if _, err := s.CanonicalHash(ctx, 100); err != nil || c.tag.Uint64() != 100 {
		t.Fatalf("canonical lookup: %v", err)
	}
	if _, err := s.CanonicalHash(ctx, 101); err == nil {
		t.Fatal("accepted wrong-height header")
	}
	c.headerErr = errors.New("unsupported tag")
	if _, err := s.Head(ctx, arbitrum.L1IngestSafe); err == nil {
		t.Fatal("unsupported head silently accepted")
	}
	c.headerErr = nil
	c.returnNilHeader = true
	if _, err := s.Head(ctx, arbitrum.L1IngestLatest); err == nil {
		t.Fatal("nil head accepted")
	}
	c.returnNilHeader = false
	c.storageErr = errors.New("hash unavailable")
	if _, err := s.AssertionStatusAtHash(ctx, earlyProposal(1, 110), blockHash(110)); err == nil {
		t.Fatal("hash status failure ignored")
	}
	c.storageErr = nil
	c.storageOutput = []byte{1}
	if _, err := s.AssertionStatusAtHash(ctx, earlyProposal(1, 110), blockHash(110)); err == nil {
		t.Fatal("malformed status accepted")
	}
}
