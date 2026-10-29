// Code generated - DO NOT EDIT.
// This file is a generated binding and any manual changes will be lost.

package contractUpdateClient

import (
	"errors"
	"math/big"
	"strings"

	ethereum "github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/accounts/abi/bind"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/event"
)

// Reference imports to suppress errors if they are not otherwise used.
var (
	_ = errors.New
	_ = big.NewInt
	_ = strings.NewReader
	_ = ethereum.NotFound
	_ = bind.Bind
	_ = common.Big1
	_ = types.BloomLookup
	_ = event.NewSubscription
	_ = abi.ConvertType
)

// IICS02ClientMsgsHeight is an auto generated low-level Go binding around an user-defined struct.
type IICS02ClientMsgsHeight struct {
	RevisionNumber uint64
	RevisionHeight uint64
}

// ISpectreClientMsgsBatchProof is an auto generated low-level Go binding around an user-defined struct.
type ISpectreClientMsgsBatchProof struct {
	Proof                  [8]*big.Int
	Commitments            [2]*big.Int
	CommitmentPok          [2]*big.Int
	Bucket                 uint16
	SignerIndices          []uint32
	PinnedValidatorIndices []uint32
	SignerPubkeys          [][32]byte
	Active                 []bool
}

// ISpectreClientMsgsMsgUpdateApplicationState is an auto generated low-level Go binding around an user-defined struct.
type ISpectreClientMsgsMsgUpdateApplicationState struct {
	TrustedConsensusState ISpectreTendermintMsgsConsensusState
	ProposedHeader        ISpectreTendermintMsgsHeader
	Time                  *big.Int
	Proof                 ISpectreClientMsgsBatchProof
}

// ISpectreClientMsgsVerifyHeaderOutput is an auto generated low-level Go binding around an user-defined struct.
type ISpectreClientMsgsVerifyHeaderOutput struct {
	TrustedConsensusState ISpectreTendermintMsgsConsensusState
	NewConsensusState     ISpectreTendermintMsgsConsensusState
	TrustedHeight         IICS02ClientMsgsHeight
	NewHeight             IICS02ClientMsgsHeight
}

// ISpectreTendermintMsgsBlockCommit is an auto generated low-level Go binding around an user-defined struct.
type ISpectreTendermintMsgsBlockCommit struct {
	Height     uint64
	Round      uint32
	BlockId    ISpectreTendermintMsgsBlockId
	CommitSigs []ISpectreTendermintMsgsCommitSig
}

// ISpectreTendermintMsgsBlockHeader is an auto generated low-level Go binding around an user-defined struct.
type ISpectreTendermintMsgsBlockHeader struct {
	Version            ISpectreTendermintMsgsVersion
	ChainId            string
	Height             uint64
	Time               *big.Int
	HasLastBlockId     bool
	LastBlockId        ISpectreTendermintMsgsBlockId
	HasLastCommitHash  bool
	LastCommitHash     [32]byte
	HasDataHash        bool
	DataHash           [32]byte
	ValidatorsHash     [32]byte
	NextValidatorsHash [32]byte
	ConsensusHash      [32]byte
	AppHash            [32]byte
	HasLastResultsHash bool
	LastResultsHash    [32]byte
	HasEvidenceHash    bool
	EvidenceHash       [32]byte
	ProposerAddress    []byte
}

// ISpectreTendermintMsgsBlockId is an auto generated low-level Go binding around an user-defined struct.
type ISpectreTendermintMsgsBlockId struct {
	HashData      [32]byte
	PartSetHeader ISpectreTendermintMsgsPartSetHeader
}

// ISpectreTendermintMsgsCommitSig is an auto generated low-level Go binding around an user-defined struct.
type ISpectreTendermintMsgsCommitSig struct {
	Flag             uint8
	ValidatorAddress [20]byte
}

// ISpectreTendermintMsgsConsensusState is an auto generated low-level Go binding around an user-defined struct.
type ISpectreTendermintMsgsConsensusState struct {
	Timestamp          *big.Int
	Root               [32]byte
	NextValidatorsHash [32]byte
}

// ISpectreTendermintMsgsHeader is an auto generated low-level Go binding around an user-defined struct.
type ISpectreTendermintMsgsHeader struct {
	SignedHeader  ISpectreTendermintMsgsSignedHeader
	TrustedHeight IICS02ClientMsgsHeight
}

// ISpectreTendermintMsgsPartSetHeader is an auto generated low-level Go binding around an user-defined struct.
type ISpectreTendermintMsgsPartSetHeader struct {
	Total    uint32
	HashData [32]byte
}

// ISpectreTendermintMsgsSignedHeader is an auto generated low-level Go binding around an user-defined struct.
type ISpectreTendermintMsgsSignedHeader struct {
	Header ISpectreTendermintMsgsBlockHeader
	Commit ISpectreTendermintMsgsBlockCommit
}

// ISpectreTendermintMsgsVersion is an auto generated low-level Go binding around an user-defined struct.
type ISpectreTendermintMsgsVersion struct {
	BlockVersion uint64
	AppVersion   uint64
}

