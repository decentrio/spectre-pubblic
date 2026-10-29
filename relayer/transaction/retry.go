// This file answers one question for both chains: what must the NEXT attempt
// change?
//
// It replaces gas_revert.go and cosmos_oog.go, which were named after the
// situations that produced them -- a revert attributed to gas on EVM, an
// out-of-gas on Cosmos -- rather than after what they decide. Naming by
// situation is why they were two files: every new situation would have added a
// third. Naming by the decision means one file, and it is the same decision C1
// turns into a Retry type, so the file already has the name that type will want.
//
// Merging also puts the two mirrors next to each other, which is what the
// symmetry rule in non-functional requirement #5 asks for.
package transaction

import (
	"context"
	"errors"
	"log"
	"math"
	"math/big"
	"math/bits"
	"relayer/chain"
	"strings"
	"time"

	"relayer/services"

	errortypes "github.com/cosmos/cosmos-sdk/types/errors"
	"github.com/ethereum/go-ethereum"
)

const (
	outOfGasThresholdBasisPoints = 9800
	gasProbeMultiplier           = 8
	ethRevertProbeTimeout        = 15 * time.Second
)

var evmGasHeadroomBasisPoints = []uint64{10_000, 15_000, 20_000, 30_000}

type ethRevertClass int

const (
	revertLogical ethRevertClass = iota
	revertOutOfGas
)

func applyEVMGasHeadroom(gas uint64, step int) uint64 {
	if step < 0 || step >= len(evmGasHeadroomBasisPoints) {
		step = 0
	}
	factor := evmGasHeadroomBasisPoints[step]
	hi, lo := bits.Mul64(gas, factor)
	if hi >= 10_000 {
		return ^uint64(0)
	}
	scaled, _ := bits.Div64(hi, lo, 10_000)
	return scaled
}

// classifyEthRevert replays a reverted call at the same block with more gas.
// The replay, not receipt utilization, is the primary OOG signal: EIP-150 can
// leave a nested multicall failure below a 99 percent utilization threshold.
func classifyEthRevert(
	stdCtx context.Context,
	endpoint services.EVMEndpoint,
	callMsg ethereum.CallMsg,
	blockNumber *big.Int,
	replayErr error,
	gasUsed, gasLimit uint64,
) ethRevertClass {
	if revertDataPresent(replayErr) {
		return revertLogical
	}

	probeGas := gasLimit
	if probeGas == 0 {
		probeGas = gasUsed
	}
	probeGas *= gasProbeMultiplier

	probeCtx, cancel := context.WithTimeout(stdCtx, ethRevertProbeTimeout)
	defer cancel()
	if header, err := endpoint.EthClient().HeaderByNumber(probeCtx, blockNumber); err == nil && header.GasLimit > 0 && probeGas > header.GasLimit {
		log.Printf("[SendEthTx] revert probe: clamping %d gas to block limit %d", probeGas, header.GasLimit)
		probeGas = header.GasLimit
	}

	probeMsg := callMsg
	probeMsg.Gas = probeGas
	// Fees are irrelevant to eth_call and can cause a large diagnostic probe to
	// be rejected for account balance rather than actually executed.
	probeMsg.GasPrice = nil
	probeMsg.GasFeeCap = nil
	probeMsg.GasTipCap = nil

	_, probeErr := endpoint.EthClient().CallContract(probeCtx, probeMsg, blockNumber)
	switch {
	case probeErr == nil:
		return revertOutOfGas
	case revertDataPresent(probeErr):
		return revertLogical
	case isExecutionOutOfGas(probeErr):
		return revertOutOfGas
	case isGasAllowanceRejection(probeErr), isProbeTransportError(probeErr):
		if outOfGasByUtilization(gasUsed, gasLimit) {
			return revertOutOfGas
		}
		return revertLogical
	default:
		return revertLogical
	}
}

func outOfGasByUtilization(gasUsed, gasLimit uint64) bool {
	return gasLimit > 0 && gasUsed <= gasLimit && gasUsed*10_000 >= gasLimit*outOfGasThresholdBasisPoints
}

