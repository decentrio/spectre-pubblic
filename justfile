set dotenv-load

# Nothing in this repository links gnark or libgaraga_rs any more: the prover is its own
# repository and process (spectre-prover), started by scripts/local/run_prover.sh.

# Default task lists all available tasks
default:
  just --list

# Build the contracts using `forge build`
[group('build')]
build-contracts: clean-foundry
	forge build

# Build the relayer using `cargo build`
[group('build')]
build-relayer:
	cargo build --bin relayer --release --locked

# Build the relayer using `go build`
[group('build')]
build-go-relayer:
	cd relayer && go build ./...

# Build the standalone Arbitrum attestor sidecar.
[group('build')]
build-arbitrum-attestor:
	mkdir -p attestor/arbitrum/bin
	cd attestor/arbitrum && go build -o bin/attestor ./cmd

# Build the Optimism attestor sidecar binary
[group('build')]
build-attestor:
	cd attestor/optimism && go build -o attestor ./cmd

# Build and optimize the eth wasm light client using `cosmwasm/optimizer`. Requires `docker` and `gzip`
#
# RUSTUP_TOOLCHAIN pins the image's own Rust 1.86.0. Without it rustup inside the container follows
# rust-toolchain.toml and downloads that newer Rust, which breaks the optimizer two ways. On 1.97.1 the
# link fails with `undefined symbol: db_read` etc.: Rust 1.96 stopped passing --allow-undefined and the
# optimizer sets RUSTFLAGS, which drops the flag in .cargo/config.toml. On 1.95.0 the link passes but the
# image's wasm-opt rejects the bulk-memory instructions with "Bulk memory operations require bulk memory".
[group('build')]
build-cw-ics08-wasm-eth:
	docker run --rm -e RUSTUP_TOOLCHAIN=1.86.0-x86_64-unknown-linux-musl -v "$(pwd)":/code --mount type=volume,source="$(basename "$(pwd)")_cache",target=/target --mount type=volume,source=registry_cache,target=/usr/local/cargo/registry cosmwasm/optimizer:0.17.0@sha256:7e0b9229c1a4118d0c9a2af2e7f5d95a91f264c26a2ce5681c779926e74d7f85 ./programs/cw-ics08-wasm-eth
	cp artifacts/cw_ics08_wasm_eth.wasm e2e/interchaintestv8/wasm
	gzip -n e2e/interchaintestv8/wasm/cw_ics08_wasm_eth.wasm -f

# Build and optimize the L2 Wasm light clients. Each recipe emits a distinct artifact.
[group('build')]
build-cw-ics08-wasm-arbitrum:
	docker run --rm -v "$(pwd)":/code --mount type=volume,source="$(basename "$(pwd)")_cache",target=/target --mount type=volume,source=registry_cache,target=/usr/local/cargo/registry cosmwasm/optimizer:0.17.0@sha256:7e0b9229c1a4118d0c9a2af2e7f5d95a91f264c26a2ce5681c779926e74d7f85 ./programs/cw-ics08-wasm-arbitrum
	cp artifacts/cw_ics08_wasm_arbitrum.wasm e2e/interchaintestv8/wasm
	gzip -n e2e/interchaintestv8/wasm/cw_ics08_wasm_arbitrum.wasm -f

[group('build')]
build-cw-ics08-wasm-base:
	docker run --rm -v "$(pwd)":/code --mount type=volume,source="$(basename "$(pwd)")_cache",target=/target --mount type=volume,source=registry_cache,target=/usr/local/cargo/registry cosmwasm/optimizer:0.17.0@sha256:7e0b9229c1a4118d0c9a2af2e7f5d95a91f264c26a2ce5681c779926e74d7f85 ./programs/cw-ics08-wasm-base
	cp artifacts/cw_ics08_wasm_base.wasm e2e/interchaintestv8/wasm
	gzip -n e2e/interchaintestv8/wasm/cw_ics08_wasm_base.wasm -f