// ContractUpdateClientMetaData contains all meta data concerning the ContractUpdateClient contract.
var ContractUpdateClientMetaData = &bind.MetaData{
	ABI: "[{\"type\":\"constructor\",\"inputs\":[{\"name\":\"signatureVerifier\",\"type\":\"address\",\"internalType\":\"address\"}],\"stateMutability\":\"nonpayable\"},{\"type\":\"function\",\"name\":\"verifyHeader\",\"inputs\":[{\"name\":\"msg_\",\"type\":\"tuple\",\"internalType\":\"structISpectreClientMsgs.MsgUpdateApplicationState\",\"components\":[{\"name\":\"trustedConsensusState\",\"type\":\"tuple\",\"internalType\":\"structISpectreTendermintMsgs.ConsensusState\",\"components\":[{\"name\":\"timestamp\",\"type\":\"uint128\",\"internalType\":\"uint128\"},{\"name\":\"root\",\"type\":\"bytes32\",\"internalType\":\"bytes32\"},{\"name\":\"nextValidatorsHash\",\"type\":\"bytes32\",\"internalType\":\"bytes32\"}]},{\"name\":\"proposedHeader\",\"type\":\"tuple\",\"internalType\":\"structISpectreTendermintMsgs.Header\",\"components\":[{\"name\":\"signedHeader\",\"type\":\"tuple\",\"internalType\":\"structISpectreTendermintMsgs.SignedHeader\",\"components\":[{\"name\":\"header\",\"type\":\"tuple\",\"internalType\":\"structISpectreTendermintMsgs.BlockHeader\",\"components\":[{\"name\":\"version\",\"type\":\"tuple\",\"internalType\":\"structISpectreTendermintMsgs.Version\",\"components\":[{\"name\":\"blockVersion\",\"type\":\"uint64\",\"internalType\":\"uint64\"},{\"name\":\"appVersion\",\"type\":\"uint64\",\"internalType\":\"uint64\"}]},{\"name\":\"chainId\",\"type\":\"string\",\"internalType\":\"string\"},{\"name\":\"height\",\"type\":\"uint64\",\"internalType\":\"uint64\"},{\"name\":\"time\",\"type\":\"uint128\",\"internalType\":\"uint128\"},{\"name\":\"hasLastBlockId\",\"type\":\"bool\",\"internalType\":\"bool\"},{\"name\":\"lastBlockId\",\"type\":\"tuple\",\"internalType\":\"structISpectreTendermintMsgs.BlockId\",\"components\":[{\"name\":\"hashData\",\"type\":\"bytes32\",\"internalType\":\"bytes32\"},{\"name\":\"partSetHeader\",\"type\":\"tuple\",\"internalType\":\"structISpectreTendermintMsgs.PartSetHeader\",\"components\":[{\"name\":\"total\",\"type\":\"uint32\",\"internalType\":\"uint32\"},{\"name\":\"hashData\",\"type\":\"bytes32\",\"internalType\":\"bytes32\"}]}]},{\"name\":\"hasLastCommitHash\",\"type\":\"bool\",\"internalType\":\"bool\"},{\"name\":\"lastCommitHash\",\"type\":\"bytes32\",\"internalType\":\"bytes32\"},{\"name\":\"hasDataHash\",\"type\":\"bool\",\"internalType\":\"bool\"},{\"name\":\"dataHash\",\"type\":\"bytes32\",\"internalType\":\"bytes32\"},{\"name\":\"validatorsHash\",\"type\":\"bytes32\",\"internalType\":\"bytes32\"},{\"name\":\"nextValidatorsHash\",\"type\":\"bytes32\",\"internalType\":\"bytes32\"},{\"name\":\"consensusHash\",\"type\":\"bytes32\",\"internalType\":\"bytes32\"},{\"name\":\"appHash\",\"type\":\"bytes32\",\"internalType\":\"bytes32\"},{\"name\":\"hasLastResultsHash\",\"type\":\"bool\",\"internalType\":\"bool\"},{\"name\":\"lastResultsHash\",\"type\":\"bytes32\",\"internalType\":\"bytes32\"},{\"name\":\"hasEvidenceHash\",\"type\":\"bool\",\"internalType\":\"bool\"},{\"name\":\"evidenceHash\",\"type\":\"bytes32\",\"internalType\":\"bytes32\"},{\"name\":\"proposerAddress\",\"type\":\"bytes\",\"internalType\":\"bytes\"}]},{\"name\":\"commit\",\"type\":\"tuple\",\"internalType\":\"structISpectreTendermintMsgs.BlockCommit\",\"components\":[{\"name\":\"height\",\"type\":\"uint64\",\"internalType\":\"uint64\"},{\"name\":\"round\",\"type\":\"uint32\",\"internalType\":\"uint32\"},{\"name\":\"blockId\",\"type\":\"tuple\",\"internalType\":\"structISpectreTendermintMsgs.BlockId\",\"components\":[{\"name\":\"hashData\",\"type\":\"bytes32\",\"internalType\":\"bytes32\"},{\"name\":\"partSetHeader\",\"type\":\"tuple\",\"internalType\":\"structISpectreTendermintMsgs.PartSetHeader\",\"components\":[{\"name\":\"total\",\"type\":\"uint32\",\"internalType\":\"uint32\"},{\"name\":\"hashData\",\"type\":\"bytes32\",\"internalType\":\"bytes32\"}]}]},{\"name\":\"commitSigs\",\"type\":\"tuple[]\",\"internalType\":\"structISpectreTendermintMsgs.CommitSig[]\",\"components\":[{\"name\":\"flag\",\"type\":\"uint8\",\"internalType\":\"enumISpectreTendermintMsgs.CommitSigFlag\"},{\"name\":\"validatorAddress\",\"type\":\"bytes20\",\"internalType\":\"bytes20\"}]}]}]},{\"name\":\"trustedHeight\",\"type\":\"tuple\",\"internalType\":\"structIICS02ClientMsgs.Height\",\"components\":[{\"name\":\"revisionNumber\",\"type\":\"uint64\",\"internalType\":\"uint64\"},{\"name\":\"revisionHeight\",\"type\":\"uint64\",\"internalType\":\"uint64\"}]}]},{\"name\":\"time\",\"type\":\"uint128\",\"internalType\":\"uint128\"},{\"name\":\"proof\",\"type\":\"tuple\",\"internalType\":\"structISpectreClientMsgs.BatchProof\",\"components\":[{\"name\":\"proof\",\"type\":\"uint256[8]\",\"internalType\":\"uint256[8]\"},{\"name\":\"commitments\",\"type\":\"uint256[2]\",\"internalType\":\"uint256[2]\"},{\"name\":\"commitmentPok\",\"type\":\"uint256[2]\",\"internalType\":\"uint256[2]\"},{\"name\":\"bucket\",\"type\":\"uint16\",\"internalType\":\"uint16\"},{\"name\":\"signerIndices\",\"type\":\"uint32[]\",\"internalType\":\"uint32[]\"},{\"name\":\"pinnedValidatorIndices\",\"type\":\"uint32[]\",\"internalType\":\"uint32[]\"},{\"name\":\"signerPubkeys\",\"type\":\"bytes32[]\",\"internalType\":\"bytes32[]\"},{\"name\":\"active\",\"type\":\"bool[]\",\"internalType\":\"bool[]\"}]}]}],\"outputs\":[{\"name\":\"\",\"type\":\"tuple\",\"internalType\":\"structISpectreClientMsgs.VerifyHeaderOutput\",\"components\":[{\"name\":\"trustedConsensusState\",\"type\":\"tuple\",\"internalType\":\"structISpectreTendermintMsgs.ConsensusState\",\"components\":[{\"name\":\"timestamp\",\"type\":\"uint128\",\"internalType\":\"uint128\"},{\"name\":\"root\",\"type\":\"bytes32\",\"internalType\":\"bytes32\"},{\"name\":\"nextValidatorsHash\",\"type\":\"bytes32\",\"internalType\":\"bytes32\"}]},{\"name\":\"newConsensusState\",\"type\":\"tuple\",\"internalType\":\"structISpectreTendermintMsgs.ConsensusState\",\"components\":[{\"name\":\"timestamp\",\"type\":\"uint128\",\"internalType\":\"uint128\"},{\"name\":\"root\",\"type\":\"bytes32\",\"internalType\":\"bytes32\"},{\"name\":\"nextValidatorsHash\",\"type\":\"bytes32\",\"internalType\":\"bytes32\"}]},{\"name\":\"trustedHeight\",\"type\":\"tuple\",\"internalType\":\"structIICS02ClientMsgs.Height\",\"components\":[{\"name\":\"revisionNumber\",\"type\":\"uint64\",\"internalType\":\"uint64\"},{\"name\":\"revisionHeight\",\"type\":\"uint64\",\"internalType\":\"uint64\"}]},{\"name\":\"newHeight\",\"type\":\"tuple\",\"internalType\":\"structIICS02ClientMsgs.Height\",\"components\":[{\"name\":\"revisionNumber\",\"type\":\"uint64\",\"internalType\":\"uint64\"},{\"name\":\"revisionHeight\",\"type\":\"uint64\",\"internalType\":\"uint64\"}]}]}],\"stateMutability\":\"nonpayable\"},{\"type\":\"error\",\"name\":\"ChainIdMismatch\",\"inputs\":[{\"name\":\"expected\",\"type\":\"string\",\"internalType\":\"string\"},{\"name\":\"actual\",\"type\":\"string\",\"internalType\":\"string\"}]},{\"type\":\"error\",\"name\":\"ConsensusStateHashMismatch\",\"inputs\":[{\"name\":\"expected\",\"type\":\"bytes32\",\"internalType\":\"bytes32\"},{\"name\":\"actual\",\"type\":\"bytes32\",\"internalType\":\"bytes32\"}]},{\"type\":\"error\",\"name\":\"ConsensusStateNotFound\",\"inputs\":[]},{\"type\":\"error\",\"name\":\"DirectCallNotAllowed\",\"inputs\":[]},{\"type\":\"error\",\"name\":\"FailedToVerifyHeader\",\"inputs\":[{\"name\":\"description\",\"type\":\"string\",\"internalType\":\"string\"}]},{\"type\":\"error\",\"name\":\"InvalidChainIdLength\",\"inputs\":[]},{\"type\":\"error\",\"name\":\"InvalidChainPrefixLength\",\"inputs\":[]},{\"type\":\"error\",\"name\":\"InvalidHeaderHeight\",\"inputs\":[{\"name\":\"height\",\"type\":\"uint64\",\"internalType\":\"uint64\"}]},{\"type\":\"error\",\"name\":\"MismatchedRevisionHeights\",\"inputs\":[{\"name\":\"expected\",\"type\":\"uint64\",\"internalType\":\"uint64\"},{\"name\":\"actual\",\"type\":\"uint64\",\"internalType\":\"uint64\"}]},{\"type\":\"error\",\"name\":\"ProofVerificationFailed\",\"inputs\":[]}]",
	Bin: "0x60c034607457601f611fe538819003918201601f19168301916001600160401b03831184841017607857808492602094604052833981010312607457516001600160a01b038116908190036074576080523060a052604051611f58908161008d823960805181610c51015260a05181607e0152f35b5f80fd5b634e487b7160e01b5f52604160045260245ffdfe60806040526004361015610011575f80fd5b5f3560e01c638787672614610024575f80fd5b34610ea1576020366003190112610ea15760043567ffffffffffffffff8111610ea15780600401908036039160031983019160c08312610ea1576100666116c5565b5073ffffffffffffffffffffffffffffffffffffffff7f00000000000000000000000000000000000000000000000000000000000000001630146116095767ffffffffffffffff7f5e9f695da54d5993169597b18a19a3159f4a62cbfd7f6669a996ca09c6df1702541693604051946100e0604087611685565b6040515f7f5e9f695da54d5993169597b18a19a3159f4a62cbfd7f6669a996ca09c6df1700548060011c91600182169182156115ff575b6020841083146115eb5783855284929081156115cc575060011461154e575b61014292500382611685565b8652602086019081527f5e9f695da54d5993169597b18a19a3159f4a62cbfd7f6669a996ca09c6df1703549563ffffffff8088169760481c166040805161018a606082611685565b81516101958161164d565b60ff7f5e9f695da54d5993169597b18a19a3159f4a62cbfd7f6669a996ca09c6df170154818116835260081c166020820152815260208101998a520190815260648501976101e38988611720565b916101f060848801611735565b9161022361021e61021761020d6102078880611772565b80611787565b604081019061179d565b36916117ec565b611914565b6020810167ffffffffffffffff815116602087019067ffffffffffffffff61024a8361175d565b16810361150c57506102a967ffffffffffffffff8351166102a461027c60606102766102078d80611772565b0161175d565b936040519261028a8461164d565b835267ffffffffffffffff6020840195168552369061185b565b611bc4565b60038110156114f857600281149081156114ed575b506114cf575067ffffffffffffffff808951169151160361147457506102e76102078580611772565b948536036102c08112610ea15760405196610260880188811067ffffffffffffffff8211176114605760405261031d368261185b565b8852604081013567ffffffffffffffff8111610ea157810136601f82011215610ea1576103519036906020813591016117ec565b916020890192835261036560608301611846565b60408a015261037660808301611749565b60608a015261038760a08301611891565b60808a0152606060bf19820112610ea15760408051916103a68361164d565b60c0840135835260df190112610ea1576040516103c28161164d565b60e083013563ffffffff81168103610ea15781526101008301356020820152602082015260a08901526103f86101208201611891565b60c089015261014081013560e08901526104156101608201611891565b6101008901526101808101356101208901526101a08101356101408901526101c08101356101608901526101e08101356101808901526102008101356101a08901526104646102208201611891565b6101c08901526102408101356101e08901526104836102608201611891565b6102008901526102808101356102208901526102a08101359067ffffffffffffffff8211610ea1570136601f82011215610ea1576104e0916104cf6104db9236906020813591016117ec565b6102408a015251611c62565b611cc5565b6101e096604051916104f28984611685565b600e835260208301601f198a0136823782515f9067ffffffffffffffff81511680611443575b50602081019067ffffffffffffffff82511680611418575b5061053d61056a93611c30565b915f9167ffffffffffffffff8151166113f0575b5067ffffffffffffffff8151166113d0575b5050611cc5565b9084511561127e575282516001101561127e57604083015261059c6104db67ffffffffffffffff604084015116611d18565b82516002101561127e5760608301526001600160801b036060820151165f906001600160801b03633b9aca008204169081151590816113b4575b633b9aca006001600160801b0391061680151580611388575b6105fb61060f95611c30565b935f9361136c575b50611351575050611cc5565b82516003101561127e576080830152608081015115155f1461132b5760a081015160208101519061064663ffffffff835116611e45565b80600101908160011161104257602301809111611042576106696106a291611c30565b9260086020850153602061069861069261068c63ffffffff855116600189611ea8565b87611e7b565b86611e92565b9101519084611f33565b5081516106ae81611e45565b602301928360231161104257610704826106fe6106f86106dc6106d761070a976107109a611907565b611c30565b96600a60208901536106ef600189611e92565b90519088611f33565b86611e7b565b85611ea8565b83611f0b565b50611cc5565b82516004101561127e5760a083015260c081015115611305576107396104db60e0830151611d4e565b82516005101561127e5760c0830152610100810151156112df576107646104db610120830151611d4e565b82516006101561127e5760e08301526107846104db610140830151611d4e565b82516007101561127e576101008301526107a56104db610160830151611d4e565b82516008101561127e576101208301526107c66104db610180830151611d4e565b82516009101561127e576101408301526107e76104db6101a0830151611d4e565b8251600a101561127e576101608301526101c0810151156112b9576108116104db89830151611d4e565b8251600b101561127e57610180830152610200810151156112925761083d6104db610220830151611d4e565b905b8251600c101561127e576102406104db91610861936101a08601520151611c62565b908051600d101561127e5761087f916101c08201525f815191611d84565b604061089861088e8880611772565b602081019061189e565b0135036112135763ffffffff6108b19151925116611b8c565b6001600160801b03806108c38c611735565b1694169384109081156111d9575b5061116e576108ee60806108e86102078780611772565b01611735565b6001600160801b03806109008c611735565b16911611156111045761091c61021761020d6102078780611772565b602081519101209060208151910120036110c05761094863ffffffff6001600160801b03925116611b8c565b16016001600160801b038111611042576001600160801b038061097360806108e86102078780611772565b92169116101561105657600167ffffffffffffffff6109946040840161175d565b16019067ffffffffffffffff82116110425767ffffffffffffffff91826109c360606102766102078680611772565b9116928391161115610fb1575b50506040519260208401946001600160801b036109ec88611749565b16865260248101359586604087015260448201359586606082015260608152610a16608082611685565b51902067ffffffffffffffff610a3160406102768d8c611720565b165f527f5e9f695da54d5993169597b18a19a3159f4a62cbfd7f6669a996ca09c6df170460205260405f20548015610f8957808203610f5b575050610a768988611720565b9160a4820135906102221901811215610ea1570160040190610a9b61088e8280611772565b9067ffffffffffffffff80610ac16060610276610207610aba8861175d565b9680611772565b16911614610ace8261175d565b9015610f3e5750610ade8161175d565b60208201359063ffffffff8216809203610ea15767ffffffffffffffff60405191610b0883611631565b16815260208101918252604080820193013583526101808401359461ffff8616809603610ea157610b3e610140918601866118b3565b906040610b4f6102008901896118b3565b94909882519a7f7d1a8695000000000000000000000000000000000000000000000000000000008c5260048c01526101008160248d01378261010082016101248d0137016101648a01376102406101a4890152816102448901527f07ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff8211610ea157610284908896979594939260051b809161026489013786018261026482016003196102648a850301016101c48a0152520193905f5b818110610f13575050509367ffffffffffffffff8493928160209751166101e486015251166102048401525161022483015203815f73ffffffffffffffffffffffffffffffffffffffff7f0000000000000000000000000000000000000000000000000000000000000000165af1908115610f08575f91610ecd575b5015610ea5575167ffffffffffffffff1691610c9b6116c5565b50610ca68685611720565b604001610cb29061175d565b9060405191610cc08361164d565b84835267ffffffffffffffff166020830152610cdc8786611720565b80610ce691611772565b80610cf091611787565b606001610cfc9061175d565b60405194610d098661164d565b855267ffffffffffffffff166020850152610d248786611720565b80610d2e91611772565b80610d3891611787565b608001610d4490611735565b96610d4f8187611720565b80610d5991611772565b80610d6391611787565b610200013590610d739087611720565b80610d7d91611772565b80610d8791611787565b6101c001359060405198610d9a8a611631565b6001600160801b031689526020890152604088015260405195610dbc87611669565b606013610ea15761014096610e9f95610e7d94610e5c93610de860405193610de385611631565b611749565b8352602083015260408201528752602087019081526040870192835260608701948552610e36604051809851604080916001600160801b038151168452602081015160208501520151910152565b5180516001600160801b03166060880152602081015160808801526040015160a0870152565b5160c085019067ffffffffffffffff60208092828151168552015116910152565b5161010083019067ffffffffffffffff60208092828151168552015116910152565bf35b5f80fd5b7fd611c318000000000000000000000000000000000000000000000000000000005f5260045ffd5b90506020813d602011610f00575b81610ee860209383611685565b81010312610ea157518015158103610ea1575f610c81565b3d9150610edb565b6040513d5f823e3d90fd5b91969550919293602080600192610f298a611891565b15158152019701910191879596949392610c05565b67ffffffffffffffff906390f4dbed60e01b5f521660045260245ffd5b7f6d23e8a3000000000000000000000000000000000000000000000000000000005f5260045260245260445ffd5b7f5b48b457000000000000000000000000000000000000000000000000000000005f5260045ffd5b610fcc60606102766102078467ffffffffffffffff95611772565b1603610fd9575f806109d0565b608460405162461bcd60e51b8152602060048201526024808201527f696e76616c696420626c6f636b3a206e6f6e20696e6372656173696e6720686560448201527f69676874000000000000000000000000000000000000000000000000000000006064820152fd5b634e487b7160e01b5f52601160045260245ffd5b608460405162461bcd60e51b815260206004820152602860248201527f696e76616c696420626c6f636b3a206865616465722069732066726f6d20746860448201527f65206675747572650000000000000000000000000000000000000000000000006064820152fd5b606460405162461bcd60e51b815260206004820152602060248201527f696e76616c696420626c6f636b3a20636861696e2d6964206d69736d617463686044820152fd5b608460405162461bcd60e51b815260206004820152602560248201527f696e76616c696420626c6f636b3a206e6f6e206d6f6e6f746f6e69632062667460448201527f2074696d650000000000000000000000000000000000000000000000000000006064820152fd5b608460405163f492ef2b60e01b815260206004820152603c60248201527f696e76616c696420626c6f636b3a20756e74727573746564207374617465206960448201527f73206f757473696465206f66207472757374696e6720706572696f64000000006064820152fd5b90506001600160801b036111ec8b611735565b168403906001600160801b038211611042576001600160801b03809116911610155f6108d1565b608460405163f492ef2b60e01b815260206004820152602360248201527f696e76616c696420626c6f636b3a206865616465722068617368206d69736d6160448201527f74636800000000000000000000000000000000000000000000000000000000006064820152fd5b634e487b7160e01b5f52603260045260245ffd5b7f6e340b9cffb37a989ca544e6bb780a2c78901d3fb33738768511a30617afa01d9061083f565b7f6e340b9cffb37a989ca544e6bb780a2c78901d3fb33738768511a30617afa01d610811565b7f6e340b9cffb37a989ca544e6bb780a2c78901d3fb33738768511a30617afa01d610764565b7f6e340b9cffb37a989ca544e6bb780a2c78901d3fb33738768511a30617afa01d610739565b7f6e340b9cffb37a989ca544e6bb780a2c78901d3fb33738768511a30617afa01d610710565b61135e6113649284611e64565b83611ea8565b505f80610563565b61138191935060086020860153600185611ea8565b915f610603565b61139182611e45565b60010180600111611042576113ac6105fb9161060f97611907565b9550506105ef565b92506113bf82611e45565b6001018060011161104257926105d6565b67ffffffffffffffff6113e66113649385611e64565b9151169083611ea8565b61141191925067ffffffffffffffff90600860208601535116600184611ea8565b905f610551565b61142190611e45565b600101806001116110425761143c61053d9161056a95611907565b9350610530565b61144e919250611e45565b6001018060011161104257905f610518565b634e487b7160e01b5f52604160045260245ffd5b6114b9906114cb875191516040519384937ff6b6676b000000000000000000000000000000000000000000000000000000008552604060048601526044850190611822565b83810360031901602485015290611822565b0390fd5b67ffffffffffffffff9051166390f4dbed60e01b5f5260045260245ffd5b60019150145f6102be565b634e487b7160e01b5f52602160045260245ffd5b61151e67ffffffffffffffff9261175d565b907f55bace6f000000000000000000000000000000000000000000000000000000005f526004521660245260445ffd5b507f5e9f695da54d5993169597b18a19a3159f4a62cbfd7f6669a996ca09c6df17005f90815290917fa4cc147281017aae47c0db3f306061a3149e6999ca67777149d3e595a1e6b4be5b8183106115b057505090602061014292820101610136565b6020919350806001915483858801015201910190918392611598565b6020925061014294915060ff191682840152151560051b820101610136565b634e487b7160e01b5f52602260045260245ffd5b92607f1692610117565b7f3921c703000000000000000000000000000000000000000000000000000000005f5260045ffd5b6060810190811067ffffffffffffffff82111761146057604052565b6040810190811067ffffffffffffffff82111761146057604052565b6080810190811067ffffffffffffffff82111761146057604052565b90601f8019910116810190811067ffffffffffffffff82111761146057604052565b604051906116b482611631565b5f6040838281528260208201520152565b604051906116d282611669565b816116db6116a7565b81526116e56116a7565b60208201526040516116f68161164d565b5f81525f602082015260408201526060604051916117138361164d565b5f83525f60208401520152565b903590605e1981360301821215610ea1570190565b356001600160801b0381168103610ea15790565b35906001600160801b0382168203610ea157565b3567ffffffffffffffff81168103610ea15790565b903590603e1981360301821215610ea1570190565b9035906102be1981360301821215610ea1570190565b903590601e1981360301821215610ea1570180359067ffffffffffffffff8211610ea157602001918136038313610ea157565b67ffffffffffffffff811161146057601f01601f191660200190565b9291926117f8826117d0565b916118066040519384611685565b829481845281830111610ea1578281602093845f960137010152565b805180835260209291819084018484015e5f828201840152601f01601f1916010190565b359067ffffffffffffffff82168203610ea157565b9190826040910312610ea1576040516118738161164d565b602061188c81839561188481611846565b855201611846565b910152565b35908115158203610ea157565b90359060be1981360301821215610ea1570190565b903590601e1981360301821215610ea1570180359067ffffffffffffffff8211610ea157602001918160051b36038313610ea157565b9190820391821161104257565b90815181101561127e570160200190565b9190820180921161104257565b6040516119208161164d565b606081525f60208201525080518015908115611b80575b50611b58575f19908051805b611ae0575b505f198214611ad2576001820190818311611042577f30000000000000000000000000000000000000000000000000000000000000007fff000000000000000000000000000000000000000000000000000000000000006119a984846118f6565b51161480611abe575b611aae575f5b8151831015611a43576119cb83836118f6565b5160f81c603081108015611a39575b611a275767ffffffffffffffff60ff8192602f19011681600a850216011691168110611a0b576001909201916119b8565b5091505060405190611a1c8261164d565b81525f602082015290565b505091505060405190611a1c8261164d565b50603981116119da565b9150916001811190811591611aa2575b50611a7a5767ffffffffffffffff9060405192611a6f8461164d565b835216602082015290565b7fe131a1dc000000000000000000000000000000000000000000000000000000005f5260045ffd5b602b915010155f611a53565b91505060405190611a1c8261164d565b506002611acc8483516118e9565b116119b2565b6040519150611a1c8261164d565b5f198101818111611042577f2d000000000000000000000000000000000000000000000000000000000000007fff00000000000000000000000000000000000000000000000000000000000000611b3783866118f6565b511614611b4e57508015611042575f190180611943565b92505f9050611948565b7fa4482ffc000000000000000000000000000000000000000000000000000000005f5260045ffd5b6040915010155f611937565b6001600160801b03633b9aca00911602906001600160801b03821691820361104257565b805182101561127e5760209160051b010190565b67ffffffffffffffff81511667ffffffffffffffff835116908181105f14611bee57505050505f90565b1115611bfb575050600290565b602067ffffffffffffffff81819301511692015116908181105f14611c205750505f90565b1115611c2b57600290565b600190565b90611c3a826117d0565b611c476040519182611685565b8281528092611c58601f19916117d0565b0190602036910137565b805115611ca957611c738151611e45565b6001018060011161104257611c906106d7611ca592845190611907565b91600a60208401536107048151600185611ea8565b5090565b50604051611cb8602082611685565b5f80825236602083013790565b80516001810180911161104257611cdb90611c30565b80511561127e5760209181611cf7845f94019284845382611ee1565b50604051918291518091835e8101838152039060025afa15610f08575f5190565b8015611ca957611d2781611e45565b6001018060011161104257611d3e611ca591611c30565b9160086020840153600183611ea8565b611ca560405191611d60606084611685565b602283526040366020850137600a6020840153611d7e600184611e92565b83611f33565b929192611d9182856118e9565b9360018514611e355760015b8060011b9086821015611db05750611d9d565b939495505090611dca611dc38486611907565b8583611d84565b611dde93611dd89195611907565b90611d84565b90604051611ded608082611685565b604181526020810190606036833780511561127e576020935f936001845360218301526041820152604051918291518091835e8101838152039060025afa15610f08575f5190565b50611e41929350611bb0565b5190565b906001915b6080811015611e565750565b60019060071c920191611e4a565b602082601092010153600181018091116110425790565b602082601292010153600181018091116110425790565b6020828192010153600181018091116110425790565b9092919083016020015b6080821015611ec657906001929391530190565b600180916080607f85161781530193019060071c9092611eb2565b908051918215611f03576021602084930191015e600101806001116110425790565b505050600190565b825191928215611f2d5783916020611f2a95818694019201015e611907565b90565b50505090565b8160209193929301015260208101809111611042579056fea164736f6c634300081c000a",
}

