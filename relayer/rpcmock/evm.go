package rpcmock

import (
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/ethclient"
)

// EVMNode is a JSON-RPC stub for an EVM execution client.
//
// Defaults are chosen so a caller that only wants "a working chain" writes no
// setup at all: chain id 1, nonce 0, 1 gwei gas price, a generous block gas
// limit, and eth_sendRawTransaction accepted. Override only what the test is
// actually about.
//
// Every field is guarded, because ethclient issues concurrent requests and the
// tests run under -race.
type EVMNode struct {
	server *httptest.Server

	mu             sync.Mutex
	chainID        uint64
	gasPrice       *big.Int
	blockGasLim    uint64
	nonces         map[common.Address]uint64
	receipts       map[common.Hash]*RawReceipt
	defaultRcpt    *RawReceipt
	sendErr        error
	callResult     string
	callBySelector map[string]string
	// callAtBlock answers an eth_call made AT an explicit block number, keyed by
	// block then selector. A call tagged "latest" never reads it, so a test can
	// model a "latest" read served on one side of a state change and a
	// block-pinned read on the other.
	callAtBlock map[uint64]map[string]string
	// failCallAt makes the next N eth_calls pinned at a block fail -- a
	// load-balanced backend a block behind answering "header not found".
	failCallAt   map[uint64]*failCallAt
	callErr      error
	callRPCErr   *rpcError
	sentRawTxs   []string
	nonceQueries map[common.Address]int
	methodCalls  map[string]int
	code         map[common.Address]string
	// storage is served by eth_getStorageAt, keyed by contract then slot. A slot
	// nobody set answers zero, which is what a real node says about an empty
	// mapping entry -- the honest default for "no commitment here".
	storage map[common.Address]map[common.Hash]common.Hash
	// logs is served by eth_getLogs, filtered the way a node filters: by
	// address, by positional topic (each position an OR-list), by block range.
	logs        []RawLog
	blockNumber uint64
	// blockTime is the timestamp eth_getBlockByNumber reports, in seconds.
	blockTime uint64
	// logRangeCap, when non-zero, makes eth_getLogs refuse any span wider than
	// it with logRangeMsg -- the shape of a provider's block-range limit.
	logRangeCap uint64
	logRangeMsg string
	// historyFloor, when non-zero, makes eth_getLogs refuse any query whose
	// fromBlock is below it with historyMsg -- the shape of geth's history
	// expiry, which refuses on the lower bound however narrow the range.
	historyFloor uint64
	historyMsg   string
	// logRanges records every eth_getLogs [fromBlock,toBlock] asked, refused or
	// not, so a test can assert what span a lookup requested.
	logRanges [][2]uint64
	// onLogs, when set, is called with the running eth_getLogs count each time
	// one is asked, before it is answered -- so a test can make the Nth lookup
	// slow. It runs under n.mu.
	onLogs func(call int)
	// batches counts JSON-RPC batch requests, so a test can assert that a
	// probe was one round trip rather than one per slot.
	batches int
	// batchErr, when set, makes every JSON-RPC BATCH request fail at the
	// transport (HTTP 500) while single requests keep working. That is the
	// shape of "the probe failed but the reads around it did not": the relayer
	// batches its storage probes and sends everything else one call at a time.
	batchErr error
}

// RawLog is one event log the stub serves. Topics[0] is the event signature
// hash, as on a real node; Data is the ABI-encoded non-indexed fields.
type RawLog struct {
	Address     common.Address
	Topics      []common.Hash
	Data        []byte
	BlockNumber uint64
	TxHash      common.Hash
	Index       uint
}

// RawReceipt is the subset of a transaction receipt the stub serves. It is a
// deliberate subset: anything the relayer does not read is not modelled, so a
// test cannot accidentally depend on a field the stub invents.
type RawReceipt struct {
	Status      uint64
	GasUsed     uint64
	BlockNumber uint64
}

