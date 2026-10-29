package client

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/cometbft/cometbft/crypto/ed25519"
	cmtprotocrypto "github.com/cometbft/cometbft/proto/tendermint/crypto"
	cmtproto "github.com/cometbft/cometbft/proto/tendermint/types"
	cmtversion "github.com/cometbft/cometbft/proto/tendermint/version"
	cmttypes "github.com/cometbft/cometbft/types"
	gogotypes "github.com/cosmos/gogoproto/types"
)

// The Solidity library contracts/light-clients/spectre/libraries/Encode.sol (and
// Header.sol on top of it) must produce the bytes CometBFT produces, because the
// light client recomputes header and validator-set hashes on-chain and compares
// them with what the validators signed. A mismatch of one byte rejects every
// valid header.
//
// This test is the Go half of that check. It computes the expected bytes with
// CometBFT's own types and writes them, together with the inputs, to
// test/fixtures/encode_vectors.json. The Foundry tests in
// test/light-clients/spectre/EncodeTest.t.sol read the same file and assert the
// Solidity output is identical.
//
// On every normal run the file is regenerated in memory and compared with the
// committed one, so a CometBFT upgrade that changes an encoding fails here
// first. Run with -update after an intentional change:
//
//	cd relayer && go test ./client -run TestEncodeVectors -update

var updateEncodeVectors = flag.Bool("update", false, "regenerate the Go↔Solidity encoding vectors")

// encodeVectorsPath is relative to this package directory.
const encodeVectorsPath = "../../test/fixtures/encode_vectors.json"

type versionVector struct {
	Block   uint64 `json:"block"`
	App     uint64 `json:"app"`
	Encoded string `json:"encoded"`
}

type validatorVector struct {
	PubKey      string `json:"pubKey"`
	VotingPower uint64 `json:"votingPower"`
	Encoded     string `json:"encoded"`
}

type blockIDVector struct {
	Hash    string `json:"hash"`
	Total   uint32 `json:"total"`
	PartSet string `json:"partSetHash"`
	Encoded string `json:"encoded"`
}

type partSetHeaderVector struct {
	Total   uint32 `json:"total"`
	Hash    string `json:"hash"`
	Encoded string `json:"encoded"`
}

type timestampVector struct {
	Nanos   uint64 `json:"nanos"`
	Encoded string `json:"encoded"`
}

type cdcStringVector struct {
	Value   string `json:"value"`
	Encoded string `json:"encoded"`
}

type cdcInt64Vector struct {
	Value   uint64 `json:"value"`
	Encoded string `json:"encoded"`
}

type cdcBytesVector struct {
	Value   string `json:"value"`
	Encoded string `json:"encoded"`
}

// valSetVector lists validators as two parallel arrays because Foundry's JSON
// cheatcodes read arrays of scalars in one call but arrays of objects one field
// at a time.
type valSetVector struct {
	Name         string   `json:"name"`
	PubKeys      []string `json:"pubKeys"`
	VotingPowers []uint64 `json:"votingPowers"`
	Hash         string   `json:"hash"`
}

// headerVector carries the same fields as ISpectreTendermintMsgs.BlockHeader.
// An optional hash is present when its Has flag is true; otherwise Go leaves it
// nil, which CometBFT hashes as an empty leaf.
type headerVector struct {
	Name               string `json:"name"`
	VersionBlock       uint64 `json:"versionBlock"`
	VersionApp         uint64 `json:"versionApp"`
	ChainID            string `json:"chainId"`
	Height             uint64 `json:"height"`
	TimeNanos          uint64 `json:"timeNanos"`
	HasLastBlockID     bool   `json:"hasLastBlockId"`
	LastBlockIDHash    string `json:"lastBlockIdHash"`
	LastPartSetTotal   uint32 `json:"lastPartSetTotal"`
	LastPartSetHash    string `json:"lastPartSetHash"`
	HasLastCommitHash  bool   `json:"hasLastCommitHash"`
	LastCommitHash     string `json:"lastCommitHash"`
	HasDataHash        bool   `json:"hasDataHash"`
	DataHash           string `json:"dataHash"`
	ValidatorsHash     string `json:"validatorsHash"`
	NextValidatorsHash string `json:"nextValidatorsHash"`
	ConsensusHash      string `json:"consensusHash"`
	AppHash            string `json:"appHash"`
	HasLastResultsHash bool   `json:"hasLastResultsHash"`
	LastResultsHash    string `json:"lastResultsHash"`
	HasEvidenceHash    bool   `json:"hasEvidenceHash"`
	EvidenceHash       string `json:"evidenceHash"`
	ProposerAddress    string `json:"proposerAddress"`
	Hash               string `json:"hash"`
}