// ContractUpdateClientABI is the input ABI used to generate the binding from.
// Deprecated: Use ContractUpdateClientMetaData.ABI instead.
var ContractUpdateClientABI = ContractUpdateClientMetaData.ABI

// ContractUpdateClientBin is the compiled bytecode used for deploying new contracts.
// Deprecated: Use ContractUpdateClientMetaData.Bin instead.
var ContractUpdateClientBin = ContractUpdateClientMetaData.Bin

// DeployContractUpdateClient deploys a new Ethereum contract, binding an instance of ContractUpdateClient to it.
func DeployContractUpdateClient(auth *bind.TransactOpts, backend bind.ContractBackend, signatureVerifier common.Address) (common.Address, *types.Transaction, *ContractUpdateClient, error) {
	parsed, err := ContractUpdateClientMetaData.GetAbi()
	if err != nil {
		return common.Address{}, nil, nil, err
	}
	if parsed == nil {
		return common.Address{}, nil, nil, errors.New("GetABI returned nil")
	}

	address, tx, contract, err := bind.DeployContract(auth, *parsed, common.FromHex(ContractUpdateClientBin), backend, signatureVerifier)
	if err != nil {
		return common.Address{}, nil, nil, err
	}
	return address, tx, &ContractUpdateClient{ContractUpdateClientCaller: ContractUpdateClientCaller{contract: contract}, ContractUpdateClientTransactor: ContractUpdateClientTransactor{contract: contract}, ContractUpdateClientFilterer: ContractUpdateClientFilterer{contract: contract}}, nil
}