// NewEVMNode starts a stub EVM node and stops it when the test finishes.
func NewEVMNode(t testing.TB) *EVMNode {
	t.Helper()
	n := &EVMNode{
		chainID:        1,
		gasPrice:       big.NewInt(1_000_000_000), // 1 gwei
		blockGasLim:    0xffffff,
		nonces:         make(map[common.Address]uint64),
		receipts:       make(map[common.Hash]*RawReceipt),
		callResult:     "0x",
		callBySelector: map[string]string{},
		nonceQueries:   make(map[common.Address]int),
		methodCalls:    make(map[string]int),
		code:           make(map[common.Address]string),
		storage:        make(map[common.Address]map[common.Hash]common.Hash),
		blockNumber:    1,
	}
	n.server = httptest.NewServer(n)
	t.Cleanup(n.server.Close)
	return n
}

// SetStorage makes eth_getStorageAt answer value for slot on addr. An unset
// slot answers zero, so a test opts IN to every commitment it wants to exist.
func (n *EVMNode) SetStorage(addr common.Address, slot, value common.Hash) {
	n.withLock(func() {
		if n.storage[addr] == nil {
			n.storage[addr] = make(map[common.Hash]common.Hash)
		}
		n.storage[addr][slot] = value
	})
}

// SetBlockNumber changes the head eth_blockNumber reports.
func (n *EVMNode) SetBlockNumber(head uint64) { n.withLock(func() { n.blockNumber = head }) }

// AddLog appends an event log eth_getLogs can serve.
func (n *EVMNode) AddLog(l RawLog) { n.withLock(func() { n.logs = append(n.logs, l) }) }

// CapLogRange makes eth_getLogs refuse any [from,to] wider than maxBlocks with
// message, which is how a capped provider answers. 0 lifts the cap.
func (n *EVMNode) CapLogRange(maxBlocks uint64, message string) {
	n.withLock(func() { n.logRangeCap, n.logRangeMsg = maxBlocks, message })
}

// PruneLogsBelow makes eth_getLogs refuse any query starting below floor with
// message (code 4444), which is how a history-pruned geth node answers. 0 lifts it.
func (n *EVMNode) PruneLogsBelow(floor uint64, message string) {
	n.withLock(func() { n.historyFloor, n.historyMsg = floor, message })
}

// LogRanges returns every [fromBlock,toBlock] eth_getLogs was asked, in order.
func (n *EVMNode) LogRanges() [][2]uint64 {
	var out [][2]uint64
	n.withLock(func() { out = append(out, n.logRanges...) })
	return out
}

// OnLogs registers f to run, with the running eth_getLogs count, each time one
// is asked and before it is answered. Pass nil to remove it.
func (n *EVMNode) OnLogs(f func(call int)) { n.withLock(func() { n.onLogs = f }) }

// FailBatches makes every JSON-RPC batch request fail with an HTTP 500 carrying
// err's text, leaving single requests untouched. Pass nil to serve batches again.
func (n *EVMNode) FailBatches(err error) { n.withLock(func() { n.batchErr = err }) }

// Batches counts JSON-RPC batch requests received.
func (n *EVMNode) Batches() int {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.batches
}

// URL is the stub's address, for a caller that dials it itself.
func (n *EVMNode) URL() string { return n.server.URL }

// Client dials the stub and returns a real *ethclient.Client.
func (n *EVMNode) Client(t testing.TB) *ethclient.Client {
	t.Helper()
	client, err := ethclient.Dial(n.server.URL)
	if err != nil {
		t.Fatalf("dial stub EVM node: %v", err)
	}
	t.Cleanup(client.Close)
	return client
}

// --- knobs ---

// SetChainID changes the id reported by eth_chainId. Distinct ids are what keep
// nonce state separate per chain, so tests about that must set it.
func (n *EVMNode) SetChainID(id uint64) { n.withLock(func() { n.chainID = id }) }

// SetNonce sets the account nonce eth_getTransactionCount reports for addr.
func (n *EVMNode) SetNonce(addr common.Address, nonce uint64) {
	n.withLock(func() { n.nonces[addr] = nonce })
}

// SetReceipt makes eth_getTransactionReceipt answer for hash. Without one the
// stub reports "not mined yet" (a null result), which is what a caller polling
// for inclusion sees.
func (n *EVMNode) SetReceipt(hash common.Hash, r RawReceipt) {
	n.withLock(func() { n.receipts[hash] = &r })
}

