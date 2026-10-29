package l2rollup_test

// TestLiveAttestorQuorumE2E drives the relayer's real L2->Cosmos signing path —
// NewQuorumAttestationFrontier, NewAttestedHeaderBuilder, and the attestorgrpc
// client — against distinct attestor processes with distinct Ed25519 keys.
// scripts/local/test_relayer_attestor_e2e.sh starts those daemons (three set
// members, an imposter, and a member at the wrong head) beside a running
// devnet. Aggregation and classification matrices stay in the unit tests;
// this suite only asserts what the real processes make the relayer do.

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"fmt"
	"math/big"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"attestor/types/attestation"
	"relayer/chain"
	"relayer/chain/l2rollup"
	"relayer/chain/l2rollup/attestorgrpc"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/ethclient"
)

const quorumEnvPrefix = "RELAYER_QUORUM_E2E_"

type quorumEnv struct {
	srcChain    string
	l2RPC       string
	l2ChainID   uint64
	head        l2rollup.HeadKind
	router      common.Address
	set         attestation.AttestorConfig
	members     []string // set member i serves index i
	imposter    string   // a live attestor whose key is not in the set
	wrongHead   string   // set member 2's key, gating on another head
	unreachable string   // a closed port
	timeout     time.Duration
}

func TestLiveAttestorQuorumE2E(t *testing.T) {
	env, enabled, err := loadQuorumEnv()
	if !enabled {
		t.Skipf("set %sSRC_CHAIN (via scripts/local/test_relayer_attestor_e2e.sh) to run against live attestors", quorumEnvPrefix)
	}
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), env.timeout)
	defer cancel()

	l2, err := ethclient.DialContext(ctx, env.l2RPC)
	if err != nil {
		t.Fatal(err)
	}
	defer l2.Close()

	clients := map[string]*attestorgrpc.Client{}
	dial := func(address string) *attestorgrpc.Client {
		if client, ok := clients[address]; ok {
			return client
		}
		client, dialErr := attestorgrpc.Dial(address)
		if dialErr != nil {
			t.Fatalf("dial attestor %s: %v", address, dialErr)
		}
		clients[address] = client
		return client
	}
	defer func() {
		for _, client := range clients {
			_ = client.Close()
		}
	}()
	// Endpoints are assigned set indices by position. One process may back two
	// indices (two "unreachable" or two "imposter" slots): the builder keys on
	// the index, and a reused process answers identically for both.
	endpoints := func(addresses ...string) []l2rollup.SigningAttestor {
		out := make([]l2rollup.SigningAttestor, len(addresses))
		for index, address := range addresses {
			out[index] = l2rollup.SigningAttestor{Index: uint16(index), Client: dial(address)}
		}
		return out
	}

	// Q0: every live process serves the route; the wrong-head one is the only
	// disagreement, which is exactly what relayer startup refuses.
	t.Run("Q0_endpoint_identity", func(t *testing.T) {
		for _, address := range append(append([]string(nil), env.members...), env.imposter, env.wrongHead) {
			chains, infoErr := dial(address).Info(ctx)
			if infoErr != nil {
				t.Fatalf("Info(%s): %v", address, infoErr)
			}
			head := ""
			for _, chainInfo := range chains {
				if chainInfo.GetSrcChain() == env.srcChain {
					head = chainInfo.GetAttestationHead()
				}
			}
			want := env.head.String()
			if address == env.wrongHead {
				if head == "" || head == want {
					t.Fatalf("wrong-head attestor %s gates on %q; want a head other than %q", address, head, want)
				}
				continue
			}
			if head != want {
				t.Fatalf("attestor %s serves %q at head %q, want %q", address, env.srcChain, head, want)
			}
		}
		evidence(t, "members %v + imposter %s at head %s; wrong-head attestor %s", env.members, env.imposter, env.head, env.wrongHead)
	})

	// The height every case builds at: one the full member quorum has attested.
	frontier, err := l2rollup.NewQuorumAttestationFrontier(endpoints(env.members...), env.set.Threshold)
	if err != nil {
		t.Fatal(err)
	}
	height := waitQuorumFrontier(t, ctx, frontier, env)

	build := func(t *testing.T, addresses ...string) (*l2rollup.SignedAttestedL2Header, error) {
		t.Helper()
		builder, buildErr := l2rollup.NewAttestedHeaderBuilder(l2, env.router, env.l2ChainID, env.set,
			endpoints(addresses...), env.srcChain, env.head.RunMode(), "quorum-e2e")
		if buildErr != nil {
			t.Fatalf("NewAttestedHeaderBuilder: %v", buildErr)
		}
		message, committed, buildErr := builder.BuildHeader(ctx, l2rollup.HeaderRequest{Height: height})
		if buildErr != nil {
			return nil, buildErr
		}
		if committed != height {
			t.Fatalf("builder committed height %d, want %d", committed, height)
		}
		header, ok := message.(*l2rollup.SignedAttestedL2Header)
		if !ok {
			t.Fatalf("builder returned %T, want *SignedAttestedL2Header", message)
		}
		return header, nil
	}

	t.Run("Q1_threshold_success_with_every_member_up", func(t *testing.T) {
		header, buildErr := build(t, env.members...)
		if buildErr != nil {
			t.Fatalf("BuildHeader(%d): %v", height, buildErr)
		}
		indices := assertQuorumSignatures(t, ctx, l2, env, height, header)
		evidence(t, "L2 block %d: %d-of-%d signatures from indices %v, each verified against the pinned key and statement",
			height, len(indices), len(env.set.PublicKeys), indices)
	})

	t.Run("Q2_unavailable_endpoint_is_tolerated", func(t *testing.T) {
		header, buildErr := build(t, env.members[0], env.members[1], env.unreachable)
		if buildErr != nil {
			t.Fatalf("BuildHeader with index 2 unreachable: %v", buildErr)
		}
		indices := assertQuorumSignatures(t, ctx, l2, env, height, header)
		if fmt.Sprint(indices) != "[0 1]" {
			t.Fatalf("signatures from %v, want [0 1] with index 2 unreachable", indices)
		}
		evidence(t, "index 2 at %s unreachable: quorum still met by %v", env.unreachable, indices)
	})

	t.Run("Q3_insufficient_quorum_is_transient_and_signs_nothing", func(t *testing.T) {
		header, buildErr := build(t, env.members[0], env.unreachable, env.unreachable)
		requireNoHeader(t, header, buildErr)
		outcome := retryOutcome(buildErr)
		if outcome != chain.OutcomeTransient || !errors.Is(buildErr, l2rollup.ErrAttestorUnavailable) {
			t.Fatalf("below-threshold error = %v (%s), want transient ErrAttestorUnavailable", buildErr, outcome)
		}
		_, found, frontierErr := mustFrontier(t, env, endpoints(env.members[0], env.unreachable, env.unreachable)).
			AttestedUpTo(ctx, env.srcChain, env.head != l2rollup.Finalized)
		if found {
			t.Fatal("frontier quorum reported a height with only one reachable member")
		}
		evidence(t, "1 of 3 reachable: BuildHeader refused (%s: %v); frontier found=%t err=%v", outcome, buildErr, found, frontierErr)
	})

	t.Run("Q4_invalid_signatures_never_count", func(t *testing.T) {
		// The imposter started after the members. Until its replica reaches the
		// build height it answers "behind" (transient), not with a signature.
		waitAttestedAtLeast(t, ctx, dial(env.imposter), env, height)
		// Two imposters leave one valid signer: no retry can reach 2, so the
		// relayer must give up permanently instead of waiting forever.
		header, twoImpostersErr := build(t, env.members[0], env.imposter, env.imposter)
		requireNoHeader(t, header, twoImpostersErr)
		outcome := retryOutcome(twoImpostersErr)
		if outcome != chain.OutcomePermanent || !errors.Is(twoImpostersErr, l2rollup.ErrAttestorSignature) {
			t.Fatalf("two-imposter error = %v (%s), want permanent ErrAttestorSignature", twoImpostersErr, outcome)
		}
		raw := rawVerdict(t, ctx, l2, env, dial(env.imposter))
		// One imposter among two honest members: the honest pair carries it.
		header, buildErr := build(t, env.members[0], env.imposter, env.members[2])
		if buildErr != nil {
			t.Fatalf("BuildHeader with one imposter: %v", buildErr)
		}
		indices := assertQuorumSignatures(t, ctx, l2, env, height, header)
		if fmt.Sprint(indices) != "[0 2]" {
			t.Fatalf("signatures from %v, want [0 2] around the imposter at index 1", indices)
		}
		evidence(t, "imposter answered valid=%t with a %d-byte signature that fails the pinned key; two imposters -> %s %v; one imposter -> quorum %v",
			raw.Valid, len(raw.Signature), outcome, twoImpostersErr, indices)
	})

	t.Run("Q5_wrong_head_member_cannot_complete_a_quorum", func(t *testing.T) {
		_, verifyErr := dial(env.wrongHead).VerifyStateRoot(ctx, verificationRequest(t, ctx, l2, env, height))
		if verifyErr == nil {
			t.Fatalf("wrong-head member answered a %s request; it must refuse a run mode it does not gate on", env.head)
		}
		header, buildErr := build(t, env.members[0], env.unreachable, env.wrongHead)
		requireNoHeader(t, header, buildErr)
		evidence(t, "wrong-head member refused the run mode (%v); with one other member down the quorum failed %s: %v",
			verifyErr, retryOutcome(buildErr), buildErr)
	})
}

