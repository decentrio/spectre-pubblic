package main

import (
	"relayer/services"
	"testing"
)

// destHasReturnLeg decides whether a Cosmos→L2 destination may record owed
// acknowledgements. Getting it wrong in the permissive direction is the failure
// worth a test: the debt is durable, nothing in the process can clear it, and it
// is eventually reported overdue for a packet another relayer may have
// acknowledged.
//
// Found in review: the gate used to be `len(l2Sources) > 0`, a global count. A
// source matches exactly one destination, so in any multi-L2 deployment every
// other destination was marked settleable.
func TestDestHasReturnLeg(t *testing.T) {
	// Distinct allocations standing in for per-destination Services instances.
	// Identity is the whole point -- sharing the instance IS the settlement
	// mechanism -- so the test must not compare by value.
	opDest := &services.Services{}
	baseDest := &services.Services{}
	arbDest := &services.Services{}

	for _, tc := range []struct {
		name     string
		dest     *services.Services
		settling []*services.Services
		want     bool
	}{
		{
			name: "forward-only: no source at all",
			dest: opDest,
			want: false,
		},
		{
			name:     "the destination a source resolved to",
			dest:     opDest,
			settling: []*services.Services{opDest},
			want:     true,
		},
		{
			name: "multi-L2: a sibling's return leg is not this one's",
			dest: baseDest,
			// one L2 source, and it matched the OP destination
			settling: []*services.Services{opDest},
			want:     false,
		},
		{
			name:     "multi-L2: each matched destination is settleable",
			dest:     arbDest,
			settling: []*services.Services{opDest, arbDest},
			want:     true,
		},
		{
			name:     "a nil destination is never settleable",
			dest:     nil,
			settling: []*services.Services{opDest},
			want:     false,
		},
		{
			name:     "a nil entry in settling matches nothing",
			dest:     opDest,
			settling: []*services.Services{nil},
			want:     false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := destHasReturnLeg(tc.dest, tc.settling); got != tc.want {
				t.Fatalf("destHasReturnLeg = %v, want %v: %s", got, tc.want, map[bool]string{
					true:  "this destination has no module sharing its ledger, so a debt it records can never be settled",
					false: "a source resolved to this destination, so its debts do get cleared",
				}[got])
			}
		})
	}

	// The multi-L2 shape the review named, asserted as a whole rather than one
	// destination at a time: one l2_to_cosmos source among several cosmos_to_l2
	// destinations must enable the ledger on exactly the destination it matched.
	t.Run("only the matched destination records owed acks", func(t *testing.T) {
		dests := []*services.Services{{}, {}, {}}
		matched := dests[1]

		var enabled []int
		for i, d := range dests {
			if destHasReturnLeg(d, []*services.Services{matched}) {
				enabled = append(enabled, i)
			}
		}

		if len(enabled) != 1 || enabled[0] != 1 {
			t.Fatalf("destinations with the ledger enabled = %v, want exactly [1]: "+
				"the other two have no return leg and would persist debt forever", enabled)
		}
	})

	// The mutation that survived last round, now as an assertion: a forward-only
	// deployment must record no debt at all, because a debt recorded there is
	// durable, unsettleable, and eventually reported overdue for a packet another
	// relayer may have acknowledged. start.go asks destHasReturnLeg this exact
	// question for its one destination (with A7 there is no list to walk any more),
	// and "no l2_to_cosmos source configured" is an empty settling set.
	t.Run("forward only deployment records no owed acks", func(t *testing.T) {
		if destHasReturnLeg(&services.Services{}, nil) {
			t.Fatal("a destination with no return leg enabled the owed-ack ledger: " +
				"every successful receive now persists a debt this process can never settle")
		}
	})
}