// encodeVectorFile groups the vectors by Solidity function. Foundry's JSON
// cheatcodes address one value at a time and have no wildcard, so every array
// has an explicit count the Solidity side reads before walking it.
type encodeVectorFile struct {
	Counts struct {
		Versions    int `json:"versions"`
		Validators  int `json:"validators"`
		BlockIDs    int `json:"blockIds"`
		PartSets    int `json:"partSetHeaders"`
		Timestamps  int `json:"timestamps"`
		CDCStrings  int `json:"cdcStrings"`
		CDCInt64s   int `json:"cdcInt64s"`
		CDCBytes    int `json:"cdcBytes"`
		CDCBytes32s int `json:"cdcBytes32s"`
		ValSets     int `json:"valSets"`
		Headers     int `json:"headers"`
	} `json:"counts"`
	Versions       []versionVector       `json:"versions"`
	Validators     []validatorVector     `json:"validators"`
	BlockIDs       []blockIDVector       `json:"blockIds"`
	PartSetHeaders []partSetHeaderVector `json:"partSetHeaders"`
	Timestamps     []timestampVector     `json:"timestamps"`
	CDCStrings     []cdcStringVector     `json:"cdcStrings"`
	CDCInt64s      []cdcInt64Vector      `json:"cdcInt64s"`
	CDCBytes       []cdcBytesVector      `json:"cdcBytes"`
	CDCBytes32s    []cdcBytesVector      `json:"cdcBytes32s"`
	ValSets        []valSetVector        `json:"valSets"`
	Headers        []headerVector        `json:"headers"`
}

func hex0x(b []byte) string { return "0x" + hex.EncodeToString(b) }

// seq returns 32 bytes start, start+1, ... — the same values as _hash1().._hash7()
// in EncodeTest.t.sol (start 0x10, 0x20, ...).
func seq(start byte) []byte {
	b := make([]byte, 32)
	for i := range b {
		b[i] = start + byte(i)
	}
	return b
}