// ContractUpdateClient is an auto generated Go binding around an Ethereum contract.
type ContractUpdateClient struct {
	ContractUpdateClientCaller     // Read-only binding to the contract
	ContractUpdateClientTransactor // Write-only binding to the contract
	ContractUpdateClientFilterer   // Log filterer for contract events
}

// ContractUpdateClientCaller is an auto generated read-only Go binding around an Ethereum contract.
type ContractUpdateClientCaller struct {
	contract *bind.BoundContract // Generic contract wrapper for the low level calls
}

// ContractUpdateClientTransactor is an auto generated write-only Go binding around an Ethereum contract.
type ContractUpdateClientTransactor struct {
	contract *bind.BoundContract // Generic contract wrapper for the low level calls
}

// ContractUpdateClientFilterer is an auto generated log filtering Go binding around an Ethereum contract events.
type ContractUpdateClientFilterer struct {
	contract *bind.BoundContract // Generic contract wrapper for the low level calls
}

// ContractUpdateClientSession is an auto generated Go binding around an Ethereum contract,
// with pre-set call and transact options.
type ContractUpdateClientSession struct {
	Contract     *ContractUpdateClient // Generic contract binding to set the session for
	CallOpts     bind.CallOpts         // Call options to use throughout this session
	TransactOpts bind.TransactOpts     // Transaction auth options to use throughout this session
}

