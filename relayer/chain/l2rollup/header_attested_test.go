package l2rollup

import (
	"attestor/types/attestation"
	attestorpb "attestor/types/attestor"
	"context"
	"errors"
	"fmt"
	"math/big"
	"relayer/chain"
	"sort"
	"strings"
	"testing"
	"time"

	ethcommon "github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/ethclient"
)

// testL2Header is a minimal execution header; only Root and the derived Hash matter
// to the attestation check.
func testL2Header(t *testing.T) *types.Header {
	t.Helper()
	return &types.Header{
		Number:     big.NewInt(4096),
		Root:       [32]byte{0xaa, 0xbb},
		ParentHash: [32]byte{0x01},
		GasLimit:   30_000_000,
		GasUsed:    21_000,
	}
}

func testSigningBuilder(t *testing.T, attestors ...*fakeAttestor) *attestedHeaderBuilder {
	t.Helper()
	if len(attestors) == 0 {
		t.Fatal("at least one test attestor is required")
	}
	keys := make([][]byte, len(attestors))
	for i, at := range attestors {
		signer, err := attestation.NewSigner(8453, fmt.Sprintf("%064x", i+1))
		if err != nil {
			t.Fatal(err)
		}
		at.verifySigner = signer
		at.verifyValid = true
		keys[i] = signer.PublicKey()
	}
	// The public keys must be supplied in the ClientState's strict lexical order.
	// One deterministic signer is enough for header-builder tests; multi-attestor
	// selection is covered with explicit ordered endpoints below.
	if len(keys) != 1 {
		t.Fatal("use testSigningBuilderOne for one attestor")
	}
	set := attestation.AttestorConfig{PublicKeys: keys, Threshold: 1}
	setHash, err := set.SetHash()
	if err != nil {
		t.Fatal(err)
	}
	return &attestedHeaderBuilder{
		chainID: 8453, router: ethcommon.HexToAddress("0x1111111111111111111111111111111111111111"), set: set, setHash: setHash,
		attestors: []SigningAttestor{{Index: 0, Client: attestors[0]}}, srcChain: "op-sepolia", runMode: attestorpb.RunMode_RUN_MODE_SAFE, name: "l2-opstack",
	}
}

