"""
Arbitrum Nitro BoLD devnet layered onto an already-running Kurtosis Ethereum L1.

The package deliberately does not deploy Ethereum. The caller supplies the
Kurtosis-internal execution/beacon endpoints and the funded L1 development key.
It deploys RollupCore, starts one simple Nitro sequencer/batch-poster/staker,
and exports the exact chain/config artifacts used by the independent attestor.
"""

DEFAULTS = {
    "chain_name": "arbdev",
    "l1_chain_id": 3151908,
    "l1_rpc_url": "http://el-1-geth-lighthouse:8545",
    "l1_beacon_url": "http://cl-1-lighthouse-geth:4000",
    "l2_chain_id": 412346,
    "nitro_image": "offchainlabs/nitro-node:v3.11.2-3599aca",
    "nitro_contracts_ref": "v3.1.0",
    "funder_private_key": "eaba42282ad33c8ef2524f07277c03a776d98ae19f581990ce75becb7cfa1c23",
    "owner_private_key": "dc04c5399f82306ec4b4d654a342f40e2e0620fe39950d967e1e574b32d4dd36",
    "owner_address": "0x5E1497dD1f08C87b2d8FE23e9AAB6c1De833D927",
    "sequencer_private_key": "cb5790da63720727af975f42c79f69918580209889225fa7128c92402a6d3a65",
    "sequencer_address": "0xe2148eE53c0755215Df69b2616E552154EdC584f",
    "validator_private_key": "182fecf15bdf909556a0f617a63e05ab22f1493d25a9f1e27c228266c772a890",
    "validator_address": "0x6A568afe0f82d34759347bb36F14A6bB171d2CBe",
    "assertion_posting_interval": "10s",
    "assertion_confirming_interval": "10s",
}

def _config(args):
    supplied = args.get("arbitrum_package", args)
    config = dict(DEFAULTS)
    for key, value in supplied.items():
        if key not in config:
            fail("unknown arbitrum_package field '{}'".format(key))
        config[key] = value

    for field in [
        "chain_name",
        "l1_rpc_url",
        "l1_beacon_url",
        "nitro_image",
        "nitro_contracts_ref",
        "funder_private_key",
        "owner_private_key",
        "owner_address",
        "sequencer_private_key",
        "sequencer_address",
        "validator_private_key",
        "validator_address",
    ]:
        if not config[field]:
            fail("arbitrum_package.{} is required".format(field))
    if config["l1_chain_id"] <= 0 or config["l2_chain_id"] <= 0:
        fail("L1 and L2 chain IDs must be positive")
    if config["l1_chain_id"] == config["l2_chain_id"]:
        fail("L1 and L2 chain IDs must differ")
    return config

def _l2_chain_config(config):
    return {
        "chainId": config["l2_chain_id"],
        "homesteadBlock": 0,
        "daoForkSupport": True,
        "eip150Block": 0,
        "eip150Hash": "0x0000000000000000000000000000000000000000000000000000000000000000",
        "eip155Block": 0,
        "eip158Block": 0,
        "byzantiumBlock": 0,
        "constantinopleBlock": 0,
        "petersburgBlock": 0,
        "istanbulBlock": 0,
        "muirGlacierBlock": 0,
        "berlinBlock": 0,
        "londonBlock": 0,
        "clique": {
            "period": 0,
            "epoch": 0,
        },
        "arbitrum": {
            "EnableArbOS": True,
            "AllowDebugPrecompiles": True,
            "DataAvailabilityCommittee": False,
            "InitialArbOSVersion": 40,
            "InitialChainOwner": config["owner_address"],
            "GenesisBlockNum": 0,
        },
    }

