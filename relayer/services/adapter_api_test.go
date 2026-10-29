package services

import (
	"bytes"
	"errors"
	"log"
	"path/filepath"
	"strings"
	"testing"

	channeltypesv2 "github.com/cosmos/ibc-go/v10/modules/core/04-channel/v2/types"
)

func TestUntrackCosmosPending_LogsPersistenceFailure(t *testing.T) {
	tests := []struct {
		name    string
		track   func(*Services, channeltypesv2.Packet) bool
		untrack func(*Services, channeltypesv2.Packet)
		tracker func(*Services) *PendingPacketTracker
		kind    string
	}{
		{
			name:    "cosmos",
			track:   func(s *Services, packet channeltypesv2.Packet) bool { return s.TrackCosmosPending(packet, 1) },
			untrack: (*Services).UntrackCosmosPending,
			tracker: func(s *Services) *PendingPacketTracker { return s.BatchBuilder.PendingTracker },
			kind:    "Cosmos",
		},
		{
			name:    "l2",
			track:   func(s *Services, packet channeltypesv2.Packet) bool { return s.TrackL2Pending(packet, 1) },
			untrack: (*Services).UntrackL2Pending,
			tracker: func(s *Services) *PendingPacketTracker { return s.BatchBuilder.L2PendingTracker },
			kind:    "L2",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			svc, err := NewWithPendingState(nil, nil, DefaultConfig(), filepath.Join(t.TempDir(), "state"))
			if err != nil {
				t.Fatal(err)
			}
			packet := channeltypesv2.Packet{SourceClient: "source", DestinationClient: "destination", Sequence: 42}
			if !tc.track(svc, packet) {
				t.Fatal("could not persist pending packet")
			}
			tc.tracker(svc).writeState = func(string, []byte) error { return errors.New("disk full") }

			var logs bytes.Buffer
			originalWriter := log.Writer()
			log.SetOutput(&logs)
			t.Cleanup(func() { log.SetOutput(originalWriter) })

			tc.untrack(svc, packet)

			want := "[PendingTracker][ATTENTION] failed to persist removal of settled " + tc.kind + " packet seq=42"
			if !strings.Contains(logs.String(), want) {
				t.Fatalf("persistence failure was not logged with context %q: %q", want, logs.String())
			}
			if got := tc.tracker(svc).Len(); got != 1 {
				t.Fatalf("failed persistence removed %s pending packet", tc.name)
			}
		})
	}
}

func TestTrackL2Pending(t *testing.T) {
	svc := New(nil, nil, DefaultConfig())
	pkt := channeltypesv2.Packet{SourceClient: "l2-client-0", Sequence: 9}

	svc.TrackL2Pending(pkt, 456)
	pending := svc.BatchBuilder.L2PendingTracker.GetAll()
	if len(pending) != 1 {
		t.Fatalf("pending len = %d, want 1", len(pending))
	}
	if pending[0].BlockNumber != 456 {
		t.Fatalf("pending block = %d, want 456", pending[0].BlockNumber)
	}

	svc.UntrackL2Pending(pkt)
	if got := svc.BatchBuilder.L2PendingTracker.Len(); got != 0 {
		t.Fatalf("pending len after untrack = %d, want 0", got)
	}
}