// ContractUpdateClientCallerSession is an auto generated read-only Go binding around an Ethereum contract,
// with pre-set call options.
type ContractUpdateClientCallerSession struct {
	Contract *ContractUpdateClientCaller // Generic contract caller binding to set the session for
	CallOpts bind.CallOpts               // Call options to use throughout this session
}

// ContractUpdateClientTransactorSession is an auto generated write-only Go binding around an Ethereum contract,
// with pre-set transact options.
type ContractUpdateClientTransactorSession struct {
	Contract     *ContractUpdateClientTransactor // Generic contract transactor binding to set the session for
	TransactOpts bind.TransactOpts               // Transaction auth options to use throughout this session
}

// ContractUpdateClientRaw is an auto generated low-level Go binding around an Ethereum contract.
type ContractUpdateClientRaw struct {
	Contract *ContractUpdateClient // Generic contract binding to access the raw methods on
}

// ContractUpdateClientCallerRaw is an auto generated low-level read-only Go binding around an Ethereum contract.
type ContractUpdateClientCallerRaw struct {
	Contract *ContractUpdateClientCaller // Generic read-only contract binding to access the raw methods on
}

// ContractUpdateClientTransactorRaw is an auto generated low-level write-only Go binding around an Ethereum contract.
type ContractUpdateClientTransactorRaw struct {
	Contract *ContractUpdateClientTransactor // Generic write-only contract binding to access the raw methods on
}

