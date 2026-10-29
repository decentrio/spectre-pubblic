package utils

import (
	"bytes"
	"encoding/binary"
	"testing"

	channeltypesv2 "github.com/cosmos/ibc-go/v10/modules/core/04-channel/v2/types"
)

func TestBytesToBytes32(t *testing.T) {
	t.Run("exact 32 bytes", func(t *testing.T) {
		input := make([]byte, 32)
		for i := range input {
			input[i] = byte(i)
		}

		result := BytesToBytes32(input)

		if !bytes.Equal(result[:], input) {
			t.Errorf("expected %v, got %v", input, result[:])
		}
	})

	t.Run("less than 32 bytes", func(t *testing.T) {
		input := []byte{0xAA, 0xBB, 0xCC}

		result := BytesToBytes32(input)

		if !bytes.Equal(result[:3], input) {
			t.Errorf("first 3 bytes should match input, got %v", result[:3])
		}
		for i := 3; i < 32; i++ {
			if result[i] != 0 {
				t.Errorf("expected zero padding at index %d, got %d", i, result[i])
			}
		}
	})

	t.Run("empty", func(t *testing.T) {
		result := BytesToBytes32([]byte{})

		expected := [32]byte{}
		if result != expected {
			t.Errorf("expected all zeros, got %v", result)
		}
	})

	t.Run("more than 32 bytes", func(t *testing.T) {
		input := make([]byte, 64)
		for i := range input {
			input[i] = byte(i)
		}

		result := BytesToBytes32(input)

		if !bytes.Equal(result[:], input[:32]) {
			t.Errorf("expected first 32 bytes of input, got %v", result[:])
		}
	})
}

func TestIBCCommitmentPath_NormalPacket(t *testing.T) {
	packet := channeltypesv2.Packet{
		Sequence:     1,
		SourceClient: "07-tendermint-0",
	}

	path := IBCCommitmentPath(packet, []byte{1})

	if len(path) != 2 {
		t.Fatalf("expected 2 path elements, got %d", len(path))
	}
	if !bytes.Equal(path[0], []byte("ibc")) {
		t.Errorf("first element should be 'ibc', got %q", path[0])
	}

	sourceClientBytes := []byte("07-tendermint-0")
	separator := []byte{1}
	seqBytes := make([]byte, 8)
	binary.BigEndian.PutUint64(seqBytes, 1)

	expected := append(sourceClientBytes, separator...)
	expected = append(expected, seqBytes...)

	if !bytes.Equal(path[1], expected) {
		t.Errorf("second element mismatch\nexpected: %v\ngot:      %v", expected, path[1])
	}
}

func TestIBCCommitmentPath_HighSequence(t *testing.T) {
	var seq uint64 = 999999999

	packet := channeltypesv2.Packet{
		Sequence:     seq,
		SourceClient: "07-tendermint-0",
	}

	path := IBCCommitmentPath(packet, []byte{1})

	if len(path) != 2 {
		t.Fatalf("expected 2 path elements, got %d", len(path))
	}
	if !bytes.Equal(path[0], []byte("ibc")) {
		t.Errorf("first element should be 'ibc', got %q", path[0])
	}

	secondElem := path[1]
	sourceClient := "07-tendermint-0"
	expectedLen := len(sourceClient) + 1 + 8
	if len(secondElem) != expectedLen {
		t.Fatalf("expected second element length %d, got %d", expectedLen, len(secondElem))
	}

	seqOffset := len(sourceClient) + 1
	gotSeq := binary.BigEndian.Uint64(secondElem[seqOffset:])
	if gotSeq != seq {
		t.Errorf("expected sequence %d, got %d", seq, gotSeq)
	}

	if secondElem[len(sourceClient)] != 1 {
		t.Errorf("expected separator byte 0x01, got 0x%02x", secondElem[len(sourceClient)])
	}
}
