// Package arbitrum tracks configured Nitro endpoints and exposes Arbitrum state
// roots through gRPC. Nitro owns persistent execution state; the attestor
// persists only its finalized-L1 assertion cursor, verified frontier, and
// mismatch metadata.
package arbitrum