[group('build')]
build-cw-ics08-wasm-op:
	docker run --rm -v "$(pwd)":/code --mount type=volume,source="$(basename "$(pwd)")_cache",target=/target --mount type=volume,source=registry_cache,target=/usr/local/cargo/registry cosmwasm/optimizer:0.17.0@sha256:7e0b9229c1a4118d0c9a2af2e7f5d95a91f264c26a2ce5681c779926e74d7f85 ./programs/cw-ics08-wasm-op
	cp artifacts/cw_ics08_wasm_op.wasm e2e/interchaintestv8/wasm
	gzip -n e2e/interchaintestv8/wasm/cw_ics08_wasm_op.wasm -f

[group('build')]
build-cw-ics08-wasm-l2-native:
	cargo build --target wasm32-unknown-unknown --release --package cw-ics08-wasm-arbitrum --package cw-ics08-wasm-base --package cw-ics08-wasm-op

# Capture fixed-block L1/L2 proof evidence. The configuration is local-only because it contains RPC URLs.
# The resulting fixture must be reviewed before it is added as a supported network configuration.
[group('generate')]
capture-l2-fixture config out:
	cd relayer && go run ./l2fixtures/cmd -config {{config}} -out ../{{out}}

# Install the Go relayer for use in the e2e tests.
# Builds with an explicit -o name because the package dir is `cmd/`, so a plain
# `go install ./cmd/...` would land at $GOPATH/bin/cmd, not $GOPATH/bin/relayer.
[group('install')]
install-go-relayer:
	cd relayer && go build -o $(go env GOPATH)/bin/relayer ./cmd

# Generate the complete configured prover artifact set only when any artifact is missing.
# Run `scripts/solidity/build-prover-artifacts.sh --force` for an intentional randomized rebuild.
[group('build')]
build-prover-artifacts:
	scripts/solidity/build-prover-artifacts.sh

# Run all linters
[group('lint')]
lint:
	@echo "Running all linters..."
	just lint-solidity
	just lint-go
	just lint-buf
	just lint-rust

# Lint the Solidity code using `forge fmt` and `bun:solhint`
[group('lint')]
lint-solidity:
	@echo "Linting the Solidity code..."
	scripts/check-solidity-format.sh
	bun solhint -w 0 '{scripts,contracts,test}/**/*.sol'
	natlint run --include 'contracts/**/*.sol'

# Lint the Go code using `golangci-lint`
[group('lint')]
lint-go:
	@echo "Linting the Go code..."
	cd relayer && golangci-lint run
	cd attestor/arbitrum && golangci-lint run
	cd attestor/optimism && golangci-lint run
	cd e2e/interchaintestv8 && golangci-lint run
	cd packages/go-abigen && golangci-lint run

# Lint the Protobuf files using `buf lint`
[group('lint')]
lint-buf:
	@echo "Linting the Protobuf files..."
	buf lint

# Lint the Rust code using `cargo fmt` and `cargo clippy`
[group('lint')]
lint-rust:
	@echo "Linting the Rust code..."
	cargo fmt --all -- --check
	cargo clippy --all-targets --all-features -- -D warnings


# Generate the (non-bytecode) ABI files for the contracts
[group('generate')]
generate-abi: build-contracts
	scripts/solidity/generate-bindings.sh shared

# Generate the ABI files with bytecode for the required contracts (only SpectreClient)
[group('generate')]
generate-abi-bytecode: build-contracts
	scripts/solidity/generate-bindings.sh bytecode

# Generate the fixtures for the wasm tests using the e2e tests
[group('generate')]
generate-fixtures-wasm: clean-foundry install-go-relayer build-prover-artifacts
	@echo "Generating fixtures... This may take a while."
	@echo "Generating recvPacket and acknowledgePacket groth16 fixtures..."
	scripts/local/run_prover.sh -- bash -c "cd e2e/interchaintestv8 && ETH_TESTNET_TYPE=pos GENERATE_WASM_FIXTURES=true E2E_PROOF_TYPE=groth16 go test -v -run '^TestWithEthCosmosTestSuite/Test_ICS20TransferERC20TokenfromEthereumToCosmosAndBack\$' -timeout 60m"
	@echo "Generating native SdkCoin recvPacket groth16 fixtures..."
	scripts/local/run_prover.sh -- bash -c "cd e2e/interchaintestv8 && ETH_TESTNET_TYPE=pos GENERATE_WASM_FIXTURES=true E2E_PROOF_TYPE=groth16 go test -v -run '^TestWithEthCosmosTestSuite/Test_ICS20TransferNativeCosmosCoinsToEthereumAndBack\$' -timeout 60m"
	@echo "Generating timeoutPacket groth16 fixtures..."
	scripts/local/run_prover.sh -- bash -c "cd e2e/interchaintestv8 && ETH_TESTNET_TYPE=pos GENERATE_WASM_FIXTURES=true E2E_PROOF_TYPE=groth16 go test -v -run '^TestWithEthCosmosTestSuite/Test_TimeoutPacketFromCosmos\$' -timeout 60m"

