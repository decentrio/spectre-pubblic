package services

import (
	"encoding/json"
	"fmt"
	"os"
	"sync"
)

// RecoveryCursorsFileName is the cursor file inside a process's state
// directory. The directory is chosen by the composition root (cmd), which is
// the only place that knows the config path this process was started with.
const (
	RecoveryCursorsFileName = "cursors.json"
	// recoveryStateVersion is 2 because the record changed shape in this commit:
	// a per-source map of chain-named fields became one map keyed by stream name.
	//
	// The bump is the whole safety mechanism. A version-1 file carries "sources"
	// and no "cursors", so it unmarshals WITHOUT error into the new struct, leaves
	// Cursors nil, passes a version check that still said 1, and is stored as
	// "loaded with no cursors" -- every stream then misses its key and silently
	// falls back to the startup lookback. That is exactly the event loss a durable
	// cursor exists to prevent, arriving quietly at the moment of upgrade.
	//
	// Bumping makes the existing check reject it by name instead. There is no
	// converter: nothing is deployed outside devnets, and a converter for a shape
	// with no users is machinery that would itself never be exercised.
	recoveryStateVersion = 2
)

// RecoveryCursors maps a scan stream to the next height or block it must scan.
// A missing key means that stream has no durable cursor yet and must use its
// lookback; a zero value means the same and is never written.
//
// Keys belong to the streams, not to this package. The fields were named for
// chains -- CosmosHeight, EthSendBlock, EthWriteAckBlk -- so every new chain
// added a field here, and a fourth would have added another. B6 put the
// direction in the DIRECTORY path, so what is left to distinguish is the stream
// within one process, and a string does that without this package learning any
// chain's name.
type RecoveryCursors map[string]uint64

// clone returns a copy, so a caller cannot reach into the store's own map and a
// stored map cannot be mutated behind the mutex that guards it.
func (c RecoveryCursors) clone() RecoveryCursors {
	if c == nil {
		return nil
	}
	out := make(RecoveryCursors, len(c))
	for stream, cursor := range c {
		out[stream] = cursor
	}
	return out
}

func (c RecoveryCursors) equal(other RecoveryCursors) bool {
	if len(c) != len(other) {
		return false
	}
	for stream, cursor := range c {
		if other[stream] != cursor {
			return false
		}
	}
	return true
}

// recoveryStateFile is one process's cursors. It used to carry a map keyed by
// source id, from when one process served several sources. A1 ended that: a
// process serves exactly one source -> destination pair, so the map could only
// ever hold one entry, and a shared file keyed by id is precisely the shape that
// lets two processes overwrite each other.
type recoveryStateFile struct {
	Version uint32          `json:"version"`
	Cursors RecoveryCursors `json:"cursors"`
}

// RecoveryStateStore persists this process's recovery cursors in one small JSON
// file. Instances are memoized per path so the Cosmos and ETH recovery streams,
// which own different fields of the same record, share one in-memory view.
type RecoveryStateStore struct {
	path string

	mtx     sync.Mutex
	cursors RecoveryCursors
	loaded  bool
}

var loadedRecoveryStores = struct {
	mtx    sync.Mutex
	byPath map[string]*RecoveryStateStore
}{byPath: map[string]*RecoveryStateStore{}}

// LoadRecoveryState loads and memoizes a recovery store. A missing file is a
// first run; unreadable, corrupt, or unsupported state is fatal to the caller.
func LoadRecoveryState(path string) (*RecoveryStateStore, error) {
	loadedRecoveryStores.mtx.Lock()
	defer loadedRecoveryStores.mtx.Unlock()
	if existing, ok := loadedRecoveryStores.byPath[path]; ok {
		return existing, nil
	}
	store, err := loadRecoveryStateFile(path)
	if err != nil {
		return nil, err
	}
	loadedRecoveryStores.byPath[path] = store
	return store, nil
}

func resetLoadedRecoveryStoresForTest() {
	loadedRecoveryStores.mtx.Lock()
	defer loadedRecoveryStores.mtx.Unlock()
	loadedRecoveryStores.byPath = map[string]*RecoveryStateStore{}
}

