// Package witness holds the one witness type the relayer needs: the signature record it
// selects from a Tendermint commit and hands to the prover.
//
// The witness byte contract itself — the layout hashed in-circuit and by
// SignatureVerifier._hashWitness, the bucket family, the dummy padding — belongs to the
// prover and lives in the spectre-prover repository. The relayer never computes any of
// it: the prover pads, hashes and proves what it is sent.
package witness

// ValidatorSignature is one slot of a proof batch: the Ed25519 signature a validator
// produced over its canonical vote bytes, plus the metadata the on-chain quorum check
// needs. It is the unit of data crossing the boundary between the relayer (which selects
// signatures) and the prover.
type ValidatorSignature struct {
	Signature   []byte // 64 bytes: R || S
	PublicKey   []byte // 32 bytes: compressed Ed25519 A
	SignedBytes []byte // Commit.VoteSignBytes(chainID, idx) — what was actually signed
	Index       int    // position in the validator set
	Power       int64  // voting power: used by the relayer's selection, never sent to the prover
	// Active distinguishes real signers (true) from deterministic dummy padding
	// (false). The relayer submits only real signers; padding slots appear only in a
	// proof result.
	Active bool
}
