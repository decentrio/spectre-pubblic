package main

import (
	"encoding/json"
	"fmt"
	"os"
)

// The hand-written JSON editor behind create-clients: it writes one member
// back into config.json and must PRESERVE the operator's formatting, which is why
// there is a byte-level JSON parser here (skipJSONSpace, scanJSONStringEnd,
// skipJSONValue, findJSONObjectMember, insertJSONObjectMember) rather than a
// round trip through encoding/json -- that would rewrite the whole file. A
// hand-written parser is also where subtle bugs live, and #310 was one in this
// exact area, so it is worth having it findable by file name. Split from
// config.go (E6): schema and loading live there, editing lives here.

// writeConfigMember rewrites configPath in place, setting
// modules[name=="cosmos_to_eth" && matches sourceClientID].config.<member> =
// value. An empty sourceClientID targets the first cosmos_to_eth module. Other
// fields and existing JSON formatting are preserved outside the value.
func writeConfigMember(configPath, sourceClientID, member, value string) error {
	return writeConfigMemberIn(configPath, sourceClientID, member, value, dirCosmosToEth, dirCosmosToL2)
}

// writeConfigMemberIn is writeConfigMember restricted to the given module directions.
// Callers that know which side they are writing MUST name it: a cosmos_to_eth and a
// cosmos_to_l2 module can share an ics26_client_id, so an unrestricted search resolves
// the ambiguity by file order rather than by intent.
func writeConfigMemberIn(configPath, sourceClientID, member, value string, dirs ...moduleDirection) error {
	data, err := os.ReadFile(configPath)
	if err != nil {
		return err
	}
	out, err := replaceConfigMemberForSourceIn(data, sourceClientID, member, value, dirs)
	if err != nil {
		return err
	}
	if err := os.WriteFile(configPath, out, configFilePerm); err != nil {
		return err
	}
	return os.Chmod(configPath, configFilePerm)
}

// assertConfigMemberWritable reports whether writeConfigMemberIn would find a
// destination for member, without touching the file.
//
// It resolves through the same replaceConfigMemberForSourceIn the real write uses
// rather than re-implementing the lookup, so the check cannot drift from the write
// it is guarding. A separately-written precheck that disagreed would be worse than
// none: it would pass, the write would still fail, and the failure would once again
// land after the irreversible step.
func assertConfigMemberWritable(configPath, sourceClientID, member string, dirs ...moduleDirection) error {
	data, err := os.ReadFile(configPath)
	if err != nil {
		return err
	}
	_, err = replaceConfigMemberForSourceIn(data, sourceClientID, member, "", dirs)
	return err
}

// unrecordedClientError explains a client that exists on chain but is not in the
// config, and names the one recovery that does not create a second one.
//
// Re-running create-clients-cosmos is the natural reaction to a non-zero exit and
// the wrong one: it creates another client, pays for it again, and orphans this one
// with nothing advancing it until it expires (#310).
func unrecordedClientError(configPath, wasmClientID string, cause error) error {
	return fmt.Errorf(
		"created Ethereum light client %s on Cosmos but could not record it in %s: %w\n"+
			"  The client exists and is usable. Set cosmos_wasm_client_id=%s on the cosmos_to_eth "+
			"module by hand, then run create-clients-eth.\n"+
			"  Do NOT re-run create-clients-cosmos: it would create a second client and leave %s orphaned",
		wasmClientID, configPath, cause, wasmClientID, wasmClientID)
}

// writeSpectreClientAddress persists the deployed SpectreClient light-client
// address (ETH side) back into the sourceClientID module.
func writeSpectreClientAddress(configPath, sourceClientID, addr string) error {
	return writeConfigMember(configPath, sourceClientID, "spectre_client", addr)
}