func loadRecoveryStateFile(path string) (*RecoveryStateStore, error) {
	store := &RecoveryStateStore{path: path}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return store, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read recovery state %q: %w", path, err)
	}

	var file recoveryStateFile
	if err := json.Unmarshal(data, &file); err != nil {
		return nil, fmt.Errorf("parse recovery state %q (delete it to resume from the lookback window, accepting that older events may be lost): %w", path, err)
	}
	if file.Version != recoveryStateVersion {
		return nil, fmt.Errorf(
			"recovery state %q has version %d, want %d (delete it to resume from the lookback window, "+
				"accepting that older events may be lost): the record shape changed and the old cursors "+
				"cannot be read", path, file.Version, recoveryStateVersion)
	}
	store.cursors = file.Cursors
	store.loaded = true
	return store, nil
}

// Get returns this process's cursors. The second result is false on a first
// run, which is a legitimate state meaning "use the lookback" -- distinct from a
// corrupt file, which never reaches here because loading fails outright.
func (s *RecoveryStateStore) Get() (RecoveryCursors, bool) {
	s.mtx.Lock()
	defer s.mtx.Unlock()
	return s.cursors.clone(), s.loaded
}

// SaveCheckpoint atomically persists each supplied non-zero cursor exactly.
// Recovery subscribers use it after clamping progress to the oldest packet
// still queued or in flight: that conservative rewind is what makes a restart
// rescan work that only exists in memory. Zero fields remain unchanged so
// independently-owned streams can share one source record.
func (s *RecoveryStateStore) SaveCheckpoint(cursors RecoveryCursors) error {
	s.mtx.Lock()
	defer s.mtx.Unlock()

	previous := s.cursors
	merged := mergeRecoveryCursors(previous, cursors)
	if merged.equal(previous) && s.loaded {
		return nil
	}

	if err := writeRecoveryState(s.path, recoveryStateFile{Version: recoveryStateVersion, Cursors: merged}); err != nil {
		return err
	}
	// Publish the new in-memory cursor only after the atomic file replacement
	// succeeds. Otherwise an identical retry would look like a no-op and leave the
	// durable file permanently behind the value reported by Get.
	s.cursors = merged
	s.loaded = true
	return nil
}

// mergeRecoveryCursors overlays the non-zero entries of next onto previous.
//
// Zero means "this stream has nothing to say", not "rewind to zero": the two
// streams sharing one record each write only their own keys, and a save that
// carried the whole map would erase the other's progress. Note this is an
// overlay, not a maximum -- a cursor is ALLOWED to move backwards, because it
// is clamped to the lowest packet still queued or in flight, and a newly queued
// packet at a lower height is a legitimate reason to rescan. Forcing it to only
// advance looks safe and reopens the gap it exists to close.
func mergeRecoveryCursors(previous, next RecoveryCursors) RecoveryCursors {
	merged := previous.clone()
	if merged == nil {
		merged = RecoveryCursors{}
	}
	for stream, cursor := range next {
		if cursor == 0 {
			continue
		}
		merged[stream] = cursor
	}
	return merged
}

func writeRecoveryState(path string, file recoveryStateFile) error {
	data, err := json.MarshalIndent(file, "", "  ")
	if err != nil {
		return fmt.Errorf("encode recovery state: %w", err)
	}
	// Wrapped: writeStateFile names the temp file or the directory, not which
	// state this was, and both callers write different files through it.
	if err := writeStateFile(path, data); err != nil {
		return fmt.Errorf("write recovery state %q: %w", path, err)
	}
	return nil
}

// ResumeCursor never lets a persisted cursor shorten the normal lookback. A
// cursor older than the lookback extends recovery backwards; a newer cursor
// still rescans the lookback because scanned events may only have reached RAM.
func ResumeCursor(persisted, lookbackStart, latest uint64) (start uint64, resumed bool, behind uint64) {
	if persisted == 0 {
		return lookbackStart, false, 0
	}
	if persisted > lookbackStart {
		return lookbackStart, true, 0
	}
	if latest > persisted {
		behind = latest - persisted
	}
	return persisted, true, behind
}