// MineEverything makes eth_getTransactionReceipt answer for ANY hash, modelling a
// chain that includes whatever it is given. Use it when the test cannot know the
// hash in advance: a signed transaction's hash is derived from its contents, so a
// caller driving a real signer has no way to pre-register a receipt for it.
// SetReceipt still wins for hashes registered explicitly.
func (n *EVMNode) MineEverything(r RawReceipt) { n.withLock(func() { n.defaultRcpt = &r }) }

// FailSend makes eth_sendRawTransaction return an RPC error. Pass nil to accept
// again.
func (n *EVMNode) FailSend(err error) { n.withLock(func() { n.sendErr = err }) }

// SetCode makes eth_getCode report codeHex ("0x...") for addr, modelling a
// deployed contract. An address left unset answers "0x", which is what a real
// node returns for one nobody deployed to — so a test must opt IN to a contract
// existing, and can never accidentally assume one does.
func (n *EVMNode) SetCode(addr common.Address, codeHex string) {
	n.withLock(func() { n.code[addr] = codeHex })
}

// SetCallResult sets the ABI-encoded return data eth_call answers with.
func (n *EVMNode) SetCallResult(hexData string) { n.withLock(func() { n.callResult = hexData }) }

// SetCallResultFor answers one 4-byte selector differently from the rest.
//
// SetCallResult alone cannot express a contract whose methods disagree, and that
// is exactly what a wiring check reads: getClient returns an address while
// getCounterparty returns a struct, and a test that cannot tell them apart can
// only assert that some call happened. selector is the hex method id, with or
// without the 0x prefix.
func (n *EVMNode) SetCallResultFor(selector, hexData string) {
	n.withLock(func() { n.callBySelector[normalizeSelector(selector)] = hexData })
}

func normalizeSelector(selector string) string {
	return strings.ToLower(strings.TrimPrefix(selector, "0x"))
}

// SetCallResultForAt answers one selector differently when the call names an
// explicit block number. See callAtBlock.
func (n *EVMNode) SetCallResultForAt(block uint64, selector, hexData string) {
	n.withLock(func() {
		if n.callAtBlock == nil {
			n.callAtBlock = make(map[uint64]map[string]string)
		}
		if n.callAtBlock[block] == nil {
			n.callAtBlock[block] = make(map[string]string)
		}
		n.callAtBlock[block][normalizeSelector(selector)] = hexData
	})
}

type failCallAt struct {
	remaining int
	err       rpcError
}

// FailCallAt makes the next times eth_calls that name block explicitly fail
// with this JSON-RPC error, then answer normally. A call tagged "latest" is
// never failed by it.
func (n *EVMNode) FailCallAt(block uint64, times, code int, message string) {
	n.withLock(func() {
		if n.failCallAt == nil {
			n.failCallAt = make(map[uint64]*failCallAt)
		}
		n.failCallAt[block] = &failCallAt{remaining: times, err: rpcError{Code: code, Message: message}}
	})
}

// FailCall makes eth_call return an RPC error, the shape a reverting contract
// read takes.
func (n *EVMNode) FailCall(err error) { n.withLock(func() { n.callErr = err }) }

// SetBlockTime sets the timestamp (unix seconds) eth_getBlockByNumber reports:
// the destination chain's clock, for checks that compare against block.timestamp.
func (n *EVMNode) SetBlockTime(unixSeconds uint64) { n.withLock(func() { n.blockTime = unixSeconds }) }

// FailCallWith makes eth_call return a JSON-RPC error with this exact code and
// data member, for a test that must tell a revert from a node error that also
// carries data (a rate limit's {"retryAfter": 1}). data nil omits the member.
func (n *EVMNode) FailCallWith(code int, message string, data any) {
	n.withLock(func() { n.callRPCErr = &rpcError{Code: code, Message: message, Data: data} })
}

// SetBlockGasLimit changes the limit reported by eth_getBlockByNumber, which is
// what batch-splitting decisions are measured against.
func (n *EVMNode) SetBlockGasLimit(limit uint64) { n.withLock(func() { n.blockGasLim = limit }) }

// --- observations ---

// SentRawTxs returns the hex payloads passed to eth_sendRawTransaction, in order.
func (n *EVMNode) SentRawTxs() []string {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([]string(nil), n.sentRawTxs...)
}

// NonceQueries counts eth_getTransactionCount calls for addr — the measurement
// behind "the nonce is cached, not re-read per transaction".
func (n *EVMNode) NonceQueries(addr common.Address) int {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.nonceQueries[addr]
}