// NewContractUpdateClient creates a new instance of ContractUpdateClient, bound to a specific deployed contract.
func NewContractUpdateClient(address common.Address, backend bind.ContractBackend) (*ContractUpdateClient, error) {
	contract, err := bindContractUpdateClient(address, backend, backend, backend)
	if err != nil {
		return nil, err
	}
	return &ContractUpdateClient{ContractUpdateClientCaller: ContractUpdateClientCaller{contract: contract}, ContractUpdateClientTransactor: ContractUpdateClientTransactor{contract: contract}, ContractUpdateClientFilterer: ContractUpdateClientFilterer{contract: contract}}, nil
}

// NewContractUpdateClientCaller creates a new read-only instance of ContractUpdateClient, bound to a specific deployed contract.
func NewContractUpdateClientCaller(address common.Address, caller bind.ContractCaller) (*ContractUpdateClientCaller, error) {
	contract, err := bindContractUpdateClient(address, caller, nil, nil)
	if err != nil {
		return nil, err
	}
	return &ContractUpdateClientCaller{contract: contract}, nil
}

// NewContractUpdateClientTransactor creates a new write-only instance of ContractUpdateClient, bound to a specific deployed contract.
func NewContractUpdateClientTransactor(address common.Address, transactor bind.ContractTransactor) (*ContractUpdateClientTransactor, error) {
	contract, err := bindContractUpdateClient(address, nil, transactor, nil)
	if err != nil {
		return nil, err
	}
	return &ContractUpdateClientTransactor{contract: contract}, nil
}