// writeEthWasmClientID persists the created 08-wasm Ethereum light-client id into
// the cosmos_to_eth module for sourceClientID. The id is assigned by ibc-go's global
// client sequence, so it cannot be known until MsgCreateClient lands — write it back
// rather than requiring the operator to predict it.
//
// The direction is pinned deliberately. A cosmos_to_eth and a cosmos_to_l2 module may
// legitimately carry the SAME ics26_client_id (config.example.json ships exactly that:
// both say "cosmoshub-1"), so a direction-agnostic search matches whichever appears
// first in the file. That silently wrote the L2 client id into the Ethereum module and
// left the L2 module untouched, breaking the counterparty wiring create-clients-eth
// reads next.
func writeEthWasmClientID(configPath, sourceClientID, id string) error {
	return writeConfigMemberIn(configPath, sourceClientID, "cosmos_wasm_client_id", id, dirCosmosToEth)
}

// writeL2WasmClientID persists the created L2 wasm client id into the cosmos_to_l2
// module for sourceClientID. See writeEthWasmClientID for why the direction is pinned.
func writeL2WasmClientID(configPath, sourceClientID, id string) error {
	return writeConfigMemberIn(configPath, sourceClientID, "cosmos_wasm_client_id", id, dirCosmosToL2)
}

// replaceConfigMember targets the first cosmos_to_eth module. Retained for the
// single-source call sites and tests.
func replaceConfigMember(data []byte, member, value string) ([]byte, error) {
	return replaceConfigMemberForSource(data, "", member, value)
}

// moduleSourceClientID resolves the source id of a cosmos_to_eth module: its
// config.ics26_client_id, falling back to the module-level src_chain — mirroring
// how loadConfig defaults ICS26ClientID.
func moduleSourceClientID(data []byte, moduleStart, configStart int) (string, error) {
	if s, e, ok, err := findJSONObjectMember(data, configStart, "ics26_client_id"); err != nil {
		return "", err
	} else if ok {
		var id string
		if err := json.Unmarshal(data[s:e], &id); err != nil {
			return "", fmt.Errorf("parse ics26_client_id: %w", err)
		}
		if id != "" {
			return id, nil
		}
	}
	if s, e, ok, err := findJSONObjectMember(data, moduleStart, "src_chain"); err != nil {
		return "", err
	} else if ok {
		var id string
		if err := json.Unmarshal(data[s:e], &id); err != nil {
			return "", fmt.Errorf("parse src_chain: %w", err)
		}
		return id, nil
	}
	return "", nil
}

// replaceConfigMemberForSource sets config.<member> on the cosmos_to_eth module
// whose source id equals sourceClientID (or the first cosmos_to_eth module when
// sourceClientID is empty), preserving the surrounding JSON formatting.
func replaceConfigMemberForSource(data []byte, sourceClientID, member, value string) ([]byte, error) {
	return replaceConfigMemberForSourceIn(data, sourceClientID, member, value,
		[]moduleDirection{dirCosmosToEth, dirCosmosToL2})
}