func loadQuorumEnv() (quorumEnv, bool, error) {
	get := func(name string) string { return strings.TrimSpace(os.Getenv(quorumEnvPrefix + name)) }
	if get("SRC_CHAIN") == "" {
		return quorumEnv{}, false, nil
	}
	env := quorumEnv{
		srcChain:    get("SRC_CHAIN"),
		l2RPC:       get("L2_RPC_URL"),
		members:     strings.Split(get("MEMBERS"), ","),
		imposter:    get("IMPOSTER"),
		wrongHead:   get("WRONG_HEAD"),
		unreachable: get("UNREACHABLE"),
		timeout:     15 * time.Minute,
	}
	if env.l2RPC == "" || len(env.members) != 3 || env.imposter == "" || env.wrongHead == "" || env.unreachable == "" {
		return env, true, fmt.Errorf("%sL2_RPC_URL, MEMBERS (3 addresses), IMPOSTER, WRONG_HEAD, and UNREACHABLE are required", quorumEnvPrefix)
	}
	chainID, err := strconv.ParseUint(get("L2_CHAIN_ID"), 10, 64)
	if err != nil || chainID == 0 {
		return env, true, fmt.Errorf("%sL2_CHAIN_ID must be a positive integer", quorumEnvPrefix)
	}
	env.l2ChainID = chainID
	switch get("HEAD") {
	case "unsafe":
		env.head = l2rollup.Unsafe
	case "safe":
		env.head = l2rollup.Safe
	case "finalized":
		env.head = l2rollup.Finalized
	default:
		return env, true, fmt.Errorf("%sHEAD must be unsafe, safe, or finalized", quorumEnvPrefix)
	}
	if !common.IsHexAddress(get("ROUTER")) {
		return env, true, fmt.Errorf("%sROUTER must be an EVM address", quorumEnvPrefix)
	}
	env.router = common.HexToAddress(get("ROUTER"))
	for _, encoded := range strings.Split(get("PUBLIC_KEYS"), ",") {
		key, decodeErr := base64.StdEncoding.DecodeString(encoded)
		if decodeErr != nil || len(key) != ed25519.PublicKeySize {
			return env, true, fmt.Errorf("%sPUBLIC_KEYS must be base64 Ed25519 keys in set order", quorumEnvPrefix)
		}
		env.set.PublicKeys = append(env.set.PublicKeys, key)
	}
	env.set.Threshold = 2
	if err := env.set.Validate(); err != nil || len(env.set.PublicKeys) != 3 {
		return env, true, fmt.Errorf("attestor set must be 3 keys, threshold 2: %v", err)
	}
	return env, true, nil
}

