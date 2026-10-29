package services

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// tendermintClientExpiry is the mirror of ethClientExpiry in chain/cosmos, and
// the two are shaped differently on purpose: a Tendermint client carries one
// explicit trusting period, while a beacon client carries none and its expiry has
// to be inferred from sync-committee geometry.
//
// What they share is the clock. Both measure from the state the client currently
// TRUSTS, never from now.
func TestTendermintClientExpiry(t *testing.T) {
	trustedAt := time.Unix(1_700_000_000, 0).UTC()

	t.Run("trusting period after the trusted header time", func(t *testing.T) {
		got := tendermintClientExpiry(trustedAt, 1209600) // 14 days
		want := trustedAt.Add(14 * 24 * time.Hour)
		if !got.Equal(want) {
			t.Fatalf("expiry = %s, want %s", got, want)
		}
	})

	t.Run("measured from the trusted header, not from now", func(t *testing.T) {
		// A value derived from wall-clock time would report every client as
		// healthy forever, because it moves with the check. This is the property
		// the anti-expiry refresh depends on.
		const period = uint32(3600)
		older := tendermintClientExpiry(trustedAt, period)
		newer := tendermintClientExpiry(trustedAt.Add(time.Hour), period)
		if gap := newer.Sub(older); gap != time.Hour {
			t.Fatalf("an hour of header progress moved the expiry by %s, want 1h", gap)
		}
	})

	t.Run("the period is seconds", func(t *testing.T) {
		// The field is uint32 seconds on the wire. Reading it as any other unit
		// silently mis-sizes the refresh window -- as nanoseconds a 14-day period
		// becomes 1.2ms and the client is always "about to expire".
		if got := tendermintClientExpiry(trustedAt, 60); !got.Equal(trustedAt.Add(time.Minute)) {
			t.Fatalf("60 gave %s, want one minute after %s", got, trustedAt)
		}
	})

	t.Run("a zero trusting period expires at the trusted header itself", func(t *testing.T) {
		// Not treated as "no expiry": zero here means the client state is
		// unusable, and reporting it as already expired sends the refresh routine
		// at it immediately rather than letting it sit unnoticed.
		if got := tendermintClientExpiry(trustedAt, 0); !got.Equal(trustedAt) {
			t.Fatalf("expiry = %s, want the trusted header time %s", got, trustedAt)
		}
	})
}

// --- the ICS-24 addressing the ETH side proves against ---
//
// EthPath and EthIBCStorageKey decide WHICH storage slot a membership proof is
// taken at. Get them wrong and the proof is well-formed but about the wrong
// location: it either fails verification, or -- worse for a non-membership proof
// -- succeeds in showing that some unrelated slot is empty.
//
// Both are a cross-language contract with contracts/core/libraries/ICS24Host.sol, so the
// expectations here are transcribed from that file rather than from the Go code
// they check. A test that derived them the same way the code does would agree
// with any mistake.

func (h *timeoutBatchHandler) SendCosmosTxBatch(_ context.Context, _ CosmosEndpoint, msgs []any) error {
	h.singletons = append(h.singletons, msgs)
	if len(msgs) != 1 {
		return errors.New("test only accepts singleton probes")
	}
	if msgs[0] == "poison" {
		return ErrPermanentRelayFailure
	}
	return nil
}

func (h *transientTimeoutBatchHandler) SendCosmosTxBatch(_ context.Context, _ CosmosEndpoint, _ []any) error {
	return errors.New("cosmos rpc unavailable")
}