func mustMarshal(t *testing.T, m interface{ Marshal() ([]byte, error) }) []byte {
	t.Helper()
	b, err := m.Marshal()
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

func blockIDProto(hash []byte, total uint32, partHash []byte) *cmtproto.BlockID {
	id := cmttypes.BlockID{Hash: hash, PartSetHeader: cmttypes.PartSetHeader{Total: total, Hash: partHash}}
	p := id.ToProto()
	return &p
}

func buildEncodeVectors(t *testing.T) encodeVectorFile {
	t.Helper()
	var f encodeVectorFile

	pub1 := seq(0xaa)

	for _, c := range [][2]uint64{{11, 0}, {11, 2}, {1, 1}, {0, 0}, {300, 129}} {
		v := &cmtversion.Consensus{Block: c[0], App: c[1]}
		f.Versions = append(f.Versions, versionVector{c[0], c[1], hex0x(mustMarshal(t, v))})
	}

	// Voting powers around every varint width boundary, including 0 (omitted).
	for _, p := range []uint64{0, 1, 100, 127, 128, 16_384, 1_000_000, 1 << 40} {
		sv := &cmtproto.SimpleValidator{
			PubKey:      &cmtprotocrypto.PublicKey{Sum: &cmtprotocrypto.PublicKey_Ed25519{Ed25519: pub1}},
			VotingPower: int64(p),
		}
		f.Validators = append(f.Validators, validatorVector{hex0x(pub1), p, hex0x(mustMarshal(t, sv))})
	}

	for _, c := range []struct {
		hash, part []byte
		total      uint32
	}{{seq(0x20), seq(0x10), 1}, {seq(0x30), seq(0x40), 128}, {seq(0xf0), seq(0x01), 1 << 20}} {
		f.BlockIDs = append(f.BlockIDs, blockIDVector{hex0x(c.hash), c.total, hex0x(c.part), hex0x(mustMarshal(t, blockIDProto(c.hash, c.total, c.part)))})
		psh := cmttypes.PartSetHeader{Total: c.total, Hash: c.part}
		pb := psh.ToProto()
		f.PartSetHeaders = append(f.PartSetHeaders, partSetHeaderVector{c.total, hex0x(c.part), hex0x(mustMarshal(t, &pb))})
	}

	for _, n := range []uint64{
		0, 1, 999_999_999, 1_000_000_000, 1_700_000_000_000_000_000,
		1_704_067_200_123_456_789, 4_102_444_800_000_000_000,
	} {
		ts := time.Unix(int64(n/1_000_000_000), int64(n%1_000_000_000)).UTC()
		enc, err := gogotypes.StdTimeMarshal(ts)
		if err != nil {
			t.Fatalf("StdTimeMarshal(%d): %v", n, err)
		}
		f.Timestamps = append(f.Timestamps, timestampVector{n, hex0x(enc)})
	}

	// CometBFT's cdcEncode wraps a value in gogoproto's StringValue / Int64Value /
	// BytesValue before hashing; empty and zero values marshal to no bytes.
	for _, s := range []string{"", "a", "test-chain", "cosmoshub-4", "x-0123456789012345678901234567890123456789012345678"} {
		f.CDCStrings = append(f.CDCStrings, cdcStringVector{s, hex0x(mustMarshal(t, &gogotypes.StringValue{Value: s}))})
	}
	for _, n := range []uint64{0, 1, 127, 128, 255, 256, 300, 12_345, 16_384, 1_000_000, 1 << 40} {
		f.CDCInt64s = append(f.CDCInt64s, cdcInt64Vector{n, hex0x(mustMarshal(t, &gogotypes.Int64Value{Value: int64(n)}))})
	}
	for _, b := range [][]byte{nil, {0x01}, make([]byte, 20), bytes.Repeat([]byte{0xab}, 130)} {
		f.CDCBytes = append(f.CDCBytes, cdcBytesVector{hex0x(b), hex0x(mustMarshal(t, &gogotypes.BytesValue{Value: b}))})
	}
	for _, b := range [][]byte{make([]byte, 32), seq(0x50), bytes.Repeat([]byte{0xff}, 32)} {
		f.CDCBytes32s = append(f.CDCBytes32s, cdcBytesVector{hex0x(b), hex0x(mustMarshal(t, &gogotypes.BytesValue{Value: b}))})
	}

	// Validator sets: 1 leaf, 2, 3 and 5 (odd sizes exercise the merkle split point),
	// with powers across varint widths.
	for _, c := range []struct {
		name   string
		powers []uint64
	}{
		{"single", []uint64{10}},
		{"two", []uint64{100, 200}},
		{"three_mixedWidths", []uint64{1, 128, 1 << 40}},
		{"five", []uint64{7, 7, 7, 7, 7}},
	} {
		set := &cmttypes.ValidatorSet{}
		vec := valSetVector{Name: c.name}
		for i, p := range c.powers {
			pub := seq(byte(0xa0 + i*3))
			set.Validators = append(set.Validators, &cmttypes.Validator{PubKey: ed25519.PubKey(pub), VotingPower: int64(p)})
			vec.PubKeys = append(vec.PubKeys, hex0x(pub))
			vec.VotingPowers = append(vec.VotingPowers, p)
		}
		vec.Hash = hex0x(set.Hash())
		f.ValSets = append(f.ValSets, vec)
	}

	// A header whose last_block_id is empty (the first block of a chain) is
	// deliberately absent from these vectors: Header.sol hashes it as an empty
	// leaf, CometBFT as the two bytes in TestEmptyBlockIDMarshalsToTwoBytes, so
	// the two disagree. See that test.
	full := headerVector{
		Name: "full", VersionBlock: 11, VersionApp: 0, ChainID: "cosmoshub-4", Height: 12_345, TimeNanos: 1_700_000_000_000_000_000,
		HasLastBlockID: true, LastBlockIDHash: hex0x(seq(0x20)), LastPartSetTotal: 1, LastPartSetHash: hex0x(seq(0x10)),
		HasLastCommitHash: true, LastCommitHash: hex0x(seq(0x30)), HasDataHash: true, DataHash: hex0x(seq(0x40)),
		ValidatorsHash: hex0x(seq(0x50)), NextValidatorsHash: hex0x(seq(0x60)), ConsensusHash: hex0x(seq(0x70)),
		AppHash: hex0x(seq(0xa0)), HasLastResultsHash: true, LastResultsHash: hex0x(seq(0x10)),
		HasEvidenceHash: true, EvidenceHash: hex0x(seq(0x20)),
		ProposerAddress: "0xf0f1f2f3f4f5f6f7f8f9fafbfcfdfeff00010203",
	}
	optionalAbsent := full
	optionalAbsent.Name = "optionalHashesAbsent"
	optionalAbsent.HasLastCommitHash, optionalAbsent.LastCommitHash = false, hex0x(make([]byte, 32))
	optionalAbsent.HasDataHash, optionalAbsent.DataHash = false, hex0x(make([]byte, 32))
	optionalAbsent.HasLastResultsHash, optionalAbsent.LastResultsHash = false, hex0x(make([]byte, 32))
	optionalAbsent.HasEvidenceHash, optionalAbsent.EvidenceHash = false, hex0x(make([]byte, 32))
	zeroApp := full
	zeroApp.Name, zeroApp.AppHash = "zeroAppHash", hex0x(make([]byte, 32))
	wide := full
	wide.Name, wide.VersionBlock, wide.VersionApp = "wideHeightFractionalTimeAppVersion", 11, 2
	wide.ChainID, wide.Height, wide.TimeNanos = "a-chain-id-that-is-fifty-characters-long-0123456789ab", 1<<40+1, 1_704_067_200_123_456_789
	sameNextVals := full
	sameNextVals.Name, sameNextVals.NextValidatorsHash = "nextValidatorsEqualsValidators", full.ValidatorsHash

	zero32 := hex0x(make([]byte, 32))
	noCommit := full
	noCommit.Name, noCommit.HasLastCommitHash, noCommit.LastCommitHash = "onlyLastCommitHashAbsent", false, zero32
	noData := full
	noData.Name, noData.HasDataHash, noData.DataHash = "onlyDataHashAbsent", false, zero32
	noResults := full
	noResults.Name, noResults.HasLastResultsHash, noResults.LastResultsHash = "onlyLastResultsHashAbsent", false, zero32
	noEvidence := full
	noEvidence.Name, noEvidence.HasEvidenceHash, noEvidence.EvidenceHash = "onlyEvidenceHashAbsent", false, zero32

	for _, h := range []headerVector{full, optionalAbsent, zeroApp, wide, sameNextVals, noCommit, noData, noResults, noEvidence} {
		hdr := headerFromVector(t, h)
		h.Hash = hex0x(hdr.Hash())
		f.Headers = append(f.Headers, h)
	}

	f.Counts.Versions, f.Counts.Validators, f.Counts.BlockIDs, f.Counts.PartSets = len(f.Versions), len(f.Validators), len(f.BlockIDs), len(f.PartSetHeaders)
	f.Counts.Timestamps, f.Counts.CDCStrings, f.Counts.CDCInt64s = len(f.Timestamps), len(f.CDCStrings), len(f.CDCInt64s)
	f.Counts.CDCBytes, f.Counts.CDCBytes32s = len(f.CDCBytes), len(f.CDCBytes32s)
	f.Counts.ValSets, f.Counts.Headers = len(f.ValSets), len(f.Headers)
	return f
}

func unhex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s[2:])
	if err != nil {
		t.Fatalf("decode %q: %v", s, err)
	}
	return b
}

