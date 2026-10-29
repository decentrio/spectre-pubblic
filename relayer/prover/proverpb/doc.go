// Package proverpb holds the Go types and gRPC bindings of the prover service
// (prover.v1.ProverService): the async Groth16 batch-proof API the relayer calls
// instead of proving in-process.
//
// prover.proto is vendored from the spectre-prover repository, which is the source
// of truth; see its header. The Go code is generated with protoc, protoc-gen-go
// v1.36.11 and protoc-gen-go-grpc v1.5.1 (the versions of the attestor stubs) and is
// committed, never edited by hand:
//
//	go generate ./prover/proverpb
package proverpb

//go:generate ./generate.sh
