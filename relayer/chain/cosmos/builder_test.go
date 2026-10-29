package cosmos

import (
	"testing"
	"time"

	"relayer/services"

	tmclient "relayer/client"

	commettypes "github.com/cometbft/cometbft/types"
)

// groth16Update is the mapping between the update pipeline and the relay engine.
// The field that matters most here is IsHop: it is the only thing that tells the
// engine a churn gap needs another climb, and the engine's own tests use a mock
// builder, so nothing else would notice this dropping it.
func TestGroth16Update(t *testing.T) {
	// SignedHeader.Header is a POINTER inside CometBFT's SignedHeader, so a
	// zero-value LightBlock panics on the first field write rather than reading
	// as an empty header.
	lightBlock := func(height int64) *tmclient.LightBlock {
		lb := &tmclient.LightBlock{BlockHeight: height}
		lb.SignedHeader.Header = &commettypes.Header{Time: time.Unix(1700000000, 0)}
		return lb
	}

	t.Run("carries the hop flag to the engine", func(t *testing.T) {
		for _, isHop := range []bool{true, false} {
			got, err := groth16Update(&services.CosmosClientUpdateBuildResult{
				HasMsg: true, LightBlock: lightBlock(90), IsHop: isHop,
			}, 30)
			if err != nil {
				t.Fatalf("map result: %v", err)
			}
			if got.IsHop != isHop {
				t.Fatalf("IsHop = %v, want %v: the engine climbs on this and on nothing else", got.IsHop, isHop)
			}
			if got.Height != 90 {
				t.Fatalf("height = %d, want the built block's 90", got.Height)
			}
		}
	})

	// A client that is already current still has to report its height, so the
	// module can seed its provability guard across a restart without a tx.
	t.Run("no message still reports the trusted height", func(t *testing.T) {
		got, err := groth16Update(&services.CosmosClientUpdateBuildResult{HasMsg: false}, 30)
		if err != nil {
			t.Fatalf("map result: %v", err)
		}
		if got.Height != 30 {
			t.Fatalf("height = %d, want the on-chain trusted 30", got.Height)
		}
		if len(got.Payloads) != 0 {
			t.Fatal("a no-op update carries a payload; the module would submit a transaction for nothing")
		}
		if got.IsHop {
			t.Fatal("a client that is already current reported a hop; the engine would climb forever")
		}
	})

	t.Run("a nil result is an error, not an empty update", func(t *testing.T) {
		if _, err := groth16Update(nil, 30); err == nil {
			t.Fatal("a nil result mapped to a usable update")
		}
	})
}