# Generate go types for the e2e tests from the ethereum light client code
[group('generate')]
generate-ethereum-types:
	cargo run --bin generate_json_schema --features test-utils
	bun quicktype --src-lang schema --lang go --just-types-and-package --package ethereum --src ethereum_types_schema.json --out e2e/interchaintestv8/types/ethereum/types.gen.go --top-level GeneratedTypes
	rm ethereum_types_schema.json
	sed -i.bak 's/int64/uint64/g' e2e/interchaintestv8/types/ethereum/types.gen.go # quicktype generates int64 instead of uint64 :(
	rm -f e2e/interchaintestv8/types/ethereum/types.gen.go.bak # this is to be linux and mac compatible (coming from the sed command)
	cd e2e/interchaintestv8 && golangci-lint run --fix types/ethereum/types.gen.go

# Generate the attestor sidecar gRPC bindings from proto/attestor using `buf generate`.
[group('generate')]
generate-buf:
    @echo "Generating Protobuf files for the attestor"
    buf generate --template buf.gen.attestor.yaml

shadowfork := if env("ETH_RPC_URL", "") == "" { "--no-match-path test/shadowfork/*" } else { "" }

# Run all the foundry tests
[group('test')]
test-foundry testname=".\\*":
	forge test -vvv --show-progress --fuzz-runs 5000 --match-test ^{{testname}}\(.\*\)\$ {{shadowfork}}
	@ {{ if shadowfork == "" { "" } else { 'echo ' + BOLD + YELLOW + 'Ran without shadowfork tests since ETH_RPC_URL was not set' } }}

# Run the benchmark tests
[group('test')]
test-benchmark testname=".\\*":
	forge test -vvv --show-progress --gas-report --match-path 'test/**/*GasTest.t.sol' --match-test {{testname}}

# Run the cargo tests
[group('test')]
test-cargo testname="--all":
	cargo test {{testname}} --locked --no-fail-fast -- --nocapture

# Run the complete optimized-Wasm release gate locally. This intentionally stays
# independent of GitHub Actions while automated workflows are disabled.
[group('test')]
validate-wasm-release:
	VALIDATE_OPTIMIZED=1 scripts/validate-l2-clients.sh

# Run the tests in abigen
[group('test')]
test-abigen:
	@echo "Running abigen tests..."
	cd packages/go-abigen && go test -v ./...

# Run Go relayer tests
[group('test')]
test-go-relayer:
	@echo "Running Go relayer tests..."
	cd relayer && go test -v ./...

# Run the standalone Arbitrum attestor tests.
[group('test')]
test-arbitrum-attestor:
	@echo "Running Arbitrum attestor tests..."
	cd attestor/arbitrum && go test -v ./...

# Run the Optimism attestor sidecar tests (own module, no native deps)
[group('test')]
test-attestor:
	@echo "Running Optimism attestor tests..."
	cd attestor/optimism && go test -v ./...

# Run the non-destructive live attestor suite against an already-running local
# sidecar. route is one of op, base, or arbitrum; the helper sources its
# attestor.env and deliberately does not start/stop or reconfigure the stack.
[group('test')]
test-attestor-e2e route="op":
	scripts/local/test_attestor_e2e.sh {{route}}

# Run the long-running attestor feed-lifecycle suite against an ISOLATED route:
# the helper starts its own attestor (temporary state, separate ports) beside
# the running devnet and removes it afterwards. phase is provisional,
# finalized, or all.
[group('test')]
test-attestor-lifecycle-e2e route="op" phase="all":
	scripts/local/test_attestor_lifecycle_e2e.sh {{route}} {{phase}}