// NewContractUpdateClientFilterer creates a new log filterer instance of ContractUpdateClient, bound to a specific deployed contract.
func NewContractUpdateClientFilterer(address common.Address, filterer bind.ContractFilterer) (*ContractUpdateClientFilterer, error) {
	contract, err := bindContractUpdateClient(address, nil, nil, filterer)
	if err != nil {
		return nil, err
	}
	return &ContractUpdateClientFilterer{contract: contract}, nil
}

// bindContractUpdateClient binds a generic wrapper to an already deployed contract.
func bindContractUpdateClient(address common.Address, caller bind.ContractCaller, transactor bind.ContractTransactor, filterer bind.ContractFilterer) (*bind.BoundContract, error) {
	parsed, err := ContractUpdateClientMetaData.GetAbi()
	if err != nil {
		return nil, err
	}
	return bind.NewBoundContract(address, *parsed, caller, transactor, filterer), nil
}

// Call invokes the (constant) contract method with params as input values and
// sets the output to result. The result type might be a single field for simple
// returns, a slice of interfaces for anonymous returns and a struct for named
// returns.
func (_ContractUpdateClient *ContractUpdateClientRaw) Call(opts *bind.CallOpts, result *[]interface{}, method string, params ...interface{}) error {
	return _ContractUpdateClient.Contract.ContractUpdateClientCaller.contract.Call(opts, result, method, params...)
}