func TestBindToAttestation(t *testing.T) {
	// A refusal must abort the build rather than package an unsigned or unrecognised
	// block. The wasm client only accepts a certificate, so old-attestor version skew
	// cannot safely degrade to the pre-signature path.
	t.Run("rejects a block the attestor does not recognise", func(t *testing.T) {
		at := &fakeAttestor{verifyValid: false}
		b := testSigningBuilder(t, at)
		at.verifyValid = false

		_, err := b.bindToAttestation(context.Background(), 4096, testL2Header(t))
		if err == nil || !strings.Contains(err.Error(), "only 0 valid attestor signatures") {
			t.Fatalf("bindToAttestation error = %v, want insufficient signatures", err)
		}
	})

	t.Run("sends the exact context and returns signed index", func(t *testing.T) {
		at := &fakeAttestor{}
		b := testSigningBuilder(t, at)
		header := testL2Header(t)

		signatures, err := b.bindToAttestation(context.Background(), 4096, header)
		if err != nil {
			t.Fatalf("bindToAttestation: %v", err)
		}
		if len(signatures) != 1 || signatures[0].AttestorIndex != 0 || len(signatures[0].Signature) != 64 {
			t.Fatalf("signatures = %+v, want one 64-byte index-0 signature", signatures)
		}
		if at.gotVerifyHeight != 4096 || at.gotVerifySrcChain != "op-sepolia" || at.gotVerifyMode != attestorpb.RunMode_RUN_MODE_SAFE {
			t.Fatalf("verification request mismatch: %+v", at)
		}
		if got, want := string(at.gotVerifyRoot), string(header.Root.Bytes()); got != want {
			t.Fatalf("state root = %x, want %x", got, want)
		}
		if got, want := string(at.gotVerifyHash), string(header.Hash().Bytes()); got != want {
			t.Fatalf("block hash = %x, want %x", got, want)
		}
		if at.gotVerifyRouter != [20]byte(b.router) || at.gotVerifySetHash != b.setHash {
			t.Fatalf("attestation context not forwarded: router=%x set_hash=%x", at.gotVerifyRouter, at.gotVerifySetHash)
		}
	})

	t.Run("requires threshold and accepts another configured signer", func(t *testing.T) {
		type pair struct {
			signer   attestation.Signer
			attestor *fakeAttestor
		}
		pairs := make([]pair, 3)
		for i := range pairs {
			signer, err := attestation.NewSigner(8453, fmt.Sprintf("%064x", i+10))
			if err != nil {
				t.Fatal(err)
			}
			pairs[i] = pair{signer: signer, attestor: &fakeAttestor{verifyValid: true, verifySigner: signer}}
		}
		sort.Slice(pairs, func(i, j int) bool { return string(pairs[i].signer.PublicKey()) < string(pairs[j].signer.PublicKey()) })
		set := attestation.AttestorConfig{Threshold: 2}
		endpoints := make([]SigningAttestor, len(pairs))
		for i, pair := range pairs {
			set.PublicKeys = append(set.PublicKeys, pair.signer.PublicKey())
			endpoints[i] = SigningAttestor{Index: uint16(i), Client: pair.attestor}
		}
		pairs[0].attestor.verifyValid = false // quorum must use indices 1 and 2.
		built, err := NewAttestedHeaderBuilder(&ethclient.Client{}, ethcommon.HexToAddress("0x1111111111111111111111111111111111111111"), 8453, set, endpoints, "op", attestorpb.RunMode_RUN_MODE_SAFE, "l2-op")
		if err != nil {
			t.Fatal(err)
		}
		signatures, err := built.(*attestedHeaderBuilder).bindToAttestation(context.Background(), 4096, testL2Header(t))
		if err != nil {
			t.Fatalf("threshold quorum: %v", err)
		}
		if len(signatures) != 2 || signatures[0].AttestorIndex != 1 || signatures[1].AttestorIndex != 2 {
			t.Fatalf("signatures = %+v, want ordered indices 1,2", signatures)
		}
	})

	t.Run("hanging endpoint does not block a healthy quorum", func(t *testing.T) {
		type pair struct {
			signer   attestation.Signer
			attestor *fakeAttestor
		}
		pairs := make([]pair, 3)
		for i := range pairs {
			signer, err := attestation.NewSigner(8453, fmt.Sprintf("%064x", i+20))
			if err != nil {
				t.Fatal(err)
			}
			pairs[i] = pair{signer: signer, attestor: &fakeAttestor{verifyValid: true, verifySigner: signer}}
		}
		sort.Slice(pairs, func(i, j int) bool { return string(pairs[i].signer.PublicKey()) < string(pairs[j].signer.PublicKey()) })
		pairs[0].attestor.verifyBlock = make(chan struct{})
		set := attestation.AttestorConfig{Threshold: 2}
		endpoints := make([]SigningAttestor, len(pairs))
		for i, pair := range pairs {
			set.PublicKeys = append(set.PublicKeys, pair.signer.PublicKey())
			endpoints[i] = SigningAttestor{Index: uint16(i), Client: pair.attestor}
		}
		built, err := NewAttestedHeaderBuilder(&ethclient.Client{}, ethcommon.HexToAddress("0x1111111111111111111111111111111111111111"), 8453, set, endpoints, "op", attestorpb.RunMode_RUN_MODE_SAFE, "l2-op")
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		signatures, err := built.(*attestedHeaderBuilder).bindToAttestation(ctx, 4096, testL2Header(t))
		if err != nil {
			t.Fatalf("healthy quorum blocked by one hanging endpoint: %v", err)
		}
		if len(signatures) != 2 || signatures[0].AttestorIndex != 1 || signatures[1].AttestorIndex != 2 {
			t.Fatalf("signatures = %+v, want ordered healthy quorum indices 1,2", signatures)
		}
	})

	t.Run("propagates transport failure as insufficient quorum", func(t *testing.T) {
		at := &fakeAttestor{verifyErr: errors.New("connection refused")}
		b := testSigningBuilder(t, at)
		_, err := b.bindToAttestation(context.Background(), 4096, testL2Header(t))
		if err == nil || !strings.Contains(err.Error(), "connection refused") {
			t.Fatalf("error = %v, want transport cause", err)
		}
		if !chain.IsRetryable(err) || chain.IsPermanent(err) {
			t.Fatalf("error = %v, want transient quorum failure", err)
		}
	})

	t.Run("classifies impossible permanent quorum", func(t *testing.T) {
		at := &fakeAttestor{verifyErr: fmt.Errorf("%w: invalid run mode", ErrAttestorBadRequest)}
		b := testSigningBuilder(t, at)
		_, err := b.bindToAttestation(context.Background(), 4096, testL2Header(t))
		if err == nil || !errors.Is(err, ErrAttestorBadRequest) || !chain.IsPermanent(err) {
			t.Fatalf("error = %v, want permanent bad-request quorum failure", err)
		}
	})

	t.Run("classifies invalid signature permanent", func(t *testing.T) {
		at := &fakeAttestor{}
		b := testSigningBuilder(t, at)
		wrongSigner, err := attestation.NewSigner(8453, "ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff")
		if err != nil {
			t.Fatal(err)
		}
		at.verifySigner = wrongSigner
		_, bindErr := b.bindToAttestation(context.Background(), 4096, testL2Header(t))
		if bindErr == nil || !errors.Is(bindErr, ErrAttestorSignature) || !chain.IsPermanent(bindErr) {
			t.Fatalf("error = %v, want permanent invalid-signature quorum failure", bindErr)
		}
	})
}

func TestNewAttestedHeaderBuilder_RejectsAnUnsafeConfiguration(t *testing.T) {
	signer, err := attestation.NewSigner(8453, "0102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f20")
	if err != nil {
		t.Fatal(err)
	}
	set := attestation.AttestorConfig{PublicKeys: [][]byte{signer.PublicKey()}, Threshold: 1}
	if _, err := NewAttestedHeaderBuilder(nil, ethcommon.Address{}, 8453, set, []SigningAttestor{{Index: 0, Client: &fakeAttestor{}}}, "op", attestorpb.RunMode_RUN_MODE_SAFE, "l2-op"); err == nil {
		t.Fatal("nil L2 client accepted")
	}
	if _, err := NewAttestedHeaderBuilder(&ethclient.Client{}, ethcommon.Address{}, 8453, set, nil, "op", attestorpb.RunMode_RUN_MODE_SAFE, "l2-op"); err == nil {
		t.Fatal("empty endpoint set accepted")
	}
}

// The run mode handed to each attestor must track the configured head kind, or the
// certificate is judged at a different finality than the source height gate.
func TestHeadKind_RunMode(t *testing.T) {
	for _, tc := range []struct {
		kind HeadKind
		want attestorpb.RunMode
	}{
		{Unsafe, attestorpb.RunMode_RUN_MODE_UNSAFE}, {Safe, attestorpb.RunMode_RUN_MODE_SAFE}, {Finalized, attestorpb.RunMode_RUN_MODE_FINALIZED},
	} {
		if got := tc.kind.RunMode(); got != tc.want {
			t.Errorf("HeadKind(%s).RunMode() = %v, want %v", tc.kind, got, tc.want)
		}
	}
}
