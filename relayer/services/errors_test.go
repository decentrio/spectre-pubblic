package services

import (
	"errors"
	"fmt"
	"testing"
)

// A batch that splits can commit its leading sub-batches and then fail. The
// count of what landed is the only thing that lets a caller re-queue the tail
// without re-submitting the head, so it has to survive being wrapped: the error
// travels up through an adapter before anyone reads it.
func TestCommittedBeforeFailure(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want int
	}{
		{
			name: "a partial batch reports its delivered prefix",
			err:  &BatchPartialError{SucceededCount: 2, Err: errors.New("node went away")},
			want: 2,
		},
		{
			name: "the prefix survives wrapping",
			err:  fmt.Errorf("cosmos dest: %w", &BatchPartialError{SucceededCount: 3, Err: errors.New("node went away")}),
			want: 3,
		},
		{
			// Nothing landed, and saying "0" is the honest answer rather than a
			// default: a single transaction either executes or it does not.
			name: "a whole-batch failure delivered nothing",
			err:  errors.New("broadcast failed"),
			want: 0,
		},
		{
			name: "no error means the caller is not on this path",
			err:  nil,
			want: 0,
		},
		{
			// Zero is indistinguishable from "no prefix", so it must not be
			// reported as a partial commit the caller then trims by.
			name: "a partial error claiming zero is not a commit",
			err:  &BatchPartialError{SucceededCount: 0, Err: errors.New("first sub-batch failed")},
			want: 0,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := CommittedBeforeFailure(tc.err); got != tc.want {
				t.Fatalf("CommittedBeforeFailure(%v) = %d, want %d", tc.err, got, tc.want)
			}
		})
	}
}
