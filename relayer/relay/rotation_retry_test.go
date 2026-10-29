package relay

import (
	"context"
	"fmt"
	"relayer/services"
	"testing"
	"testing/synctest"
	"time"
)

// Drive the actual scheduler with virtual time. A packet update at the rotation
// deadline leaves latestHeight current but the pinned snapshot unrefreshed.
// Waiting for another source block must still leave time for the proof and tx.
func TestPeriodicUpdateLoop_CaughtUpRotationFinishesBeforeExpiry(t *testing.T) {
	for _, period := range []time.Duration{4 * time.Second, 2 * time.Minute, 4 * time.Minute, 16 * time.Hour} {
		t.Run(period.String(), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				snapshotAt := time.Now()
				margin := services.RefreshSafetyMargin(period)
				interval := min(services.DEFAULT_REFRESH_INTERVAL, period-margin)
				// Even repeated caught-up results must not grow the poll delay.
				sourceAdvancesAt := snapshotAt.Add(interval + margin/4)
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				var attempts []time.Time
				m := NewModule("test", "client-0", &mockSource{}, &mockDest{}, &mockBuilder{},
					WithPeriodicUpdate(interval, interval, func(context.Context) error {
						attempts = append(attempts, time.Now())
						if time.Now().Before(sourceAdvancesAt) {
							return fmt.Errorf("refresh: %w", services.ErrForcedRotationNotReady)
						}
						// Leave a concrete budget for generating and landing the update.
						time.Sleep(margin / 2)
						cancel()
						return nil
					}),
				)
				m.periodicUpdateLoop(ctx)
				if len(attempts) < 2 {
					t.Fatalf("attempts=%v, want caught-up attempt and retry", attempts)
				}
				if !time.Now().Before(snapshotAt.Add(period)) {
					t.Fatalf("rotation finished at %s, snapshot expired at %s", time.Since(snapshotAt), period)
				}
				for i := 1; i < len(attempts); i++ {
					if gap := attempts[i].Sub(attempts[i-1]); gap > min(time.Second, margin/4) {
						t.Fatalf("caught-up retry gap %s consumes too much of safety margin %s", gap, margin)
					}
				}
			})
		})
	}
}