func TestRecoveryState(t *testing.T) {
	t.Run("missing file and partial checkpoints", func(t *testing.T) {
		resetLoadedRecoveryStoresForTest()
		path := filepath.Join(t.TempDir(), "state", "cursors.json")
		store, err := LoadRecoveryState(path)
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := store.Get(); ok {
			t.Fatal("missing file unexpectedly contained a source")
		}
		if err := store.SaveCheckpoint(RecoveryCursors{"cosmos": 20, "eth_send": 30}); err != nil {
			t.Fatal(err)
		}
		if err := store.SaveCheckpoint(RecoveryCursors{"cosmos": 10, "eth_write_ack": 40}); err != nil {
			t.Fatal(err)
		}
		got, ok := store.Get()
		if !ok || !got.equal(RecoveryCursors{"cosmos": 10, "eth_send": 30, "eth_write_ack": 40}) {
			t.Fatalf("merged cursors = %+v, ok=%t", got, ok)
		}
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != stateFilePerm {
			t.Fatalf("state mode = %o, want %o", info.Mode().Perm(), stateFilePerm)
		}

		resetLoadedRecoveryStoresForTest()
		reloaded, err := LoadRecoveryState(path)
		if err != nil {
			t.Fatal(err)
		}
		got, ok = reloaded.Get()
		if !ok || got["cosmos"] != 10 || got["eth_send"] != 30 || got["eth_write_ack"] != 40 {
			t.Fatalf("reloaded cursors = %+v, ok=%t", got, ok)
		}
	})

	t.Run("checkpoint can move backward", func(t *testing.T) {
		resetLoadedRecoveryStoresForTest()
		path := filepath.Join(t.TempDir(), "state.json")
		store, err := LoadRecoveryState(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := store.SaveCheckpoint(RecoveryCursors{"cosmos": 100, "eth_send": 200}); err != nil {
			t.Fatal(err)
		}
		if err := store.SaveCheckpoint(RecoveryCursors{"cosmos": 50}); err != nil {
			t.Fatal(err)
		}

		got, ok := store.Get()
		want := RecoveryCursors{"cosmos": 50, "eth_send": 200}
		if !ok || !got.equal(want) {
			t.Fatalf("checkpoint cursors = %+v, ok=%t, want %+v", got, ok, want)
		}

		resetLoadedRecoveryStoresForTest()
		reloaded, err := LoadRecoveryState(path)
		if err != nil {
			t.Fatal(err)
		}
		got, ok = reloaded.Get()
		if !ok || !got.equal(want) {
			t.Fatalf("reloaded checkpoint cursors = %+v, ok=%t, want %+v", got, ok, want)
		}
	})

	t.Run("failed write does not advance memory", func(t *testing.T) {
		resetLoadedRecoveryStoresForTest()
		dir := t.TempDir()
		path := filepath.Join(dir, "state.json")
		store, err := LoadRecoveryState(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := store.SaveCheckpoint(RecoveryCursors{"cosmos": 10}); err != nil {
			t.Fatal(err)
		}

		blockedPath := filepath.Join(dir, "blocked")
		if err := os.Mkdir(blockedPath, 0o700); err != nil {
			t.Fatal(err)
		}
		store.path = blockedPath
		if err := store.SaveCheckpoint(RecoveryCursors{"cosmos": 20}); err == nil {
			t.Fatal("Save succeeded when the target path was a directory")
		}
		got, ok := store.Get()
		if !ok || got["cosmos"] != 10 {
			t.Fatalf("cursor after failed write = %+v, ok=%t, want height 10", got, ok)
		}

		store.path = path
		if err := store.SaveCheckpoint(RecoveryCursors{"cosmos": 20}); err != nil {
			t.Fatal(err)
		}
		resetLoadedRecoveryStoresForTest()
		reloaded, err := LoadRecoveryState(path)
		if err != nil {
			t.Fatal(err)
		}
		got, ok = reloaded.Get()
		if !ok || got["cosmos"] != 20 {
			t.Fatalf("reloaded cursor after retry = %+v, ok=%t, want height 20", got, ok)
		}
	})

	t.Run("rejects corrupt and unsupported files", func(t *testing.T) {
		for _, tc := range []struct {
			name string
			data []byte
		}{
			{name: "corrupt", data: []byte("{")},
			{name: "wrong version", data: []byte(`{"version":1,"sources":{}}`)},
		} {
			t.Run(tc.name, func(t *testing.T) {
				resetLoadedRecoveryStoresForTest()
				path := filepath.Join(t.TempDir(), "state.json")
				if err := os.WriteFile(path, tc.data, 0o600); err != nil {
					t.Fatal(err)
				}
				if _, err := LoadRecoveryState(path); err == nil {
					t.Fatal("LoadRecoveryState succeeded for invalid state")
				}
			})
		}
	})

	// One process now keeps one record, but two recovery streams write to it: the
	// Cosmos subscriber owns CosmosHeight, the ETH subscriber owns EthSendBlock and
	// EthWriteAckBlk. They run concurrently and each save is a read-modify-write of
	// the whole file, so a lost update here means a stream silently resumes from an
	// older cursor and rescans -- or worse, the other stream's newer cursor is
	// rolled back and its window is skipped.
	//
	// Memoization by path is what makes the two share one view; without it each
	// would hold its own copy and the last writer would erase the other's field.
	t.Run("memoization shares one view between streams", func(t *testing.T) {
		resetLoadedRecoveryStoresForTest()
		path := filepath.Join(t.TempDir(), "state.json")
		cosmosView, err := LoadRecoveryState(path)
		if err != nil {
			t.Fatal(err)
		}
		ethView, err := LoadRecoveryState(path)
		if err != nil {
			t.Fatal(err)
		}
		if cosmosView != ethView {
			t.Fatal("the same path returned two stores; the two streams would each keep their own cursors")
		}

		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			if err := cosmosView.SaveCheckpoint(RecoveryCursors{"cosmos": 11}); err != nil {
				t.Errorf("cosmos save: %v", err)
			}
		}()
		go func() {
			defer wg.Done()
			if err := ethView.SaveCheckpoint(RecoveryCursors{"eth_send": 22, "eth_write_ack": 33}); err != nil {
				t.Errorf("eth save: %v", err)
			}
		}()
		wg.Wait()

		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var file recoveryStateFile
		if err := json.Unmarshal(data, &file); err != nil {
			t.Fatal(err)
		}
		want := RecoveryCursors{"cosmos": 11, "eth_send": 22, "eth_write_ack": 33}
		if !file.Cursors.equal(want) {
			t.Fatalf("persisted cursors = %+v, want %+v; one stream's write erased the other's", file.Cursors, want)
		}
	})

	t.Run("write leaves no temp files", func(t *testing.T) {
		resetLoadedRecoveryStoresForTest()
		dir := t.TempDir()
		store, err := LoadRecoveryState(filepath.Join(dir, "state.json"))
		if err != nil {
			t.Fatal(err)
		}
		for i := range 5 {
			if err := store.SaveCheckpoint(RecoveryCursors{"cosmos": uint64(i + 1)}); err != nil {
				t.Fatalf("save %d: %v", i, err)
			}
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		for _, entry := range entries {
			if strings.HasSuffix(entry.Name(), ".tmp") {
				t.Fatalf("temporary recovery file %q was not removed", entry.Name())
			}
		}
	})
}