func revertDataPresent(err error) bool {
	if err == nil {
		return false
	}
	var dataErr interface{ ErrorData() interface{} }
	if !errors.As(err, &dataErr) {
		return false
	}
	switch data := dataErr.ErrorData().(type) {
	case nil:
		return false
	case string:
		return data != "" && data != "0x"
	default:
		return true
	}
}

func isExecutionOutOfGas(err error) bool {
	return err != nil && !revertDataPresent(err) && strings.Contains(strings.ToLower(err.Error()), "out of gas")
}

func isGasAllowanceRejection(err error) bool {
	if err == nil || revertDataPresent(err) {
		return false
	}
	msg := strings.ToLower(err.Error())
	for _, phrase := range []string{"gas required exceeds", "exceeds block gas limit", "intrinsic gas too low"} {
		if strings.Contains(msg, phrase) {
			return true
		}
	}
	return false
}

func isProbeTransportError(err error) bool {
	if err == nil || revertDataPresent(err) {
		return false
	}
	msg := strings.ToLower(err.Error())
	for _, phrase := range []string{
		"execution reverted", "out of gas", "invalid opcode",
		"gas required exceeds", "exceeds block gas limit", "intrinsic gas too low",
	} {
		if strings.Contains(msg, phrase) {
			return false
		}
	}
	return true
}

type gasLimitKnob string

const (
	knobEthGasLimit       gasLimitKnob = "ETH_GAS_LIMIT"
	knobMulticallGasLimit gasLimitKnob = "ETH_MULTICALL_GAS_LIMIT"
	knobMisbehaviour      gasLimitKnob = "ETH_MISBEHAVIOUR_GAS_LIMIT"
	knobDeployEstimate    gasLimitKnob = "the deploy gas estimate (estimateCosmosClientDeployGas)"
	knobFixedClientLimit  gasLimitKnob = "the hardcoded client-call gas limit in handler.go"
)

var cosmosGasHeadroom = []float64{1.3, 2.0, 3.0}

// applyCosmosGasHeadroom scales both simulated and fallback limits. Applying it
// only after successful simulation made a simulation-failure retry identical:
// an included out-of-gas singleton climbed the ladder while reusing the same
// fallback gas on every step.
func applyCosmosGasHeadroom(gas uint64, headroomStep int) uint64 {
	if headroomStep < 0 || headroomStep >= len(cosmosGasHeadroom) {
		headroomStep = 0
	}
	factor := cosmosGasHeadroom[headroomStep]
	maxUint64 := ^uint64(0)
	if gas > 0 && float64(gas) >= float64(maxUint64)/factor {
		return maxUint64
	}
	return uint64(float64(gas) * factor)
}

// applyCosmosFeeHeadroom preserves the configured fee on the historical first
// attempt, then scales it by the same relative factor as the gas ladder. This
// keeps the configured gas-price ratio from shrinking on CheckTx OOG retries.
func applyCosmosFeeHeadroom(fee int64, headroomStep int) int64 {
	if fee <= 0 {
		return fee
	}
	if headroomStep < 0 || headroomStep >= len(cosmosGasHeadroom) {
		headroomStep = 0
	}
	ratio := cosmosGasHeadroom[headroomStep] / cosmosGasHeadroom[0]
	const maxInt64 = int64(^uint64(0) >> 1)
	if float64(fee) >= float64(maxInt64)/ratio {
		return maxInt64
	}
	return int64(math.Ceil(float64(fee) * ratio))
}

// The knobs the ladders below turn. They are named in the units an operator
// reads in a log line, and they are constants because the call site switches on
// which knob a plan chose -- a typo in a literal there would silently pick the
// wrong branch.
const (
	knobBatchSize = "messages per batch"
	knobCosmosGas = "cosmos gas limit"
	knobEVMGas    = "eth gas limit"
)

func cosmosOutOfGas(codespace string, code uint32) bool {
	return codespace == errortypes.ErrOutOfGas.Codespace() && code == errortypes.ErrOutOfGas.ABCICode()
}

// The change ladders this package owns, expressed as chain.ChangeFunc so the
// engine can walk them without knowing what a basis point is.
//
// They are C1's third error class in transaction/: an out-of-gas failure is
// neither transient (retrying the same gas fails the same way) nor permanent
// (more gas would work). Handing back a ladder is what lets chain.Climb say
// "retry, changed" and, at the bottom rung, "now it is permanent" in ONE place
// instead of at each of the three call sites that used to write that branch out.
//
// Each is a pure function of the attempt number, which is what lets a test walk
// one without a chain: change(0), change(1), change(2), and the last false.

