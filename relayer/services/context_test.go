package services

import (
	"testing"

	"github.com/ethereum/go-ethereum/common"
)

// The predicate that had three copies. Its truth table is short, which is part
// of why it was easy to write out again each time it was needed -- and why the
// copies agreed right up until one of them would not have.
func TestEVMEndpoint_RouterManagesProofSubmission(t *testing.T) {
	addr := func(b byte) common.Address { return common.Address{b} }

	for _, tc := range []struct {
		name        string
		roleManager common.Address
		router      common.Address
		want        bool
	}{
		{
			// The deployment shape the whole predicate exists to name: the router
			// IS the role manager, so proof calls route through it and a client
			// update can ride inside the packet multicall.
			name: "the router is the role manager", roleManager: addr(1), router: addr(1), want: true,
		},
		{
			name: "a separate role manager", roleManager: addr(2), router: addr(1), want: false,
		},
		{
			// Two unconfigured addresses are equal, and reading that as "the router
			// manages submission" would fold an update into a multicall against a
			// contract that is not there.
			name: "both unset", want: false,
		},
		{
			name: "role manager unset", router: addr(1), want: false,
		},
		{
			name: "router unset", roleManager: addr(1), want: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := EVMEndpoint{Contracts: EVMContracts{RoleManager: tc.roleManager, Router: tc.router}}
			if got := e.RouterManagesProofSubmission(); got != tc.want {
				t.Fatalf("RouterManagesProofSubmission() = %v, want %v", got, tc.want)
			}
		})
	}
}
