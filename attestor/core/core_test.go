package core

import (
	"errors"
	"testing"
)

func TestRunModeValidation(t *testing.T) {
	for _, mode := range []RunMode{RunModeUnsafe, RunModeSafe, RunModeFinalized} {
		if !mode.Valid() {
			t.Fatalf("RunMode %q is not valid", mode)
		}
	}
	if RunMode("other").Valid() {
		t.Fatal("unknown run mode is valid")
	}
}

func TestErrorKindPreservesClassification(t *testing.T) {
	cause := errors.New("upstream down")
	err := NewError(ErrorUnavailable, "query replica", cause)
	if ErrorKindOf(err) != ErrorUnavailable {
		t.Fatalf("kind = %v, want Unavailable", ErrorKindOf(err))
	}
	if !errors.Is(err, cause) {
		t.Fatal("classified error did not preserve cause")
	}
	if ErrorKindOf(errors.New("unclassified")) != ErrorInternal {
		t.Fatal("unclassified error did not fail closed as Internal")
	}
}

func TestCloneDoesNotShareTransportBytes(t *testing.T) {
	index := uint64(7)
	root := AttestedRoot{Root: []byte{1}, AssertionHash: []byte{2}, GameIndex: &index}
	copy := root.Clone()
	copy.Root[0] = 9
	copy.AssertionHash[0] = 8
	*copy.GameIndex = 6
	if root.Root[0] != 1 || root.AssertionHash[0] != 2 || *root.GameIndex != 7 {
		t.Fatalf("clone mutated original: %+v", root)
	}
}