// Transfer initiates a plain transaction to move funds to the contract, calling
// its default method if one is available.
func (_ContractUpdateClient *ContractUpdateClientRaw) Transfer(opts *bind.TransactOpts) (*types.Transaction, error) {
	return _ContractUpdateClient.Contract.ContractUpdateClientTransactor.contract.Transfer(opts)
}

// Transact invokes the (paid) contract method with params as input values.
func (_ContractUpdateClient *ContractUpdateClientRaw) Transact(opts *bind.TransactOpts, method string, params ...interface{}) (*types.Transaction, error) {
	return _ContractUpdateClient.Contract.ContractUpdateClientTransactor.contract.Transact(opts, method, params...)
}

// Call invokes the (constant) contract method with params as input values and
// sets the output to result. The result type might be a single field for simple
// returns, a slice of interfaces for anonymous returns and a struct for named
// returns.
func (_ContractUpdateClient *ContractUpdateClientCallerRaw) Call(opts *bind.CallOpts, result *[]interface{}, method string, params ...interface{}) error {
	return _ContractUpdateClient.Contract.contract.Call(opts, result, method, params...)
}

// Transfer initiates a plain transaction to move funds to the contract, calling
// its default method if one is available.
func (_ContractUpdateClient *ContractUpdateClientTransactorRaw) Transfer(opts *bind.TransactOpts) (*types.Transaction, error) {
	return _ContractUpdateClient.Contract.contract.Transfer(opts)
}

// Transact invokes the (paid) contract method with params as input values.
func (_ContractUpdateClient *ContractUpdateClientTransactorRaw) Transact(opts *bind.TransactOpts, method string, params ...interface{}) (*types.Transaction, error) {
	return _ContractUpdateClient.Contract.contract.Transact(opts, method, params...)
}

// VerifyHeader is a paid mutator transaction binding the contract method 0x87876726.
//
// Solidity: function verifyHeader(((uint128,bytes32,bytes32),((((uint64,uint64),string,uint64,uint128,bool,(bytes32,(uint32,bytes32)),bool,bytes32,bool,bytes32,bytes32,bytes32,bytes32,bytes32,bool,bytes32,bool,bytes32,bytes),(uint64,uint32,(bytes32,(uint32,bytes32)),(uint8,bytes20)[])),(uint64,uint64)),uint128,(uint256[8],uint256[2],uint256[2],uint16,uint32[],uint32[],bytes32[],bool[])) msg_) returns(((uint128,bytes32,bytes32),(uint128,bytes32,bytes32),(uint64,uint64),(uint64,uint64)))
func (_ContractUpdateClient *ContractUpdateClientTransactor) VerifyHeader(opts *bind.TransactOpts, msg_ ISpectreClientMsgsMsgUpdateApplicationState) (*types.Transaction, error) {
	return _ContractUpdateClient.contract.Transact(opts, "verifyHeader", msg_)
}

// VerifyHeader is a paid mutator transaction binding the contract method 0x87876726.
//
// Solidity: function verifyHeader(((uint128,bytes32,bytes32),((((uint64,uint64),string,uint64,uint128,bool,(bytes32,(uint32,bytes32)),bool,bytes32,bool,bytes32,bytes32,bytes32,bytes32,bytes32,bool,bytes32,bool,bytes32,bytes),(uint64,uint32,(bytes32,(uint32,bytes32)),(uint8,bytes20)[])),(uint64,uint64)),uint128,(uint256[8],uint256[2],uint256[2],uint16,uint32[],uint32[],bytes32[],bool[])) msg_) returns(((uint128,bytes32,bytes32),(uint128,bytes32,bytes32),(uint64,uint64),(uint64,uint64)))
func (_ContractUpdateClient *ContractUpdateClientSession) VerifyHeader(msg_ ISpectreClientMsgsMsgUpdateApplicationState) (*types.Transaction, error) {
	return _ContractUpdateClient.Contract.VerifyHeader(&_ContractUpdateClient.TransactOpts, msg_)
}

// VerifyHeader is a paid mutator transaction binding the contract method 0x87876726.
//
// Solidity: function verifyHeader(((uint128,bytes32,bytes32),((((uint64,uint64),string,uint64,uint128,bool,(bytes32,(uint32,bytes32)),bool,bytes32,bool,bytes32,bytes32,bytes32,bytes32,bytes32,bool,bytes32,bool,bytes32,bytes),(uint64,uint32,(bytes32,(uint32,bytes32)),(uint8,bytes20)[])),(uint64,uint64)),uint128,(uint256[8],uint256[2],uint256[2],uint16,uint32[],uint32[],bytes32[],bool[])) msg_) returns(((uint128,bytes32,bytes32),(uint128,bytes32,bytes32),(uint64,uint64),(uint64,uint64)))
func (_ContractUpdateClient *ContractUpdateClientTransactorSession) VerifyHeader(msg_ ISpectreClientMsgsMsgUpdateApplicationState) (*types.Transaction, error) {
	return _ContractUpdateClient.Contract.VerifyHeader(&_ContractUpdateClient.TransactOpts, msg_)
}