def _sequencer_config(config):
    return {
        "ensure-rollup-deployment": False,
        "parent-chain": {
            "connection": {
                "url": config["l1_rpc_url"],
            },
            "blob-client": {
                "beacon-url": config["l1_beacon_url"],
            },
        },
        "chain": {
            "id": config["l2_chain_id"],
            "info-files": ["/config/chain_info.json"],
        },
        "node": {
            "bold": {
                "rpc-block-number": "latest",
                "assertion-posting-interval": config["assertion_posting_interval"],
                "assertion-scanning-interval": config["assertion_posting_interval"],
                "assertion-confirming-interval": config["assertion_confirming_interval"],
                "minimum-gap-to-parent-assertion": config["assertion_posting_interval"],
                "parent-chain-block-time": 2,
            },
            "staker": {
                "enable": True,
                "strategy": "MakeNodes",
                "staker-interval": "10s",
                "make-assertion-interval": config["assertion_posting_interval"],
                "disable-challenge": False,
                "use-smart-contract-wallet": True,
                "dangerous": {
                    "without-block-validator": True,
                },
                "parent-chain-wallet": {
                    "private-key": config["validator_private_key"],
                },
            },
            "sequencer": True,
            "dangerous": {
                "no-sequencer-coordinator": True,
                "disable-blob-reader": False,
            },
            "delayed-sequencer": {
                "enable": True,
            },
            "batch-poster": {
                "enable": True,
                "max-delay": "30s",
                "l1-block-bound": "ignore",
                "parent-chain-wallet": {
                    "private-key": config["sequencer_private_key"],
                },
                "data-poster": {
                    "wait-for-l1-finality": False,
                },
            },
            "feed": {
                "output": {
                    "enable": True,
                    "port": 9642,
                },
            },
        },
        "execution": {
            "sequencer": {
                "enable": True,
            },
            "forwarding-target": "",
        },
        "persistent": {
            "chain": "/home/user/.arbitrum/arbdev/nitro",
        },
        "http": {
            "addr": "0.0.0.0",
            "port": 8547,
            "vhosts": "*",
            "corsdomain": "*",
            "api": ["net", "web3", "eth", "debug", "txpool"],
        },
        "ws": {
            "addr": "0.0.0.0",
            "port": 8548,
            "origins": "*",
            "api": ["net", "web3", "eth", "debug", "txpool"],
        },
        "validation": {
            "wasm": {
                "allowed-wasm-module-roots": [
                    "/home/user/nitro-legacy/machines",
                    "/home/user/target/machines",
                ],
            },
        },
    }

