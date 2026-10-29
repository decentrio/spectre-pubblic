// Package rpcmock provides JSON-RPC stub servers for the two chains the relayer
// talks to, so tests can drive real client code without a devnet.
//
// The seam it exploits is that both clients are built from a URL:
// ethclient.Dial and rpchttp.New take an address, so pointing them at an
// httptest.Server yields a REAL *ethclient.Client / *rpchttp.HTTP that runs the
// real encoding, batching and error-decoding paths — only the far end is fake.
// Nothing in production has to become an interface for this to work, which
// matters here because services.CosmosEndpoint and services.EVMEndpoint hold
// concrete client pointers by design.
//
// This pattern was already used ad hoc in transaction/, client/, services/,
// subscriber/, cmd/ and chain/l2rollup, each with its own private copy. This
// package is the shared version, so a stub written once is reusable by any
// package that needs one.
//
// What it does NOT do: verify that a real chain answers the way the stub does.
// A misunderstanding of a wire format is reproduced faithfully by the stub and
// the test stays green. That gap is what the interchaintest e2e suite covers;
// the two are complementary, not alternatives.
//
// This package imports NOTHING from the relayer, deliberately. services.CosmosEndpoint
// is the obvious return type for a stub, but services imports client, so a
// services-typed helper here would make client's own tests an import cycle. The
// stubs return the raw clients and each caller wraps them in one line.
//
// Only _test.go files import this package, so it is never linked into the
// relayer binary despite importing testing.
package rpcmock
