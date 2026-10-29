package transaction

import (
	"errors"
	"fmt"
	"strings"

	contractICS20Transfer "relayer/bindings/ICS20Transfer"
	contractICS26Router "relayer/bindings/ICS26Router"
	contractMembership "relayer/bindings/Membership"
	contractMisbehaviour "relayer/bindings/Misbehaviour"
	spectreContract "relayer/bindings/SpectreClient"
	contractUpdateClient "relayer/bindings/UpdateClient"

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/accounts/abi/bind"
	"github.com/ethereum/go-ethereum/common"
)

// knownRevertErrors indexes every custom error the contracts this relayer calls
// can revert with, by selector. A SpectreClient revert surfaces through the
// router unchanged, and a module revert through the client's delegatecall, so
// one flat index covers every call path.
var knownRevertErrors = buildRevertErrorIndex(
	spectreContract.ContractSpectreClientMetaData,
	contractICS26Router.ContractICS26RouterMetaData,
	contractICS20Transfer.ContractICS20TransferMetaData,
	contractUpdateClient.ContractUpdateClientMetaData,
	contractMembership.ContractMembershipMetaData,
	contractMisbehaviour.ContractMisbehaviourMetaData,
)

func buildRevertErrorIndex(metas ...*bind.MetaData) map[[4]byte]abi.Error {
	index := make(map[[4]byte]abi.Error)
	// Solidity's built-in require-string and panic errors are in no contract ABI.
	stringType, _ := abi.NewType("string", "", nil)
	uint256Type, _ := abi.NewType("uint256", "", nil)
	for _, e := range []abi.Error{
		abi.NewError("Error", abi.Arguments{{Name: "reason", Type: stringType}}),
		abi.NewError("Panic", abi.Arguments{{Name: "code", Type: uint256Type}}),
	} {
		var selector [4]byte
		copy(selector[:], e.ID[:4])
		index[selector] = e
	}
	for _, meta := range metas {
		parsed, err := meta.GetAbi()
		if err != nil {
			// The bindings are generated from compiled ABIs; a parse failure is a
			// broken build, and decoding is diagnostics only, so skip rather than
			// take the sender down.
			continue
		}
		for _, e := range parsed.Errors {
			var selector [4]byte
			copy(selector[:], e.ID[:4])
			index[selector] = e
		}
	}
	return index
}

// revertData extracts the revert data a node attached to a failed eth_call, or
// nil when it attached none.
func revertData(err error) []byte {
	var dataErr interface{ ErrorData() interface{} }
	if !errors.As(err, &dataErr) {
		return nil
	}
	switch raw := dataErr.ErrorData().(type) {
	case string:
		return common.FromHex(revertHex(raw))
	case fmt.Stringer:
		return common.FromHex(revertHex(raw.String()))
	case []byte:
		return raw
	default:
		return nil
	}
}

// revertHex strips the text some nodes put before the hex: Nethermind and
// Erigon send "Reverted 0x..." and others "execution reverted: 0x...", where
// geth sends "0x...". It strips whatever precedes the first 0x when that text
// names a revert -- the same test services.isRevertData applies when it decides
// the data is revert data -- and leaves anything else alone. Handed to
// common.FromHex whole, such data decoded to garbage and the contract's reason
// was lost (review of 65786fb).
//
// The index is taken on raw itself, not on a lowercased copy, so it is a valid
// offset into raw whatever the prefix's encoding.
func revertHex(raw string) string {
	raw = strings.TrimSpace(raw)
	i := strings.Index(raw, "0x")
	if j := strings.Index(raw, "0X"); j >= 0 && (i < 0 || j < i) {
		i = j
	}
	if i > 0 && strings.Contains(strings.ToLower(raw[:i]), "revert") {
		return raw[i:]
	}
	return raw
}

// decodeRevert names the custom error in a failed call's revert data, with its
// arguments: "InsufficientTrustingPeriod(4042888, 57600)". ok is false when there
// is no data, or the selector is not one of ours (the caller then has only the
// node's own message, usually a bare "execution reverted").
//
// Without this, every SpectreClient revert logged as "execution reverted" and
// the cause took a manual `cast run` to find (#541).
func decodeRevert(err error) (string, bool) {
	data := revertData(err)
	if len(data) < 4 {
		return "", false
	}
	var selector [4]byte
	copy(selector[:], data[:4])
	e, ok := knownRevertErrors[selector]
	if !ok {
		return fmt.Sprintf("unknown custom error 0x%x", selector), true
	}
	args, unpackErr := e.Inputs.Unpack(data[4:])
	if unpackErr != nil {
		return fmt.Sprintf("%s(<undecodable arguments: %v>)", e.Name, unpackErr), true
	}
	parts := make([]string, len(args))
	for i, arg := range args {
		parts[i] = formatRevertArg(arg)
	}
	return e.Name + "(" + strings.Join(parts, ", ") + ")", true
}

func formatRevertArg(arg interface{}) string {
	switch v := arg.(type) {
	case [32]byte:
		return common.Hash(v).Hex()
	case []byte:
		return "0x" + common.Bytes2Hex(v)
	default:
		return fmt.Sprintf("%v", v)
	}
}

// describeRevert is the node's message plus the decoded custom error when there
// is one: what an operator needs in the log line, in one place.
func describeRevert(err error) string {
	if decoded, ok := decodeRevert(err); ok {
		return err.Error() + ": " + decoded
	}
	return err.Error()
}