func evidence(t *testing.T, format string, args ...any) {
	t.Helper()
	t.Logf("EVIDENCE %s: %s", t.Name(), fmt.Sprintf(format, args...))
}

func mustFrontier(t *testing.T, env quorumEnv, attestors []l2rollup.SigningAttestor) l2rollup.AttestationFrontier {
	t.Helper()
	frontier, err := l2rollup.NewQuorumAttestationFrontier(attestors, env.set.Threshold)
	if err != nil {
		t.Fatal(err)
	}
	return frontier
}

func waitQuorumFrontier(t *testing.T, ctx context.Context, frontier l2rollup.AttestationFrontier, env quorumEnv) uint64 {
	t.Helper()
	for {
		root, found, err := frontier.AttestedUpTo(ctx, env.srcChain, env.head != l2rollup.Finalized)
		if err == nil && found && root.GetL2BlockNumber() > 0 {
			return root.GetL2BlockNumber()
		}
		select {
		case <-ctx.Done():
			t.Fatalf("member quorum never reported a frontier (last error: %v)", err)
		case <-time.After(2 * time.Second):
		}
	}
}

// waitAttestedAtLeast blocks until one attestor's own frontier reaches height.
func waitAttestedAtLeast(t *testing.T, ctx context.Context, client *attestorgrpc.Client, env quorumEnv, height uint64) {
	t.Helper()
	for {
		root, found, err := client.AttestedUpTo(ctx, env.srcChain, env.head != l2rollup.Finalized)
		if err == nil && found && root.GetL2BlockNumber() >= height {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatalf("attestor never attested L2 block %d (last error: %v)", height, err)
		case <-time.After(2 * time.Second):
		}
	}
}

