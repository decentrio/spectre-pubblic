// Package spectrepb holds the Go types of the spectre Ethereum light client
// messages (ibc.lightclients.spectre.v1) that a Cosmos chain hosts as a native
// ibc-go light client module, as opposed to a CosmWasm contract.
//
// spectre.proto is vendored from the atomone module, which is the source of
// truth; see its header. The Go code is generated with protoc and
// protoc-gen-gocosmos from the gogoproto version pinned in go.mod and is
// committed, never edited by hand:
//
//	go install github.com/cosmos/gogoproto/protoc-gen-gocosmos@v1.7.0
//	go generate ./chain/ethnative/spectrepb
package spectrepb

//go:generate ./generate.sh