// replaceConfigMemberForSourceIn is replaceConfigMemberForSource restricted to dirs.
func replaceConfigMemberForSourceIn(data []byte, sourceClientID, member, value string, dirs []moduleDirection) ([]byte, error) {
	encodedValue, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}

	rootStart := skipJSONSpace(data, 0)
	if rootStart >= len(data) || data[rootStart] != '{' {
		return nil, fmt.Errorf("config root is not an object")
	}
	modulesStart, modulesEnd, ok, err := findJSONObjectMember(data, rootStart, "modules")
	if err != nil {
		return nil, err
	}
	if !ok || modulesStart >= len(data) || data[modulesStart] != '[' {
		return nil, fmt.Errorf("config has no modules array")
	}

	i := skipJSONSpace(data, modulesStart+1)
	for i < modulesEnd {
		if data[i] == ']' {
			break
		}
		if data[i] != '{' {
			return nil, fmt.Errorf("module entry is not an object")
		}

		moduleStart := i
		moduleEnd, err := skipJSONValue(data, moduleStart)
		if err != nil {
			return nil, err
		}

		// Classify this module by the SAME rule loadConfig/classifyModule uses
		// (canonical src_chain/dst_chain first, legacy name fallback), NOT by a raw
		// name == "cosmos_to_eth" check. A canonical config's name is a free-form
		// label (e.g. "cosmos-to-eth"), so a name check would miss it and write-back
		// would fail with "not found" after create-clients mutated chain state.
		name, err := moduleStringMember(data, moduleStart, "name")
		if err != nil {
			return nil, err
		}
		srcChain, err := moduleStringMember(data, moduleStart, "src_chain")
		if err != nil {
			return nil, err
		}
		dstChain, err := moduleStringMember(data, moduleStart, "dst_chain")
		if err != nil {
			return nil, err
		}
		dir, _, cerr := classifyModule(configModule{Name: name, SrcChain: srcChain, DstChain: dstChain})
		// create-clients-eth writes spectre_client back into either a cosmos_to_eth
		// source or a cosmos_to_l2 destination (both hold a SpectreClient on the EVM
		// side, created by the same flow), so accept both directions here.
		accepted := false
		for _, d := range dirs {
			if dir == d {
				accepted = true
				break
			}
		}
		if cerr == nil && accepted {
			configStart, configEnd, ok, err := findJSONObjectMember(data, moduleStart, "config")
			if err != nil {
				return nil, err
			}
			if !ok || configStart >= len(data) || data[configStart] != '{' {
				return nil, fmt.Errorf("%s.config is not an object", dir)
			}

			matches := sourceClientID == ""
			if !matches {
				id, err := moduleSourceClientID(data, moduleStart, configStart)
				if err != nil {
					return nil, err
				}
				matches = id == sourceClientID
			}
			if matches {
				valueStart, valueEnd, ok, err := findJSONObjectMember(data, configStart, member)
				if err != nil {
					return nil, err
				}
				if ok {
					out := make([]byte, 0, len(data)-valueEnd+valueStart+len(encodedValue))
					out = append(out, data[:valueStart]...)
					out = append(out, encodedValue...)
					out = append(out, data[valueEnd:]...)
					return out, nil
				}
				return insertJSONObjectMember(data, configStart, configEnd, member, encodedValue)
			}
		}

		i = skipJSONSpace(data, moduleEnd)
		if i < len(data) && data[i] == ',' {
			i = skipJSONSpace(data, i+1)
			continue
		}
		if i < len(data) && data[i] == ']' {
			break
		}
	}

	if sourceClientID != "" {
		return nil, fmt.Errorf("cosmos_to_eth source %q not found in config", sourceClientID)
	}
	return nil, fmt.Errorf("module cosmos_to_eth not found in config")
}

// moduleStringMember reads an optional string member of the raw JSON module object
// at moduleStart, returning "" when the member is absent (so classification can run
// on whatever subset of name/src_chain/dst_chain the config carries).
func moduleStringMember(data []byte, moduleStart int, key string) (string, error) {
	start, end, ok, err := findJSONObjectMember(data, moduleStart, key)
	if err != nil {
		return "", err
	}
	if !ok {
		return "", nil
	}
	var s string
	if err := json.Unmarshal(data[start:end], &s); err != nil {
		return "", fmt.Errorf("parse module %s: %w", key, err)
	}
	return s, nil
}

func skipJSONSpace(data []byte, i int) int {
	for i < len(data) {
		switch data[i] {
		case ' ', '\n', '\r', '\t':
			i++
		default:
			return i
		}
	}
	return i
}

func scanJSONStringEnd(data []byte, i int) (int, error) {
	if i >= len(data) || data[i] != '"' {
		return 0, fmt.Errorf("expected JSON string")
	}
	escaped := false
	for j := i + 1; j < len(data); j++ {
		if escaped {
			escaped = false
			continue
		}
		switch data[j] {
		case '\\':
			escaped = true
		case '"':
			return j + 1, nil
		}
	}
	return 0, fmt.Errorf("unterminated JSON string")
}

