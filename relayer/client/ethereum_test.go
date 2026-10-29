package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	ethcommon "github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/ethclient"
)

// ethProofServer answers eth_getProof with one storage proof whose value field
// is valueJSON, inserted raw so a caller can send a JSON null -- which is what a
// node may report for a slot it has never written, alongside "0x0".
func ethProofServer(t *testing.T, valueJSON string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID json.RawMessage `json:"id"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":{
			"accountProof":["0xf8"],
			"storageHash":"0x0000000000000000000000000000000000000000000000000000000000000042",
			"storageProof":[{"key":"0x0000000000000000000000000000000000000000000000000000000000000001","value":%s,"proof":["0xf8"]}]
		}}`, req.ID, valueJSON)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// The two proof helpers are halves of one guard, and only one half existed.
// GetEthNonMembershipProof already refuses a slot that is NOT empty — evidence
// the packet was received. GetEthMembershipProof accepted a slot that IS empty
// and marshalled a "proof" of zero, which the destination's light client rejects.
// That costs a transaction, and the revert reads as deterministic, so a packet
// whose only problem was a proof height the destination had not reached yet was
// dropped after paying to discover it.
func TestGetEthMembershipProof(t *testing.T) {
	t.Run("an empty slot is an absent commitment, not a proof of zero", func(t *testing.T) {
		for _, tc := range []struct{ name, valueJSON string }{
			{name: "zero", valueJSON: `"0x0"`},
			{name: "null", valueJSON: `null`},
		} {
			t.Run(tc.name, func(t *testing.T) {
				srv := ethProofServer(t, tc.valueJSON)
				c, err := DialEthRPC(context.Background(), srv.URL, time.Minute)
				if err != nil {
					t.Fatalf("DialEthRPC: %v", err)
				}
				t.Cleanup(c.Close)

				_, err = GetEthMembershipProof(
					context.Background(), c, ethcommon.Address{}, []byte("path"), ethcommon.Hash{}, big.NewInt(7))
				if err == nil {
					t.Fatal("an empty slot produced a proof; the chain would reject it and the packet would be dropped")
				}
				if !errors.Is(err, ErrCommitmentAbsent) {
					t.Fatalf("error = %v, want it to name the commitment as absent", err)
				}
				// Absent is not dead: the height moves, and permanent here would
				// drop a packet that is merely early.
				if errors.Is(err, ErrPacketAlreadyReceived) {
					t.Fatal("an absent commitment was reported as a delivered packet")
				}
			})
		}
	})

	t.Run("a populated slot produces a proof", func(t *testing.T) {
		srv := ethProofServer(t, `"0x2a"`)
		c, err := DialEthRPC(context.Background(), srv.URL, time.Minute)
		if err != nil {
			t.Fatalf("DialEthRPC: %v", err)
		}
		t.Cleanup(c.Close)

		raw, err := GetEthMembershipProof(
			context.Background(), c, ethcommon.Address{}, []byte("path"), ethcommon.Hash{}, big.NewInt(7))
		if err != nil {
			t.Fatalf("a non-empty slot must still prove: %v", err)
		}
		if !strings.Contains(string(raw), "0x2a") {
			t.Fatalf("proof does not carry the slot value: %s", raw)
		}
	})

	// The proof helpers are called from goroutines that drive one relay direction
	// each, so an eth_getProof that never comes back used to wedge that direction
	// with no error and nothing in the log. These tests pin that the caller's
	// context can now interrupt the call.
	//
	// Both run against a server that accepts the request and never answers — the
	// shape of the incident, rather than a connection failure, which would prove
	// nothing about context handling.
	t.Run("honours context", func(t *testing.T) {
		srv := blackHoleServer(t)
		c, err := DialEthRPC(context.Background(), srv.URL, time.Hour)
		if err != nil {
			t.Fatalf("DialEthRPC: %v", err)
		}
		t.Cleanup(c.Close)

		ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
		defer cancel()

		done := make(chan error, 1)
		go func() {
			_, err := GetEthMembershipProof(ctx, c, ethcommon.Address{}, []byte("path"), ethcommon.Hash{}, big.NewInt(1))
			done <- err
		}()

		select {
		case err := <-done:
			if err == nil {
				t.Fatal("expected an error from the cancelled call")
			}
		case <-time.After(10 * time.Second):
			t.Fatal("GetEthMembershipProof ignored its context: the relay direction would be wedged")
		}
	})

	t.Run("RPCs honor cancellation", func(t *testing.T) {
		for _, tc := range []struct {
			name string
			call func(context.Context, *ethclient.Client) error
		}{
			{"membership", func(ctx context.Context, client *ethclient.Client) error {
				_, err := GetEthMembershipProof(ctx, client, common.Address{}, []byte("path"), common.Hash{}, big.NewInt(1))
				return err
			}},
			{"non-membership", func(ctx context.Context, client *ethclient.Client) error {
				_, err := GetEthNonMembershipProof(ctx, client, common.Address{}, []byte("path"), common.Hash{}, big.NewInt(1))
				return err
			}},
		} {
			t.Run(tc.name, func(t *testing.T) {
				ctx, cancel := context.WithCancel(context.Background())
				done := make(chan error, 1)
				client := silentEthRPC(t)
				go func() { done <- tc.call(ctx, client) }()
				time.Sleep(30 * time.Millisecond)
				cancel()
				select {
				case err := <-done:
					if !errors.Is(err, context.Canceled) {
						t.Fatalf("error = %v, want context.Canceled", err)
					}
				case <-time.After(3 * time.Second):
					t.Fatal("proof RPC ignored cancellation")
				}
			})
		}
	})
}

func TestGetEthNonMembershipProof_HonoursContext(t *testing.T) {
	srv := blackHoleServer(t)
	c, err := DialEthRPC(context.Background(), srv.URL, time.Hour)
	if err != nil {
		t.Fatalf("DialEthRPC: %v", err)
	}
	t.Cleanup(c.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	done := make(chan error, 1)
	go func() {
		_, err := GetEthNonMembershipProof(ctx, c, ethcommon.Address{}, []byte("path"), ethcommon.Hash{}, big.NewInt(1))
		done <- err
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected an error from the cancelled call")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("GetEthNonMembershipProof ignored its context: the timeout scanner would stop for good")
	}
}

func silentEthRPC(t *testing.T) *ethclient.Client {
	t.Helper()
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { <-release }))
	t.Cleanup(server.Close)
	t.Cleanup(func() { close(release) })
	client, err := ethclient.Dial(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(client.Close)
	return client
}