func headerFromVector(t *testing.T, h headerVector) cmttypes.Header {
	t.Helper()
	hdr := cmttypes.Header{
		Version:            cmtversion.Consensus{Block: h.VersionBlock, App: h.VersionApp},
		ChainID:            h.ChainID,
		Height:             int64(h.Height),
		Time:               time.Unix(int64(h.TimeNanos/1_000_000_000), int64(h.TimeNanos%1_000_000_000)).UTC(),
		ValidatorsHash:     unhex(t, h.ValidatorsHash),
		NextValidatorsHash: unhex(t, h.NextValidatorsHash),
		ConsensusHash:      unhex(t, h.ConsensusHash),
		AppHash:            unhex(t, h.AppHash),
		ProposerAddress:    unhex(t, h.ProposerAddress),
	}
	if h.HasLastBlockID {
		hdr.LastBlockID = cmttypes.BlockID{
			Hash:          unhex(t, h.LastBlockIDHash),
			PartSetHeader: cmttypes.PartSetHeader{Total: h.LastPartSetTotal, Hash: unhex(t, h.LastPartSetHash)},
		}
	}
	if h.HasLastCommitHash {
		hdr.LastCommitHash = unhex(t, h.LastCommitHash)
	}
	if h.HasDataHash {
		hdr.DataHash = unhex(t, h.DataHash)
	}
	if h.HasLastResultsHash {
		hdr.LastResultsHash = unhex(t, h.LastResultsHash)
	}
	if h.HasEvidenceHash {
		hdr.EvidenceHash = unhex(t, h.EvidenceHash)
	}
	return hdr
}