// assertQuorumSignatures checks exactly threshold signatures, in ascending set
// index order, each verifying the statement the wasm client reconstructs from
// the canonical L2 header.
func assertQuorumSignatures(t *testing.T, ctx context.Context, l2 *ethclient.Client, env quorumEnv, height uint64, header *l2rollup.SignedAttestedL2Header) []uint16 {
	t.Helper()
	if len(header.AttestorSignature) != int(env.set.Threshold) {
		t.Fatalf("header carries %d signatures, want threshold %d", len(header.AttestorSignature), env.set.Threshold)
	}
	setHash, err := env.set.SetHash()
	if err != nil {
		t.Fatal(err)
	}
	canonical, err := l2.HeaderByNumber(ctx, new(big.Int).SetUint64(height))
	if err != nil {
		t.Fatal(err)
	}
	statement, err := attestation.SigningBytes(env.l2ChainID, env.router.Bytes(), setHash[:], canonical.Number.Uint64(),
		canonical.Hash().Bytes(), canonical.Root.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	indices := make([]uint16, 0, len(header.AttestorSignature))
	for position, signature := range header.AttestorSignature {
		if position > 0 && signature.AttestorIndex <= indices[position-1] {
			t.Fatalf("signature indices %v are not strictly ascending", header.AttestorSignature)
		}
		if !ed25519.Verify(env.set.PublicKeys[signature.AttestorIndex], statement, signature.Signature) {
			t.Fatalf("signature for index %d does not verify", signature.AttestorIndex)
		}
		indices = append(indices, signature.AttestorIndex)
	}
	return indices
}

func verificationRequest(t *testing.T, ctx context.Context, l2 *ethclient.Client, env quorumEnv, height uint64) l2rollup.VerificationRequest {
	t.Helper()
	canonical, err := l2.HeaderByNumber(ctx, new(big.Int).SetUint64(height))
	if err != nil {
		t.Fatal(err)
	}
	setHash, err := env.set.SetHash()
	if err != nil {
		t.Fatal(err)
	}
	request := l2rollup.VerificationRequest{
		SrcChain: env.srcChain, BlockNumber: height, StateRoot: canonical.Root.Bytes(),
		BlockHash: canonical.Hash().Bytes(), RunMode: env.head.RunMode(), AttestorSetHash: setHash,
	}
	copy(request.L2Router[:], env.router.Bytes())
	return request
}

// rawVerdict records what the imposter itself answered, so the evidence shows
// the rejection happened in the relayer, not in the attestor.
func rawVerdict(t *testing.T, ctx context.Context, l2 *ethclient.Client, env quorumEnv, client *attestorgrpc.Client) l2rollup.SignedVerdict {
	t.Helper()
	root, found, err := client.AttestedUpTo(ctx, env.srcChain, env.head != l2rollup.Finalized)
	if err != nil || !found {
		t.Fatalf("imposter frontier: %v (found=%t)", err, found)
	}
	verdict, err := client.VerifyStateRoot(ctx, verificationRequest(t, ctx, l2, env, root.GetL2BlockNumber()))
	if err != nil {
		t.Fatalf("imposter VerifyStateRoot: %v", err)
	}
	return verdict
}

func requireNoHeader(t *testing.T, header *l2rollup.SignedAttestedL2Header, err error) {
	t.Helper()
	if err == nil || header != nil {
		t.Fatalf("BuildHeader returned a header (%v) and error %v; a failed quorum must sign nothing", header, err)
	}
}

func retryOutcome(err error) chain.Outcome {
	retry, _ := chain.Classify(err)
	return retry.Outcome()
}