func skipJSONValue(data []byte, i int) (int, error) {
	i = skipJSONSpace(data, i)
	if i >= len(data) {
		return 0, fmt.Errorf("unexpected end of JSON")
	}

	switch data[i] {
	case '"':
		return scanJSONStringEnd(data, i)
	case '{':
		j := skipJSONSpace(data, i+1)
		if j < len(data) && data[j] == '}' {
			return j + 1, nil
		}
		for {
			keyEnd, err := scanJSONStringEnd(data, j)
			if err != nil {
				return 0, err
			}
			j = skipJSONSpace(data, keyEnd)
			if j >= len(data) || data[j] != ':' {
				return 0, fmt.Errorf("expected ':' after object key")
			}
			j, err = skipJSONValue(data, j+1)
			if err != nil {
				return 0, err
			}
			j = skipJSONSpace(data, j)
			if j >= len(data) {
				return 0, fmt.Errorf("unterminated JSON object")
			}
			if data[j] == '}' {
				return j + 1, nil
			}
			if data[j] != ',' {
				return 0, fmt.Errorf("expected ',' or '}' in object")
			}
			j = skipJSONSpace(data, j+1)
		}
	case '[':
		j := skipJSONSpace(data, i+1)
		if j < len(data) && data[j] == ']' {
			return j + 1, nil
		}
		for {
			var err error
			j, err = skipJSONValue(data, j)
			if err != nil {
				return 0, err
			}
			j = skipJSONSpace(data, j)
			if j >= len(data) {
				return 0, fmt.Errorf("unterminated JSON array")
			}
			if data[j] == ']' {
				return j + 1, nil
			}
			if data[j] != ',' {
				return 0, fmt.Errorf("expected ',' or ']' in array")
			}
			j = skipJSONSpace(data, j+1)
		}
	default:
		j := i
		for j < len(data) {
			switch data[j] {
			case ' ', '\n', '\r', '\t', ',', '}', ']':
				return j, nil
			default:
				j++
			}
		}
		return j, nil
	}
}

func findJSONObjectMember(data []byte, objectStart int, key string) (int, int, bool, error) {
	if objectStart >= len(data) || data[objectStart] != '{' {
		return 0, 0, false, fmt.Errorf("expected JSON object")
	}
	i := skipJSONSpace(data, objectStart+1)
	if i < len(data) && data[i] == '}' {
		return 0, 0, false, nil
	}
	for {
		keyStart := i
		keyEnd, err := scanJSONStringEnd(data, keyStart)
		if err != nil {
			return 0, 0, false, err
		}
		var got string
		if err := json.Unmarshal(data[keyStart:keyEnd], &got); err != nil {
			return 0, 0, false, fmt.Errorf("parse object key: %w", err)
		}
		i = skipJSONSpace(data, keyEnd)
		if i >= len(data) || data[i] != ':' {
			return 0, 0, false, fmt.Errorf("expected ':' after object key")
		}
		valueStart := skipJSONSpace(data, i+1)
		valueEnd, err := skipJSONValue(data, valueStart)
		if err != nil {
			return 0, 0, false, err
		}
		if got == key {
			return valueStart, valueEnd, true, nil
		}
		i = skipJSONSpace(data, valueEnd)
		if i >= len(data) {
			return 0, 0, false, fmt.Errorf("unterminated JSON object")
		}
		if data[i] == '}' {
			return 0, 0, false, nil
		}
		if data[i] != ',' {
			return 0, 0, false, fmt.Errorf("expected ',' or '}' in object")
		}
		i = skipJSONSpace(data, i+1)
	}
}

func insertJSONObjectMember(data []byte, objectStart, objectEnd int, key string, encodedValue []byte) ([]byte, error) {
	if objectEnd <= objectStart || objectEnd > len(data) || data[objectEnd-1] != '}' {
		return nil, fmt.Errorf("invalid object range")
	}
	encodedKey, err := json.Marshal(key)
	if err != nil {
		return nil, err
	}
	closeIndex := objectEnd - 1
	empty := skipJSONSpace(data, objectStart+1) == closeIndex

	separator := []byte(",")
	if empty {
		separator = nil
	}
	insert := make([]byte, 0, len(separator)+len(encodedKey)+len(encodedValue)+2)
	insert = append(insert, separator...)
	insert = append(insert, encodedKey...)
	insert = append(insert, ':', ' ')
	insert = append(insert, encodedValue...)

	out := make([]byte, 0, len(data)+len(insert))
	out = append(out, data[:closeIndex]...)
	out = append(out, insert...)
	out = append(out, data[closeIndex:]...)
	return out, nil
}