# Run the attestor fault-recovery suite: disposable attestors behind TCP fault
# proxies (replica, L1 RPC, WebSocket outages; SIGTERM/SIGKILL restarts;
# replica lag). The devnet itself is never stopped.
[group('test')]
test-attestor-fault-e2e route="op":
	scripts/local/test_attestor_fault_e2e.sh {{route}}

# Run the attestor divergence drills (#533) on isolated routes: an invalid
# on-chain OP game (permanent on L1), and provisional corrections driven by a
# replica-RPC interposer. route is op or arbitrum; drill is invalid-game (op),
# correction, or all.
[group('test')]
test-attestor-divergence-e2e route="op" drill="all":
	scripts/local/test_attestor_divergence_e2e.sh {{route}} {{drill}}

# Run any e2e test using the test's full name. For example, `just test-e2e TestWithEthCosmosTestSuite/Test_Deploy`
#
# ETH_TESTNET_TYPE=pos picks the Kurtosis PoS network (with beacon API), which is what
# the wasm light client + the cosmos→eth direction need. Override to "pow" only if the
# test explicitly targets the anvil/PoW path.
[group('test')]
test-e2e testname: clean-foundry install-go-relayer build-prover-artifacts
	@echo "Running {{testname}} test..."
	scripts/local/run_prover.sh -- bash -c 'cd e2e/interchaintestv8 && \
		RELAYER_BINARY="$(go env GOPATH)/bin/relayer" \
		ETH_TESTNET_TYPE=pos \
		E2E_PROOF_TYPE="${E2E_PROOF_TYPE:-groth16}" \
		go test -v -run "^{{testname}}\$" -timeout 120m'

# Run the Cosmos -> L2 pause/expire/restart timeout test against a running OP or Base devnet.
# For example, `just test-l2-timeout relayer/config.json`. The sender defaults to the devnet's
# `test2` key (it must not be the relayer's COSMOS_PRIVATE_KEY); see docs/E2E.md.
[group('test')]
test-l2-timeout config:
	@echo "Running the L2 timeout lifecycle test against {{config}}..."
	. ./scripts/local/gaiad_binary.sh && \
	L2_TIMEOUT_SENDER_KEY="${L2_TIMEOUT_SENDER_KEY:-$("$GAIAD" keys export test2 --unarmored-hex --unsafe --keyring-backend test --home "$HOME/.gaia" -y)}" \
	L2_TIMEOUT_CONFIG="$(cd "$(dirname "{{config}}")" && pwd)/$(basename "{{config}}")" \
		bash -c 'cd relayer && go test -tags l2devnet -race -count=1 -timeout 40m -run TestL2TimeoutLifecycle -v ./cmd'

# Run any e2e test in the EthCosmosTestSuite. For example, `just test-e2e-eth-cosmos Test_Deploy`
[group('test')]
test-e2e-eth-cosmos testname:
	@echo "Running {{testname}} test..."
	just test-e2e TestWithEthCosmosTestSuite/{{testname}}

# Run any e2e test in the RelayerTestSuite. For example, `just test-e2e-relayer Test_5_RecvPacketToEth`
[group('test')]
test-e2e-relayer testname:
	@echo "Running {{testname}} test..."
	just test-e2e TestWithRelayerTestSuite/{{testname}}

# Run any e2e test in the MultichainTestSuite. For example, `just test-e2e-multichain Test_Deploy`
[group('test')]
test-e2e-multichain testname:
	@echo "Running {{testname}} test..."
	just test-e2e TestWithMultichainTestSuite/{{testname}}

# Clean up the foundry cache and out directories
[group('clean')]
clean-foundry:
	@echo "Cleaning up cache and out directories"
	-rm -rf cache out broadcast # ignore errors

# Clean up the cargo artifacts using `cargo clean`
[group('clean')]
clean-cargo:
	@echo "Cleaning up cargo target directory"
	cargo clean

# Run Slither static analysis on contracts
[group('security')]
slither:
	@echo "Running Slither static analysis..."
	slither . --config-file .slither.config.json
