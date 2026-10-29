FROM node:20-trixie-slim

ARG NITRO_CONTRACTS_REF=v3.1.0

RUN apt-get update \
    && apt-get install -y --no-install-recommends \
        ca-certificates \
        curl \
        git \
        jq \
        make \
        gcc \
        g++ \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /workspace
RUN git clone --branch "${NITRO_CONTRACTS_REF}" --depth 1 \
        https://github.com/OffchainLabs/nitro-contracts.git . \
    && git submodule update --init --recursive --depth 1 \
    && yarn install \
    && yarn cache clean

RUN curl -L https://foundry.paradigm.xyz | bash \
    && /root/.foundry/bin/foundryup --install 1.0.0

ENV PATH="${PATH}:/root/.foundry/bin"

RUN touch scripts/config.ts && yarn build:all

ENTRYPOINT ["yarn"]