// TestEncodeVectors regenerates the vectors and compares them with the committed
// file. A failure means CometBFT's encoding (or this generator) changed: rerun
// with -update, then expect the Foundry tests to fail until Encode.sol / Header.sol
// match — or, if only Go drifted, until Go is fixed back.
func TestEncodeVectors(t *testing.T) {
	got, err := json.MarshalIndent(buildEncodeVectors(t), "", "  ")
	if err != nil {
		t.Fatalf("marshal vectors: %v", err)
	}
	got = append(got, '\n')

	path := filepath.Clean(encodeVectorsPath)
	if *updateEncodeVectors {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("create fixture dir: %v", err)
		}
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatalf("write vectors: %v", err)
		}
		t.Logf("regenerated %s", path)
		return
	}

	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read vectors (run with -update to generate): %v", err)
	}
	if !bytes.Equal(bytes.TrimSpace(want), bytes.TrimSpace(got)) {
		t.Fatalf("encoding vectors are stale: %s no longer matches what CometBFT produces.\n"+
			"If the change was intended, rerun with -update and make Encode.sol / Header.sol match in the same change.", path)
	}
}

// TestEmptyBlockIDMarshalsToTwoBytes pins the CometBFT behavior that Header.sol
// does not reproduce. BlockID.part_set_header is a non-nullable field, so an
// empty BlockID marshals to 0x12 0x00 rather than to no bytes, and
// Header.Hash() merkle-hashes those two bytes for last_block_id.
//
// Header.sol instead hashes a header with hasLastBlockId == false (which the
// relayer sets from LastBlockID.IsZero(), tendermint.go) as an empty leaf, so
// for a header with an empty last_block_id — the first block of a chain — the
// on-chain hash differs from the hash the validators signed. That is why no such
// header is in the vectors above: the Solidity test would fail.
func TestEmptyBlockIDMarshalsToTwoBytes(t *testing.T) {
	var id cmttypes.BlockID
	p := id.ToProto()
	if got := hex.EncodeToString(mustMarshal(t, &p)); got != "1200" {
		t.Fatalf("empty BlockID marshals to %s, want 1200", got)
	}

	hdr := headerFromVector(t, buildEncodeVectors(t).Headers[0])
	withZeroBlockID := hdr
	withZeroBlockID.LastBlockID = cmttypes.BlockID{}
	if bytes.Equal(hdr.Hash(), withZeroBlockID.Hash()) {
		t.Fatal("expected an empty last_block_id to change the header hash")
	}
}