// MethodCalls counts calls to a JSON-RPC method by name.
func (n *EVMNode) MethodCalls(method string) int {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.methodCalls[method]
}

func (n *EVMNode) withLock(f func()) {
	n.mu.Lock()
	defer n.mu.Unlock()
	f()
}

// --- transport ---

type rpcRequest struct {
	JSONRPC string            `json:"jsonrpc"`
	ID      json.RawMessage   `json:"id"`
	Method  string            `json:"method"`
	Params  []json.RawMessage `json:"params"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

func (n *EVMNode) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var body json.RawMessage
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "application/json")

	// A JSON-RPC batch is an array of requests answered by an array of
	// responses, in order. go-ethereum's BatchCallContext sends exactly that.
	if len(body) > 0 && body[0] == '[' {
		var reqs []rpcRequest
		if err := json.Unmarshal(body, &reqs); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		n.mu.Lock()
		n.batches++
		if n.batchErr != nil {
			n.mu.Unlock()
			http.Error(w, n.batchErr.Error(), http.StatusInternalServerError)
			return
		}
		responses := make([]rpcResponse, 0, len(reqs))
		for _, req := range reqs {
			n.methodCalls[req.Method]++
			result, rpcErr := n.dispatch(req)
			responses = append(responses, rpcResponse{JSONRPC: "2.0", ID: req.ID, Result: result, Error: rpcErr})
		}
		n.mu.Unlock()
		_ = json.NewEncoder(w).Encode(responses)
		return
	}

	var req rpcRequest
	if err := json.Unmarshal(body, &req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	n.mu.Lock()
	n.methodCalls[req.Method]++
	result, rpcErr := n.dispatch(req)
	n.mu.Unlock()

	_ = json.NewEncoder(w).Encode(rpcResponse{
		JSONRPC: "2.0", ID: req.ID, Result: result, Error: rpcErr,
	})
}

// dispatch answers one call. The caller holds n.mu.
func (n *EVMNode) dispatch(req rpcRequest) (any, *rpcError) {
	switch req.Method {
	case "eth_chainId":
		return hexUint(n.chainID), nil
	case "eth_gasPrice", "eth_maxPriorityFeePerGas":
		return "0x" + n.gasPrice.Text(16), nil
	case "eth_getTransactionCount":
		addr := addressParam(req.Params)
		n.nonceQueries[addr]++
		return hexUint(n.nonces[addr]), nil
	case "eth_sendRawTransaction":
		if n.sendErr != nil {
			return nil, &rpcError{Code: -32000, Message: n.sendErr.Error()}
		}
		n.sentRawTxs = append(n.sentRawTxs, stringParam(req.Params, 0))
		return zeroHash, nil
	case "eth_getTransactionReceipt":
		hash := common.HexToHash(stringParam(req.Params, 0))
		receipt, ok := n.receipts[hash]
		if !ok || receipt == nil {
			receipt = n.defaultRcpt
		}
		if receipt == nil {
			return nil, nil // not mined yet
		}
		return receiptJSON(hash, receipt), nil
	case "eth_getBlockByNumber":
		return n.blockJSON(), nil
	case "eth_getCode":
		// "0x" is the honest default: an address nobody deployed to holds no
		// code, and that is what a real node answers. A test about a deployed
		// contract says so with SetCode.
		addr := common.HexToAddress(stringParam(req.Params, 0))
		if code, ok := n.code[addr]; ok {
			return code, nil
		}
		return "0x", nil
	case "eth_call":
		if n.callRPCErr != nil {
			return nil, n.callRPCErr
		}
		if n.callErr != nil {
			return nil, &rpcError{Code: -32000, Message: n.callErr.Error()}
		}
		if block, ok := explicitCallBlock(req.Params); ok {
			if f := n.failCallAt[block]; f != nil && f.remaining > 0 {
				f.remaining--
				e := f.err
				return nil, &e
			}
		}
		if result, ok := n.callResultAtBlock(req.Params); ok {
			return result, nil
		}
		if result, ok := n.callResultForRequest(req.Params); ok {
			return result, nil
		}
		return n.callResult, nil
	case "eth_estimateGas":
		return hexUint(21_000), nil
	case "eth_blockNumber":
		return hexUint(n.blockNumber), nil
	case "eth_getStorageAt":
		addr := common.HexToAddress(stringParam(req.Params, 0))
		slot := common.HexToHash(stringParam(req.Params, 1))
		return n.storage[addr][slot].Hex(), nil
	case "eth_getLogs":
		return n.getLogs(req.Params)
	default:
		return nil, &rpcError{Code: -32601, Message: "method not found: " + req.Method}
	}
}

// getLogs filters n.logs the way a node does. The caller holds n.mu.
func (n *EVMNode) getLogs(params []json.RawMessage) (any, *rpcError) {
	if len(params) == 0 {
		return nil, &rpcError{Code: -32602, Message: "missing filter"}
	}
	var filter struct {
		FromBlock string            `json:"fromBlock"`
		ToBlock   string            `json:"toBlock"`
		Address   json.RawMessage   `json:"address"`
		Topics    []json.RawMessage `json:"topics"`
	}
	if err := json.Unmarshal(params[0], &filter); err != nil {
		return nil, &rpcError{Code: -32602, Message: err.Error()}
	}
	from := n.blockParam(filter.FromBlock, 0)
	to := n.blockParam(filter.ToBlock, n.blockNumber)
	n.logRanges = append(n.logRanges, [2]uint64{from, to})
	if n.onLogs != nil {
		n.onLogs(len(n.logRanges))
	}
	if from < n.historyFloor {
		return nil, &rpcError{Code: 4444, Message: n.historyMsg}
	}
	if n.logRangeCap > 0 && to >= from && to-from+1 > n.logRangeCap {
		return nil, &rpcError{Code: -32005, Message: n.logRangeMsg}
	}
	addresses := hashListParam(filter.Address)
	topics := make([][]string, len(filter.Topics))
	for i, raw := range filter.Topics {
		topics[i] = hashListParam(raw)
	}

	out := []any{}
	for _, l := range n.logs {
		if l.BlockNumber < from || l.BlockNumber > to {
			continue
		}
		if len(addresses) > 0 && !containsFold(addresses, l.Address.Hex()) {
			continue
		}
		if !topicsMatch(topics, l.Topics) {
			continue
		}
		hexTopics := make([]string, len(l.Topics))
		for i, t := range l.Topics {
			hexTopics[i] = t.Hex()
		}
		out = append(out, map[string]any{
			"address":          l.Address.Hex(),
			"topics":           hexTopics,
			"data":             "0x" + common.Bytes2Hex(l.Data),
			"blockNumber":      hexUint(l.BlockNumber),
			"transactionHash":  l.TxHash.Hex(),
			"transactionIndex": "0x0",
			"blockHash":        zeroHash,
			"logIndex":         hexUint(uint64(l.Index)),
			"removed":          false,
		})
	}
	return out, nil
}

// blockParam decodes a block tag or hex quantity; tags mean the head.
func (n *EVMNode) blockParam(raw string, dflt uint64) uint64 {
	switch raw {
	case "":
		return dflt
	case "latest", "pending", "safe", "finalized":
		return n.blockNumber
	case "earliest":
		return 0
	}
	v, ok := new(big.Int).SetString(strings.TrimPrefix(raw, "0x"), 16)
	if !ok {
		return dflt
	}
	return v.Uint64()
}

// hashListParam decodes a filter member that is a hex string, a list of them,
// or null (match anything) into a list; empty means "any".
func hashListParam(raw json.RawMessage) []string {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var one string
	if err := json.Unmarshal(raw, &one); err == nil {
		return []string{one}
	}
	var many []string
	_ = json.Unmarshal(raw, &many)
	return many
}

func containsFold(list []string, want string) bool {
	for _, s := range list {
		if strings.EqualFold(s, want) {
			return true
		}
	}
	return false
}

// topicsMatch applies positional topic filters: a nil position matches any
// topic, a list matches any of its members, and a log shorter than the filter
// does not match.
func topicsMatch(filter [][]string, topics []common.Hash) bool {
	for i, want := range filter {
		if len(want) == 0 {
			continue
		}
		if i >= len(topics) || !containsFold(want, topics[i].Hex()) {
			return false
		}
	}
	return true
}

const zeroHash = "0x0000000000000000000000000000000000000000000000000000000000000000"

func hexUint(v uint64) string { return fmt.Sprintf("0x%x", v) }

func stringParam(params []json.RawMessage, i int) string {
	if len(params) <= i {
		return ""
	}
	var s string
	if err := json.Unmarshal(params[i], &s); err != nil {
		return ""
	}
	return s
}

func addressParam(params []json.RawMessage) common.Address {
	return common.HexToAddress(stringParam(params, 0))
}

func receiptJSON(hash common.Hash, r *RawReceipt) map[string]any {
	return map[string]any{
		"transactionHash":   hash.Hex(),
		"transactionIndex":  "0x0",
		"blockHash":         zeroHash,
		"blockNumber":       hexUint(r.BlockNumber),
		"gasUsed":           hexUint(r.GasUsed),
		"cumulativeGasUsed": hexUint(r.GasUsed),
		"status":            hexUint(r.Status),
		"logsBloom":         "0x" + strings.Repeat("0", 512),
		"logs":              []any{},
	}
}

func (n *EVMNode) blockJSON() map[string]any {
	return map[string]any{
		"parentHash":       zeroHash,
		"sha3Uncles":       "0x1dcc4de8dec75d7aab85b567b6ccd41ad312451b948a7413f0a142fd40d49347",
		"miner":            "0x0000000000000000000000000000000000000000",
		"stateRoot":        zeroHash,
		"transactionsRoot": "0x56e81f171bcc55a6ff8345e692c0f86e5b48e01b996cadc001622fb5e363b421",
		"receiptsRoot":     "0x56e81f171bcc55a6ff8345e692c0f86e5b48e01b996cadc001622fb5e363b421",
		"logsBloom":        "0x" + strings.Repeat("0", 512),
		"difficulty":       "0x0",
		"number":           hexUint(max(n.blockNumber, 1)),
		"gasLimit":         hexUint(n.blockGasLim),
		"gasUsed":          "0x0",
		"timestamp":        hexUint(n.blockTime),
		"extraData":        "0x",
		"mixHash":          zeroHash,
		"nonce":            "0x0000000000000000",
		"baseFeePerGas":    "0x" + n.gasPrice.Text(16),
	}
}

// callResultAtBlock picks a SetCallResultForAt answer when the call names an
// explicit block number that has one for its selector.
func (n *EVMNode) callResultAtBlock(params []json.RawMessage) (string, bool) {
	if len(n.callAtBlock) == 0 {
		return "", false
	}
	block, ok := explicitCallBlock(params)
	if !ok {
		return "", false
	}
	bySelector := n.callAtBlock[block]
	if bySelector == nil {
		return "", false
	}
	var arg struct {
		Data  string `json:"data"`
		Input string `json:"input"`
	}
	if err := json.Unmarshal(params[0], &arg); err != nil {
		return "", false
	}
	data := arg.Data
	if data == "" {
		data = arg.Input
	}
	data = normalizeSelector(data)
	if len(data) < 8 {
		return "", false
	}
	result, ok := bySelector[data[:8]]
	return result, ok
}

// explicitCallBlock reads an eth_call's block parameter when it is a number,
// not a tag.
func explicitCallBlock(params []json.RawMessage) (uint64, bool) {
	if len(params) < 2 {
		return 0, false
	}
	var tag string
	if err := json.Unmarshal(params[1], &tag); err != nil || !strings.HasPrefix(tag, "0x") {
		return 0, false
	}
	block, ok := new(big.Int).SetString(strings.TrimPrefix(tag, "0x"), 16)
	if !ok || !block.IsUint64() {
		return 0, false
	}
	return block.Uint64(), true
}

// callResultForRequest picks a per-selector answer when one was registered.
func (n *EVMNode) callResultForRequest(params []json.RawMessage) (string, bool) {
	if len(n.callBySelector) == 0 {
		return "", false
	}
	if len(params) == 0 {
		return "", false
	}
	var arg struct {
		Data  string `json:"data"`
		Input string `json:"input"`
	}
	if err := json.Unmarshal(params[0], &arg); err != nil {
		return "", false
	}
	data := arg.Data
	if data == "" {
		data = arg.Input
	}
	data = normalizeSelector(data)
	if len(data) < 8 {
		return "", false
	}
	result, ok := n.callBySelector[data[:8]]
	return result, ok
}
