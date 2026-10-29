package services

import (
	"log"

	rpchttp "github.com/cometbft/cometbft/rpc/client/http"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/ethclient"
)

// CosmosEndpoint is the transport to a Cosmos chain. It deliberately carries
// no counterparty state or transaction configuration.
type CosmosEndpoint struct{ Client *rpchttp.HTTP }

// EVMContracts contains the contracts used by the EVM side of a relay path.
// Zero addresses retain the former "not configured" behaviour; consumers that
// require an address validate it at their boundary.
type EVMContracts struct {
	Router, SignatureVerifier, Membership, Misbehaviour, UpdateClient, RoleManager, SpectreClient common.Address
}

// EVMEndpoint is the transport and contract set for one EVM chain.
type EVMEndpoint struct {
	Client       *ethclient.Client
	WSURL        string
	BeaconAPIURL string
	Contracts    EVMContracts
}

// ClientIDs name the two counterparty clients without tying either identifier
// to a shared process-wide context.
type ClientIDs struct {
	CosmosOnEVM string
	EVMOnCosmos string
}

func (e CosmosEndpoint) CosmosClient() *rpchttp.HTTP { return e.Client }

func (e EVMEndpoint) EthClient() *ethclient.Client { return e.Client }
func (e EVMEndpoint) EthWsURL() string             { return e.WSURL }
func (e EVMEndpoint) MembershipContract() *common.Address {
	return &e.Contracts.Membership
}
func (e EVMEndpoint) MisbehaviourContract() *common.Address {
	return &e.Contracts.Misbehaviour
}
func (e EVMEndpoint) UpdateClientContract() *common.Address {
	return &e.Contracts.UpdateClient
}
func (e EVMEndpoint) RoleManagerAddress() *common.Address {
	return &e.Contracts.RoleManager
}
func (e EVMEndpoint) RouterContract() *common.Address { return &e.Contracts.Router }

// RouterManagesProofSubmission reports whether proof calls must route through
// ICS26Router rather than go directly to SpectreClient. It is the same question
// as "can a client update ride inside the packet multicall", because only that
// deployment shape can express the update as an inner router call.
//
// One definition, and it lives on the type that owns both addresses. There were
// three: an unexported predicate in transaction, an exported wrapper around it,
// and a byte-identical copy in chain/evm. All three agreed, which is the reason
// to merge them now rather than after they stop agreeing -- and the L2 folding
// work was about to add a fourth.
//
// The old copies each began by checking the two accessors for nil. Both return
// the address of a struct field on a value receiver, so neither can be nil and
// the branch was unreachable; EVMEndpoint is a concrete struct with no interface
// behind it, so there is no other implementation that could differ. A guard that
// cannot fire reads as protection the caller does not have.
func (e EVMEndpoint) RouterManagesProofSubmission() bool {
	roleManager, router := e.Contracts.RoleManager, e.Contracts.Router
	if roleManager == (common.Address{}) || router == (common.Address{}) {
		return false // an unconfigured deployment is not a router-managed one
	}
	return roleManager == router
}
func (e EVMEndpoint) SpectreClientContract() *common.Address {
	return &e.Contracts.SpectreClient
}

// RelayDeps contains the complete wiring assembled by the command layer for a
// relay process. It is intentionally a composition-root value: adapters,
// subscribers, workers, and transaction handlers receive only the endpoint,
// identifier, and configuration values they actually use.
type RelayDeps struct {
	Cosmos CosmosEndpoint
	EVM    EVMEndpoint
	IDs    ClientIDs
	Config Config
	Logger *log.Logger
}
