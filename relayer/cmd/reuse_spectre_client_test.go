package main

import (
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/common"

	updateClientContract "relayer/bindings/UpdateClient"
	relayerclient "relayer/client"
)

func reusableState(chainID string, height uint64) relayerclient.ClientState {
	return relayerclient.ClientState{
		ChainId:      chainID,
		LatestHeight: updateClientContract.IICS02ClientMsgsHeight{RevisionHeight: height},
	}
}

// TestSpectreClientIsReusable covers the checks that decide whether a
// spectre_client address already in the config can be kept. The old code asked
// only whether *any* bytecode lived there, which is not the same question:
// contract addresses derive from deployer+nonce, so the same key redeploying on
// a second chain lands the identical contract at the identical address, and a
// re-created Cosmos devnet keeps its chain id while resetting its height.
func TestSpectreClientIsReusable(t *testing.T) {
	t.Parallel()

	const chain = "test-ibc-eth"

	t.Run("matching chain and a height the chain has reached", func(t *testing.T) {
		t.Parallel()
		if err := spectreClientIsReusable(reusableState(chain, 1200), chain, 1634); err != nil {
			t.Fatalf("healthy client rejected: %v", err)
		}
	})

	t.Run("height exactly at the live head is fine", func(t *testing.T) {
		t.Parallel()
		if err := spectreClientIsReusable(reusableState(chain, 1634), chain, 1634); err != nil {
			t.Fatalf("client at the live head rejected: %v", err)
		}
	})

	// The case that cost a bring-up: a client tracking a Cosmos chain that was
	// wiped and re-created. Same chain id, so only the height gives it away.
	t.Run("height above the live head means the chain was re-created", func(t *testing.T) {
		t.Parallel()
		err := spectreClientIsReusable(reusableState(chain, 1634), chain, 12)
		if err == nil {
			t.Fatal("accepted a client trusting a height the chain has never reached")
		}
		for _, want := range []string{"1634", "12", "re-created"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error %q does not mention %q", err, want)
			}
		}
	})

	// The cross-chain collision: same deployer, same nonce, different chain.
	t.Run("different chain id", func(t *testing.T) {
		t.Parallel()
		err := spectreClientIsReusable(reusableState("othernet-1", 10), chain, 1634)
		if err == nil {
			t.Fatal("accepted a client tracking a different Cosmos chain")
		}
		if !strings.Contains(err.Error(), "othernet-1") || !strings.Contains(err.Error(), chain) {
			t.Errorf("error %q should name both chain ids", err)
		}
	})

	t.Run("frozen client", func(t *testing.T) {
		t.Parallel()
		state := reusableState(chain, 100)
		state.IsFrozen = true
		err := spectreClientIsReusable(state, chain, 1634)
		if err == nil {
			t.Fatal("accepted a frozen client")
		}
		if !strings.Contains(err.Error(), "frozen") {
			t.Errorf("error %q should say the client is frozen", err)
		}
	})

	// A contract that answers getClientState() with an empty chain id is not a
	// SpectreClient — most likely an unrelated contract at a colliding address.
	t.Run("empty chain id", func(t *testing.T) {
		t.Parallel()
		if err := spectreClientIsReusable(reusableState("", 0), chain, 1634); err == nil {
			t.Fatal("accepted a client state with no chain id")
		}
	})
}

func TestConsensusStateHashIsReusable(t *testing.T) {
	t.Parallel()

	const (
		chain  = "test-ibc-eth"
		height = uint64(1634)
	)

	t.Run("matching stored and live state", func(t *testing.T) {
		t.Parallel()
		hash := [32]byte{0x11}
		if err := consensusStateHashIsReusable(hash, hash, chain, height); err != nil {
			t.Fatalf("healthy client rejected: %v", err)
		}
	})

	// A replacement chain can grow past the client's old trusted height, making
	// the height-only reset guard pass. Its consensus state at that height is
	// still different, and must decline reuse before all later updates revert.
	t.Run("re-created chain grown past trusted height", func(t *testing.T) {
		t.Parallel()
		err := consensusStateHashIsReusable([32]byte{0x11}, [32]byte{0x22}, chain, height)
		if err == nil {
			t.Fatal("accepted a different consensus state at the trusted height")
		}
		for _, want := range []string{"1634", chain, "re-created"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error %q does not mention %q", err, want)
			}
		}
	})
}

// TestRouterWiringIsReusable covers the second half of "can this deploy be
// skipped": whether the ICS26Router in this config is actually wired to the
// SpectreClient we are about to reuse, AND to the Cosmos client this config
// relays.
//
// The address half shipped without this test and was incomplete: it accepted a
// router whose counterparty still pointed at a wasm client from an earlier
// create-clients-cosmos run. That state passes every other check — right chain,
// not frozen, right height, right address — and relays nothing.
func TestRouterWiringIsReusable(t *testing.T) {
	t.Parallel()

	addr := common.HexToAddress("0x00000000000000000000000000000000000000AA")
	other := common.HexToAddress("0x00000000000000000000000000000000000000BB")
	const wasm = "08-wasm-7"

	t.Run("address and counterparty both match", func(t *testing.T) {
		t.Parallel()
		if err := routerWiringIsReusable(routerWiring{client: addr, counterparty: wasm}, addr, wasm); err != nil {
			t.Fatalf("expected reuse, got %v", err)
		}
	})

	t.Run("address points elsewhere", func(t *testing.T) {
		t.Parallel()
		err := routerWiringIsReusable(routerWiring{client: other, counterparty: wasm}, addr, wasm)
		if err == nil {
			t.Fatal("expected a router registered to a different address to decline reuse")
		}
		if !strings.Contains(err.Error(), other.Hex()) {
			t.Fatalf("reason should name the address actually registered: %v", err)
		}
	})

	// The regression the reviewers found: everything matches except the
	// counterparty, because create-clients-cosmos was re-run and minted a new
	// wasm client id.
	t.Run("stale counterparty from a re-run create-clients-cosmos", func(t *testing.T) {
		t.Parallel()
		err := routerWiringIsReusable(routerWiring{client: addr, counterparty: "08-wasm-3"}, addr, wasm)
		if err == nil {
			t.Fatal("expected a stale counterparty to decline reuse")
		}
		if !strings.Contains(err.Error(), "08-wasm-3") || !strings.Contains(err.Error(), wasm) {
			t.Fatalf("reason should name both the registered and the expected client id: %v", err)
		}
	})

	t.Run("counterparty never registered", func(t *testing.T) {
		t.Parallel()
		if err := routerWiringIsReusable(routerWiring{client: addr, counterparty: ""}, addr, wasm); err == nil {
			t.Fatal("expected an empty counterparty to decline reuse")
		}
	})
}
