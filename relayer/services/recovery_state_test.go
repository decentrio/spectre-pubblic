package services

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestResumeCursor(t *testing.T) {
	tests := []struct {
		name                        string
		persisted, lookback, latest uint64
		start                       uint64
		resumed                     bool
		behind                      uint64
	}{
		{name: "missing", lookback: 744, latest: 1000, start: 744},
		{name: "older than lookback", persisted: 100, lookback: 744, latest: 1000, start: 100, resumed: true, behind: 900},
		{name: "at lookback", persisted: 744, lookback: 744, latest: 1000, start: 744, resumed: true, behind: 256},
		{name: "inside lookback", persisted: 900, lookback: 744, latest: 1000, start: 744, resumed: true},
		{name: "ahead of head", persisted: 1100, lookback: 744, latest: 1000, start: 744, resumed: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			start, resumed, behind := ResumeCursor(tt.persisted, tt.lookback, tt.latest)
			if start != tt.start || resumed != tt.resumed || behind != tt.behind {
				t.Fatalf("ResumeCursor() = (%d,%t,%d), want (%d,%t,%d)", start, resumed, behind, tt.start, tt.resumed, tt.behind)
			}
		})
	}
}

// A corrupt cursor file must be fatal, never treated as a first run. Silently
// falling back to the lookback skips every event between the lost cursor and
// the lookback window, with nothing in the log to say so -- which is the outage
// the cursor exists to prevent, recreated by the recovery from losing it.
//
// A missing file is the opposite: that IS a first run, and it must load.
func TestLoadRecoveryState_RefusesDamagedState(t *testing.T) {
	write := func(t *testing.T, content string) string {
		t.Helper()
		path := filepath.Join(t.TempDir(), "cursors.json")
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatalf("seed: %v", err)
		}
		return path
	}
	valid := `{"version":2,"cursors":{"cosmos":10,"eth_send":20,"eth_write_ack":30}}`

	t.Run("a truncated file is fatal", func(t *testing.T) {
		for name, cut := range map[string]int{
			"mid object": len(valid) / 2,
			"mid key":    12,
			"first byte": 1,
		} {
			t.Run(name, func(t *testing.T) {
				resetLoadedRecoveryStoresForTest()
				if _, err := LoadRecoveryState(write(t, valid[:cut])); err == nil {
					t.Fatal("a truncated cursor file loaded as if it were a first run")
				}
			})
		}
	})

	t.Run("an unknown version is fatal rather than partially read", func(t *testing.T) {
		resetLoadedRecoveryStoresForTest()
		_, err := LoadRecoveryState(write(t, `{"version":99,"cursors":{"cosmos":10}}`))
		if err == nil {
			t.Fatal("a future version loaded; a field this build does not understand would be dropped silently")
		}
		if !strings.Contains(err.Error(), "version") {
			t.Errorf("error %q does not say the version is the problem", err)
		}
	})

	t.Run("a missing file is a first run, not an error", func(t *testing.T) {
		resetLoadedRecoveryStoresForTest()
		store, err := LoadRecoveryState(filepath.Join(t.TempDir(), "cursors.json"))
		if err != nil {
			t.Fatalf("a first run was rejected: %v", err)
		}
		if _, ok := store.Get(); ok {
			t.Fatal("a first run reported durable cursors")
		}
	})

	// The control: the file this test truncates must load intact, or the cases
	// above would pass against a loader that rejects everything.
	t.Run("an intact file loads", func(t *testing.T) {
		resetLoadedRecoveryStoresForTest()
		store, err := LoadRecoveryState(write(t, valid))
		if err != nil {
			t.Fatalf("intact state was rejected: %v", err)
		}
		got, ok := store.Get()
		if !ok {
			t.Fatal("intact state reported no cursors")
		}
		if want := (RecoveryCursors{"cosmos": 10, "eth_send": 20, "eth_write_ack": 30}); !got.equal(want) {
			t.Fatalf("cursors = %+v, want %+v", got, want)
		}
	})
}

// Reported by @DongLieu on #440. The record changed shape in this PR -- a
// per-source map of chain-named fields became one map keyed by stream -- while
// the file version stayed 1.
//
// The danger is that the old file is not malformed. It unmarshals cleanly into
// the new struct: "sources" is simply an unknown field, "cursors" is absent, so
// Cursors comes back nil and the version check passes. The store would then
// report itself LOADED with no cursors, every stream would miss its key, and
// every recovery scan would quietly restart from the startup lookback -- the
// exact event loss a durable cursor exists to prevent, arriving at the moment of
// upgrade and saying nothing.
//
// The fixture is the real previous format, not a hand-shortened one, because the
// failure depends on it deserializing successfully.
func TestLoadRecoveryState_RefusesThePreviousRecordShape(t *testing.T) {
	resetLoadedRecoveryStoresForTest()

	legacy := `{"version":1,"sources":{"cosmoshub-1":{"cosmos_height":8123,` +
		`"eth_send_block":4477,"eth_write_ack_block":4470}}}`
	path := filepath.Join(t.TempDir(), RecoveryCursorsFileName)
	if err := os.WriteFile(path, []byte(legacy), 0o600); err != nil {
		t.Fatalf("write legacy state: %v", err)
	}

	store, err := LoadRecoveryState(path)
	if err == nil {
		cursors, loaded := store.Get()
		t.Fatalf("a version-1 file was accepted (loaded=%v, cursors=%v); the old cursors are "+
			"unreadable in the new shape, so every stream would silently fall back to the "+
			"startup lookback and lose the progress this file exists to keep", loaded, cursors)
	}
	// The operator has to learn which versions are involved and what to do.
	for _, want := range []string{"version 1", "want 2", "delete it"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q does not mention %q", err, want)
		}
	}
}
