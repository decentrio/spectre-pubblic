#!/usr/bin/env bash

# Shared signing identity for the repository's disposable OP, Base and Arbitrum
# devnets. The seed was generated from a cryptographically secure RNG. It is
# intentionally checked in so a fresh local stack and its sample client profile
# agree without an operator having to create a key first. It is public test data:
# never reuse either value outside a local devnet.
#
# A caller may supply a different identity, but must set both values together so
# the handoff's ATTESTOR_PUBLIC_KEY remains the public half of the signing seed.
if [ "${ATTESTOR_SIGNING_KEY+x}" = x ] || [ "${ATTESTOR_PUBLIC_KEY+x}" = x ]; then
    : "${ATTESTOR_SIGNING_KEY:?ATTESTOR_SIGNING_KEY and ATTESTOR_PUBLIC_KEY must be set together}"
    : "${ATTESTOR_PUBLIC_KEY:?ATTESTOR_SIGNING_KEY and ATTESTOR_PUBLIC_KEY must be set together}"
else
    ATTESTOR_SIGNING_KEY=fac8c6533c38c5670c6eadac6cd5d5722a24d8e9eb5705111950f1e849fe15fb
    ATTESTOR_PUBLIC_KEY=0x5d9fbbeaa3960a8febd75dd7e7aa405edd218b7019a58f4b538e27178bc23268
fi