def run(plan, args = {}):
    config = _config(args)
    plan.print("Attaching Arbitrum Nitro {} to existing L1 chain {}".format(
        config["l2_chain_id"],
        config["l1_chain_id"],
    ))

    wasm_root = plan.run_sh(
        image = config["nitro_image"],
        run = "cat /home/user/target/machines/latest/module-root.txt | tr -d '\\n'",
    ).output.strip()
    if not wasm_root:
        fail("could not read the Nitro validation-machine module root")

    input_artifact = plan.render_templates(
        name = "arb-rollup-inputs",
        config = {
            "l2_chain_config.json": struct(
                template = json.encode(_l2_chain_config(config)),
                data = {},
            ),
            "sequencer_config.base.json": struct(
                template = json.encode(_sequencer_config(config)),
                data = {},
            ),
        },
    )

    deploy_script = """
set -eu
mkdir -p /config
cp /inputs/l2_chain_config.json /config/l2_chain_config.json

fund() {
    address="$1"
    cast send "$address" \
        --rpc-url "$PARENT_CHAIN_RPC" \
        --private-key "$FUNDER_PRIVKEY" \
        --legacy \
        --gas-price 2gwei \
        --value 1000ether >/dev/null
}

echo "Funding Arbitrum owner, sequencer, and validator on the shared L1"
fund "$OWNER_ADDRESS"
fund "$SEQUENCER_ADDRESS"
fund "$VALIDATOR_ADDRESS"

echo "Deploying BoLD RollupCore contracts"
yarn create-rollup-testnode
jq '[.[]]' /config/deployed_chain_info.json > /config/chain_info.json
jq --rawfile chain_info /config/chain_info.json \
    '.chain |= (del(."info-files") + {"info-json": $chain_info})' \
    /inputs/sequencer_config.base.json > /config/sequencer_config.json
echo "Arbitrum deployment artifacts are ready"
tail -f /dev/null
"""

    deployer = plan.add_service(
        name = "arb-rollup-deployer",
        config = ServiceConfig(
            image = ImageBuildSpec(
                image_name = "spectre-arb-rollupcreator",
                build_context_dir = "./rollupcreator",
                build_args = {
                    "NITRO_CONTRACTS_REF": config["nitro_contracts_ref"],
                },
            ),
            entrypoint = ["/bin/sh", "-c"],
            cmd = [deploy_script],
            files = {
                "/inputs": input_artifact,
            },
            env_vars = {
                "PARENT_CHAIN_RPC": config["l1_rpc_url"],
                "PARENT_CHAIN_ID": str(config["l1_chain_id"]),
                "FUNDER_PRIVKEY": config["funder_private_key"],
                "DEPLOYER_PRIVKEY": config["owner_private_key"],
                "CHILD_CHAIN_NAME": config["chain_name"],
                "MAX_DATA_SIZE": "117964",
                "OWNER_ADDRESS": config["owner_address"],
                "SEQUENCER_ADDRESS": config["sequencer_address"],
                "VALIDATOR_ADDRESS": config["validator_address"],
                "AUTHORIZE_VALIDATORS": "10",
                "CHILD_CHAIN_CONFIG_PATH": "/config/l2_chain_config.json",
                "CHAIN_DEPLOYMENT_INFO": "/config/deployment.json",
                "CHILD_CHAIN_INFO": "/config/deployed_chain_info.json",
                "WASM_MODULE_ROOT": wasm_root,
            },
        ),
    )

    plan.wait(
        service_name = deployer.name,
        recipe = ExecRecipe(command = ["test", "-s", "/config/sequencer_config.json"]),
        field = "code",
        assertion = "==",
        target_value = 0,
        timeout = "15m",
        interval = "5s",
    )

    deployment_artifact = plan.store_service_files(
        service_name = deployer.name,
        src = "/config/deployment.json",
        name = "arb-deployment-info",
    )
    chain_info_artifact = plan.store_service_files(
        service_name = deployer.name,
        src = "/config/chain_info.json",
        name = "arb-chain-info",
    )
    sequencer_config_artifact = plan.store_service_files(
        service_name = deployer.name,
        src = "/config/sequencer_config.json",
        name = "arb-sequencer-config",
    )

    rollup_address = plan.exec(
        service_name = deployer.name,
        recipe = ExecRecipe(
            command = [
                "sh",
                "-c",
                "jq -r '.[0].rollup.rollup' /config/chain_info.json | tr -d '\\r\\n'",
            ],
        ),
    )["output"].strip()
    if not rollup_address:
        fail("RollupCore address is missing from the generated chain info")

    sequencer = plan.add_service(
        name = "arb-sequencer",
        config = ServiceConfig(
            image = config["nitro_image"],
            entrypoint = ["/usr/local/bin/nitro"],
            cmd = [
                "--conf.file=/config/sequencer_config.json",
                "--node.feed.output.enable",
                "--node.feed.output.port=9642",
                "--node.seq-coordinator.my-url=http://arb-sequencer:8547",
                "--validation.wasm.allowed-wasm-module-roots=/home/user/nitro-legacy/machines,/home/user/target/machines",
            ],
            files = {
                "/config": sequencer_config_artifact,
            },
            ports = {
                "rpc": PortSpec(
                    number = 8547,
                    transport_protocol = "TCP",
                    application_protocol = "http",
                ),
                "ws": PortSpec(
                    number = 8548,
                    transport_protocol = "TCP",
                    application_protocol = "ws",
                ),
                "feed": PortSpec(
                    number = 9642,
                    transport_protocol = "TCP",
                    application_protocol = "ws",
                ),
            },
            # PortSpec has no public_port argument; fixed host ports are
            # declared here, keyed by the same port ids.
            public_ports = {
                "rpc": PortSpec(
                    number = 32200,
                    transport_protocol = "TCP",
                    application_protocol = "http",
                ),
                "ws": PortSpec(
                    number = 32201,
                    transport_protocol = "TCP",
                    application_protocol = "ws",
                ),
                "feed": PortSpec(
                    number = 32202,
                    transport_protocol = "TCP",
                    application_protocol = "ws",
                ),
            },
            ready_conditions = ReadyCondition(
                recipe = PostHttpRequestRecipe(
                    port_id = "rpc",
                    endpoint = "",
                    content_type = "application/json",
                    body = '{"jsonrpc":"2.0","method":"eth_chainId","params":[],"id":1}',
                ),
                field = "code",
                assertion = "==",
                target_value = 200,
                timeout = "15m",
                interval = "10s",
            ),
        ),
    )

    plan.print("Arbitrum Nitro is ready: RollupCore {}".format(rollup_address))
    return {
        "rollup_core": rollup_address,
        "l1_chain_id": config["l1_chain_id"],
        "l2_chain_id": config["l2_chain_id"],
        "nitro_image": config["nitro_image"],
        "l2_rpc_url": "http://{}:8547".format(sequencer.hostname),
        "l2_ws_url": "ws://{}:8548".format(sequencer.hostname),
        "feed_url": "ws://{}:9642".format(sequencer.hostname),
        "artifacts": {
            "deployment": deployment_artifact,
            "chain_info": chain_info_artifact,
            "sequencer_config": sequencer_config_artifact,
        },
    }