// planCosmosOutOfGas describes what an out-of-gas Cosmos transaction must change
// next: a splittable batch halves, an atomic one climbs the finite gas ladder.
// Which was chosen is readable off the returned Attempt's What, so the call site
// acts on the change itself rather than on an enum only it knows how to read.
//
// There is deliberately no "give up" arm. Exhaustion is what chain.Climb reports
// when the chosen ladder runs out, which is why the two call sites below no
// longer each carry a copy of that decision.
//
// base is the gas limit the failed attempt used, so the ladder is anchored to
// what actually ran rather than to a simulation that already proved too low.
//
// It returns the position to climb from as well as the ladder, because the two
// ladders count different things. headroomStep counts gas escalations; splitting
// does not advance it, and must not be read as though it did -- a batch that
// reached step 2 by another route would otherwise be asked for the third rung of
// the batch ladder and be told the ladder was exhausted while it still had eight
// messages to halve. Each split re-derives the ladder from the new message
// count, so its position is always the first rung.
func planCosmosOutOfGas(cause error, msgCount, headroomStep int, base, maxBlockGas uint64, allowSplit bool) (chain.Retry, int) {
	if allowSplit && msgCount > 1 {
		return chain.NeedsChange(cause, CosmosBatchLadder(msgCount)), 0
	}
	return chain.NeedsChange(cause, CosmosGasLadder(base, maxBlockGas)), headroomStep
}

// CosmosGasLadder returns the gas limit for the attempt after `attempt`
// failures, anchored at base.
//
// maxBlockGas is a clamp, not a cut-off: an attempt that would exceed the block
// limit is worth making at exactly the limit, because that is still more gas
// than the failed one used. What ends the ladder is the rung being no larger
// than the one before it -- either the finite headroom list ran out, or the
// clamp has flattened two rungs onto the same number, and "retry with the
// identical gas" is precisely the infinite loop this class exists to prevent.
func CosmosGasLadder(base, maxBlockGas uint64) chain.ChangeFunc {
	clamp := func(step int) uint64 {
		gas := applyCosmosGasHeadroom(base, step)
		if maxBlockGas > 0 && gas > maxBlockGas {
			return maxBlockGas
		}
		return gas
	}
	return func(attempt int) (chain.Attempt, bool) {
		step := attempt + 1
		if attempt < 0 || step >= len(cosmosGasHeadroom) {
			return chain.Attempt{}, false
		}
		gas := clamp(step)
		if gas <= clamp(step-1) {
			return chain.Attempt{}, false
		}
		return chain.Attempt{What: knobCosmosGas, To: gas}, true
	}
}

// CosmosBatchLadder halves a non-atomic batch, down to one message.
//
// It stops at one rather than zero: a single message that still runs out of gas
// is not a batching problem, and planCosmosOutOfGas hands it to CosmosGasLadder
// instead. That hand-off is the decision the old planCosmosOutOfGas made inside
// one enum; stating it as two ladders is what lets each be walked on its own.
func CosmosBatchLadder(msgCount int) chain.ChangeFunc {
	if msgCount < 0 {
		msgCount = 0
	}
	return chain.HalvingLadder(knobBatchSize, uint64(msgCount))
}

// EVMGasLadder returns the gas limit for the attempt after `attempt` failures,
// anchored at base.
//
// The block ceiling is NOT folded in here, unlike the Cosmos ladder. Hitting the
// chain's own limit is a different diagnosis from exhausting our headroom list --
// one is fixed by a smaller batch, the other by a larger constant -- and the call
// site checks it first so the actionable reason is not masked by generic
// exhaustion. The call site clamps the rung this returns.
func EVMGasLadder(base uint64) chain.ChangeFunc {
	return func(attempt int) (chain.Attempt, bool) {
		step := attempt + 1
		if attempt < 0 || step >= len(evmGasHeadroomBasisPoints) {
			return chain.Attempt{}, false
		}
		return chain.Attempt{What: knobEVMGas, To: applyEVMGasHeadroom(base, step)}, true
	}
}
