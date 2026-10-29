package transaction

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

// The revert data of 0x11014a24…3f88 on Sepolia (#541), exactly as `cast run`
// printed it: an expired pinned snapshot, 4042888s old against a 57600s period.
const insufficientTrustingPeriodRevert = "0xdb020436" +
	"00000000000000000000000000000000000000000000000000000000003db088" +
	"000000000000000000000000000000000000000000000000000000000000e100"

func TestDecodeRevert(t *testing.T) {
	for _, tc := range []struct {
		name   string
		err    error
		want   string
		wantOK bool
	}{
		{"#541 trusting period", testDataError{data: insufficientTrustingPeriodRevert},
			"InsufficientTrustingPeriod(4042888, 57600)", true},
		{"no-argument error", testDataError{data: fmt.Sprintf("0x%x", errorSelector("FrozenClientState()"))},
			"FrozenClientState()", true},
		{"wrapped", fmt.Errorf("call: %w", testDataError{data: insufficientTrustingPeriodRevert}),
			"InsufficientTrustingPeriod(4042888, 57600)", true},
		{"require string", testDataError{data: "0x08c379a0" +
			"0000000000000000000000000000000000000000000000000000000000000020" +
			"0000000000000000000000000000000000000000000000000000000000000004" +
			"6e6f706500000000000000000000000000000000000000000000000000000000"},
			"Error(nope)", true},
		{"unknown selector", testDataError{data: "0xdeadbeef"}, "unknown custom error 0xdeadbeef", true},
		{"selector with bad arguments", testDataError{data: "0xdb020436" + "00"},
			"InsufficientTrustingPeriod(<undecodable arguments", true},
		// Nethermind / Erigon put "Reverted 0x..." in the data member, a shape
		// services.IsEVMRevert already accepts; the decoder lost it (review of 65786fb).
		{"Reverted prefix", testDataError{data: "Reverted " + insufficientTrustingPeriodRevert},
			"InsufficientTrustingPeriod(4042888, 57600)", true},
		{"Reverted prefix, any case and spacing", testDataError{data: "  reverted  " + insufficientTrustingPeriodRevert},
			"InsufficientTrustingPeriod(4042888, 57600)", true},
		{"execution reverted: prefix", testDataError{data: "execution reverted: " + insufficientTrustingPeriodRevert},
			"InsufficientTrustingPeriod(4042888, 57600)", true},
		{"other prefix is not stripped", testDataError{data: "boom " + insufficientTrustingPeriodRevert}, "", false},
		{"no data", testDataError{data: nil}, "", false},
		{"short data", testDataError{data: "0xdb02"}, "", false},
		{"plain error", errors.New("execution reverted"), "", false},
		{"nil", nil, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := decodeRevert(tc.err)
			if ok != tc.wantOK || !strings.HasPrefix(got, tc.want) {
				t.Fatalf("decodeRevert = %q, %v; want prefix %q, %v", got, ok, tc.want, tc.wantOK)
			}
		})
	}
}

func TestDescribeRevert(t *testing.T) {
	got := describeRevert(testDataError{data: insufficientTrustingPeriodRevert})
	if want := "execution reverted: InsufficientTrustingPeriod(4042888, 57600)"; got != want {
		t.Fatalf("describeRevert = %q, want %q", got, want)
	}
	if got := describeRevert(errors.New("dial tcp: connection refused")); got != "dial tcp: connection refused" {
		t.Fatalf("an undecodable error must pass through unchanged, got %q", got)
	}
}

// The classifiers that route a revert to a hold or a cache retry now read the
// data through revertData; they must keep matching what they matched before.
func TestRevertErrorNameStillMatchesSelectors(t *testing.T) {
	frozen := testDataError{data: fmt.Sprintf("0x%x", errorSelector("FrozenClientState()"))}
	if name, ok := clientNotActiveErrorName(frozen); !ok || name != "FrozenClientState" {
		t.Fatalf("clientNotActiveErrorName = %q, %v", name, ok)
	}
	if _, ok := clientNotActiveErrorName(testDataError{data: insufficientTrustingPeriodRevert}); ok {
		t.Fatal("InsufficientTrustingPeriod must not classify as FrozenClientState")
	}
}
