"""
Base devnet layered onto an already-running Kurtosis Ethereum L1.

Base no longer runs vanilla OP Stack binaries: the base/base monorepo ships one
unified Rust node (`base`: execution + consensus in a single process, selected by
the `sequencer` / `rpc` / `bootnode` subcommand) plus its own batcher. This
package reproduces the topology of that repo's docker-compose devnet, minus the
ZK prover pipeline and observability stack, on the shared L1 the OP and Arbitrum
packages already settle to:

    base-bootnode   EL discv4 + CL discovery bootstrap (publishes the CL ENR)
    base-builder    sequencer (EL + CL), batches gossiped via flashblocks
    base-batcher    posts L2 batches to the shared L1
    base-client     independent follower — the node the attestor and the L2 IBC
                    contracts point at

Like the Arbitrum package it deliberately does not deploy Ethereum: the caller
supplies the Kurtosis-internal L1 endpoints and has already funded the Base role
accounts on that L1 (scripts/local/run_base_node.sh does both).

Nothing here posts dispute games — Base replaced op-proposer with a ZK prover
pipeline that this package does not run — so DisputeGameFactory.gameCount stays
0 and what gets exercised is the proposal-independent derived-root path.

All keys are the public base/base devnet development keys (etc/docker/devnet-env
in that repo). Never point this package at a public or valuable L1.
"""

DEFAULTS = {
    # ---- shared L1 (kurtosis-internal DNS) --------------------------------
    "l1_chain_id": 3151908,
    "l1_rpc_url": "http://el-1-geth-lighthouse:8545",
    "l1_beacon_url": "http://cl-1-lighthouse-geth:4000",
    # ethereum-package's genesis artifact; the L1 chain config the Base node
    # wants (--l1-config-file) is derived from it.
    "l1_genesis_artifact": "el_cl_genesis_data",
    "l1_slot_duration": 2,
    # ---- L2 ---------------------------------------------------------------
    "l2_chain_id": 84538453,
    "node_image": "base:local",
    "batcher_image": "base-batcher:local",
    "setup_image": "devnet-setup:local",
    # "blobs" or "calldata". Blobs match base/base's own devnet and production.
    # They REQUIRE a shared L1 running Lighthouse >= v8: base-node reads batches
    # back from the Fulu/PeerDAS endpoint eth/v1/beacon/blobs/{slot} (base/base
    # crates/consensus/providers/src/beacon_client.rs), which v7 does not serve
    # — there the follower 404s on every batch, declares blobs permanently
    # unavailable and resets its derivation pipeline forever with safe_l2 stuck
    # at 0. eth-network-params.yaml pins that image; fall back to "calldata"
    # (which never touches the beacon API) if the L1 is ever downgraded.
    "batcher_da_type": "blobs",
    # How many L1 confirmations the follower waits for before deriving. The
    # base compose devnet uses 15 on a 4s-slot L1; 4 keeps the derived head
    # fresh on this 2s-slot L1, which is what the attestor consumes.
    "verifier_l1_confs": 4,
    # ---- roles (base/base etc/docker/devnet-env) --------------------------
    "deployer_address": "0xf39Fd6e51aad88F6F4ce6aB8827279cffFb92266",
    "deployer_private_key": "0xac0974bec39a17e36ba4a6b4d238ff944bacb478cbed5efcae784d7bf4f2ff80",
    "sequencer_address": "0x9965507D1a55bcC2695C58ba16FB37d819B0A4dc",
    "sequencer_private_key": "8b3a350cf5c34c9194ca85829a2df0ec3153be0318b5e2d3348e872092edffba",
    "batcher_address": "0x976EA74026E726554dB657fA54763abd0C3a0aa9",
    "batcher_private_key": "0x92db14e403b83dfe3df233f83dfa3a0d7096f21ca9b0d6d6b8d88b2b4ec1564e",
    "proposer_address": "0x14dC79964da2C08b23698B3D3cc7Ca32193d9955",
    "challenger_address": "0x23618e81E3f5cdF7f54C3d65f7FBc0aBf5B21E8f",
    # ---- p2p identities ---------------------------------------------------
    "builder_p2p_key": "2a871d0798f97d79848a013d4936a73bf4cc922c825d33c1cf7073dff6d409c6",
    "builder_enode_id": "3255458e24278e31d5940f304b16300fdff3f6efd3e2a030b5818310ac67af45e28d057e6a332d07e0c5ab09d6947fd4eed1a646edbf224e2d2fec6f49f90abc",
    "el_bootnode_p2p_key": "1111111111111111111111111111111111111111111111111111111111111111",
    "el_bootnode_enode_id": "4f355bdcb7cc0af728ef3cceb9615d90684bb5b2ca5f859ab0f0b704075871aa385b6b1b8ead809ca67454d9683fcf2ba03456d6fe2c4abe2b07f0fbdbb2f1c1",
    "cl_bootnode_p2p_key": "2222222222222222222222222222222222222222222222222222222222222222",
    "seq1_p2p_key": "7c852118294e51e653712a81e05800f419141751be58f605c371e15141b007a6",
    "seq2_p2p_key": "47e179ec197488593b187f80a00eb0da91f1b9d0b13f8733639f19c30a34926a",
    # ---- Base fork activations -------------------------------------------
    # Activation BLOCK heights; setup-l2.sh converts each to a timestamp
    # (genesis + block_time * height) and patches genesis.json + rollup.json.
    # Defaults match base/base's own devnet schedule, so the chain runs the
    # newest Base forks a few blocks after genesis. "" leaves a fork unset —
    # base/base leaves Isthmus unset because op-deployer already enables it.
    "isthmus_block": "",
    "azul_block": "20",
    "beryl_block": "21",
    "cobalt_block": "22",
    # ---- devnet knobs -----------------------------------------------------
    "metering_gas_limit": 60000000,
    "metering_da_bytes": 1572860,
    "metering_target_flashblocks_per_block": 10,
    "builder_max_rejected_txs_per_block": 20000,
    "setup_timeout": "30m",
    "startup_timeout": "15m",
    # Seconds to wait before restarting a crashed batcher (see _batcher).
    "batcher_restart_delay": 2,
    # Appended to every stored file-artifact name. Kurtosis rejects storing an
    # artifact under a name the enclave already holds and has no way to delete
    # one, so a retry or a --reset in the same enclave must not reuse the
    # previous run's names. The wrapper picks the next free suffix.
    "artifact_suffix": "",
}

# Container-internal ports, identical to base/base etc/docker/devnet-env so the
# base repo's own docs and log lines still read correctly.
EL_BOOTNODE_P2P_PORT = 9303
CL_BOOTNODE_P2P_PORT = 9003

BUILDER_HTTP_PORT = 7545
BUILDER_WS_PORT = 7546
BUILDER_AUTH_PORT = 7551
BUILDER_P2P_PORT = 7303
BUILDER_FLASHBLOCKS_PORT = 7111
BUILDER_METRICS_PORT = 7090
BUILDER_CL_RPC_PORT = 7549
BUILDER_CL_P2P_PORT = 7003

CLIENT_HTTP_PORT = 8545
CLIENT_WS_PORT = 8546
CLIENT_AUTH_PORT = 8551
CLIENT_P2P_PORT = 8303
CLIENT_METRICS_PORT = 8090
CLIENT_CL_RPC_PORT = 8549
CLIENT_CL_P2P_PORT = 8003

BATCHER_METRICS_PORT = 6060

CL_BOOTNODE_ENR_PATH = "/bootnodes/cl-bootnode.enr"

def _p2p_port(number, transport_protocol = "TCP"):
    """A discovery/gossip port Kurtosis publishes but must not health-check.

    Kurtosis dials every declared port before a service counts as started. The
    discovery ports are dialled only by peers inside the enclave and some of
    them never accept a plain TCP connection at all — the bootnode advertises
    tcp/udp 9003 in its ENR but only listens on UDP — so the default check
    fails a service that is in fact healthy. Readiness is asserted where it is
    meaningful instead: the published ENR for the bootnode, /healthz for the
    nodes.
    """
    return PortSpec(number = number, transport_protocol = transport_protocol, wait = None)

def _config(args):
    supplied = args.get("base_package", args)
    config = dict(DEFAULTS)
    for key, value in supplied.items():
        if key not in config:
            fail("unknown base_package field '{}'".format(key))
        config[key] = value

    for field in [
        "l1_rpc_url",
        "l1_beacon_url",
        "l1_genesis_artifact",
        "node_image",
        "batcher_image",
        "setup_image",
        "deployer_address",
        "deployer_private_key",
        "sequencer_address",
        "sequencer_private_key",
        "batcher_address",
        "batcher_private_key",
        "proposer_address",
        "challenger_address",
        "builder_p2p_key",
        "builder_enode_id",
        "el_bootnode_p2p_key",
        "el_bootnode_enode_id",
        "cl_bootnode_p2p_key",
    ]:
        if not config[field]:
            fail("base_package.{} is required".format(field))
    if config["l1_chain_id"] <= 0 or config["l2_chain_id"] <= 0:
        fail("L1 and L2 chain IDs must be positive")
    if config["l1_chain_id"] == config["l2_chain_id"]:
        fail("L1 and L2 chain IDs must differ")
    if config["l1_slot_duration"] <= 0:
        fail("base_package.l1_slot_duration must be positive")
    if config["batcher_da_type"] not in ["calldata", "blobs"]:
        fail("base_package.batcher_da_type must be 'calldata' or 'blobs'")
    return config

def _chain_env(config, extra = {}):
    """Environment every `base` process needs to resolve `--chain dev`."""
    env = {
        "BASE_CHAIN_NAME": "dev",
        "BASE_CHAIN_L1_CHAIN_ID": str(config["l1_chain_id"]),
        "BASE_CHAIN_L2_CHAIN_ID": str(config["l2_chain_id"]),
    }
    env.update(extra)
    return env

# The base image is debian-slim: no `ip`/`awk`, but getent + sed are present.
# Kurtosis assigns container IPs dynamically, so each node resolves its own
# advertised IP at start-up instead of being handed a static one the way the
# compose devnet does.
_SELF_IP = "$(getent hosts \"$(hostname)\" | head -1 | sed 's/[[:space:]].*//')"

def _http_ready(port_id, endpoint, timeout):
    return ReadyCondition(
        recipe = GetHttpRequestRecipe(port_id = port_id, endpoint = endpoint),
        field = "code",
        assertion = "==",
        target_value = 200,
        timeout = timeout,
        interval = "2s",
    )

def _setup(plan, config):
    """op-deployer live deployment + the L1 config/JWT the Base node needs.

    setup-l2.sh (base/base etc/scripts/devnet) is already parameterised by
    L1_RPC_URL/L1_CHAIN_ID, so it runs unmodified against the shared L1. The
    same task derives /genesis/el/chain-config.json from the ethereum-package
    L1 genesis (Base wants the bare `.config` object, exactly what that repo's
    own setup-l1.sh writes) and mints the JWT the node uses between its own
    execution and consensus halves.
    """
    setup_script = " && ".join([
        "set -eu",
        "mkdir -p /genesis/l2 /genesis/el",
        "/usr/local/bin/setup-l2.sh",
        # geth v1.15 dropped terminalTotalDifficultyPassed but the pinned
        # ethereum-package genesis still carries it; drop it so a strict
        # deserializer on the Base side cannot trip over it.
        "jq '.config | del(.terminalTotalDifficultyPassed)' /l1-genesis/genesis.json > /genesis/el/chain-config.json",
        "openssl rand -hex 32 > /genesis/jwt.hex",
        "echo 'base L2 configs ready'",
    ])

    result = plan.run_sh(
        description = "Deploying the Base L1 contracts and generating the L2 configs",
        image = config["setup_image"],
        run = setup_script,
        env_vars = {
            "L1_RPC_URL": config["l1_rpc_url"],
            "L1_CHAIN_ID": str(config["l1_chain_id"]),
            "L2_CHAIN_ID": str(config["l2_chain_id"]),
            "OUTPUT_DIR": "/genesis/l2",
            "TEMPLATE_DIR": "/templates",
            "DEPLOYER_ADDR": config["deployer_address"],
            "DEPLOYER_KEY": config["deployer_private_key"],
            "SEQUENCER_ADDR": config["sequencer_address"],
            "BATCHER_ADDR": config["batcher_address"],
            "PROPOSER_ADDR": config["proposer_address"],
            "CHALLENGER_ADDR": config["challenger_address"],
            "BUILDER_P2P_KEY": config["builder_p2p_key"],
            "BUILDER_ENODE_ID": config["builder_enode_id"],
            "L2_EL_BOOTNODE_P2P_KEY": config["el_bootnode_p2p_key"],
            "L2_EL_BOOTNODE_ENODE_ID": config["el_bootnode_enode_id"],
            # Written to a convenience file only; the enode the nodes actually
            # dial is built below, once Kurtosis has assigned the bootnode's IP.
            "L2_EL_BOOTNODE_ENODE": "enode://{}@127.0.0.1:{}".format(
                config["el_bootnode_enode_id"],
                EL_BOOTNODE_P2P_PORT,
            ),
            "L2_CL_BOOTNODE_P2P_KEY": config["cl_bootnode_p2p_key"],
            "L2_CL_BOOTNODE_ENR_PATH": CL_BOOTNODE_ENR_PATH,
            "SEQ1_P2P_KEY": config["seq1_p2p_key"],
            "SEQ2_P2P_KEY": config["seq2_p2p_key"],
            "L2_ISTHMUS_BLOCK": str(config["isthmus_block"]),
            "L2_BASE_AZUL_BLOCK": str(config["azul_block"]),
            "L2_BASE_BERYL_BLOCK": str(config["beryl_block"]),
            "L2_BASE_COBALT_BLOCK": str(config["cobalt_block"]),
        },
        files = {
            "/l1-genesis": config["l1_genesis_artifact"],
        },
        store = [
            StoreSpec(
                src = "/genesis",
                name = "base-devnet-configs{}".format(config["artifact_suffix"]),
            ),
        ],
        wait = config["setup_timeout"],
    )
    return result.files_artifacts[0]

def _bootnode(plan, config, configs_artifact):
    argv = [
        "/app/base",
        "bootnode",
        "--chain", "dev",
        "--l2-config-file", "/genesis/l2/rollup.json",
        "--v4-addr=0.0.0.0:{}".format(EL_BOOTNODE_P2P_PORT),
        "--nat=extip:{}".format(_SELF_IP),
        "--p2p-secret-key=/genesis/l2/el-bootnode-p2p-key.txt",
        "--p2p.listen.tcp", str(CL_BOOTNODE_P2P_PORT),
        "--p2p.listen.udp", str(CL_BOOTNODE_P2P_PORT),
        "--p2p.advertise.ip", _SELF_IP,
        "--p2p.priv.path", "/genesis/l2/cl-bootnode-p2p-key.txt",
        "--p2p.bootstore", "/data/bootstore.json",
        "--p2p.enr-output", CL_BOOTNODE_ENR_PATH,
        "-vvv",
    ]

    service = plan.add_service(
        name = "base-bootnode",
        description = "Base EL/CL discovery bootnode",
        config = ServiceConfig(
            image = config["node_image"],
            entrypoint = ["/bin/sh", "-c"],
            cmd = ["mkdir -p /data /bootnodes && exec {}".format(" ".join(argv))],
            env_vars = _chain_env(config),
            files = {
                "/genesis": configs_artifact,
            },
            ports = {
                "el-disc": _p2p_port(EL_BOOTNODE_P2P_PORT, "UDP"),
                "cl-p2p": _p2p_port(CL_BOOTNODE_P2P_PORT),
            },
        ),
    )

    # The CL half of the bootnode writes its ENR once discovery is up; the
    # builder and the follower bootstrap from that file.
    plan.wait(
        description = "Waiting for the bootnode to publish its consensus ENR",
        service_name = service.name,
        recipe = ExecRecipe(command = ["test", "-s", CL_BOOTNODE_ENR_PATH]),
        field = "code",
        assertion = "==",
        target_value = 0,
        timeout = config["startup_timeout"],
        interval = "2s",
    )
    enr_artifact = plan.store_service_files(
        service_name = service.name,
        src = CL_BOOTNODE_ENR_PATH,
        name = "base-cl-bootnode-enr{}".format(config["artifact_suffix"]),
    )
    el_enode = "enode://{}@{}:{}".format(
        config["el_bootnode_enode_id"],
        service.ip_address,
        EL_BOOTNODE_P2P_PORT,
    )
    return (service, enr_artifact, el_enode)

def _builder(plan, config, configs_artifact, enr_artifact, el_enode):
    argv = [
        "/app/base",
        "sequencer",
        "--chain", "dev",
        "--execution-chain=/genesis/l2/genesis.json",
        "--datadir=/data",
        "--http",
        "--http.addr=0.0.0.0",
        "--http.port={}".format(BUILDER_HTTP_PORT),
        "--http.api=admin,eth,web3,net,rpc,debug,txpool,miner",
        "--http.corsdomain=*",
        "--ws",
        "--ws.addr=0.0.0.0",
        "--ws.port={}".format(BUILDER_WS_PORT),
        "--ws.api=eth,web3,net,txpool,debug",
        "--ws.origins=*",
        "--authrpc.port={}".format(BUILDER_AUTH_PORT),
        "--authrpc.addr=0.0.0.0",
        "--authrpc.jwtsecret=/genesis/jwt.hex",
        "--auth-ipc.path=/tmp/base-builder-engine.ipc",
        "--port={}".format(BUILDER_P2P_PORT),
        "--discovery.port={}".format(BUILDER_P2P_PORT),
        "--nat=extip:{}".format(_SELF_IP),
        "--p2p-secret-key-hex={}".format(config["builder_p2p_key"]),
        "--bootnodes={}".format(el_enode),
        "--rollup.discovery.v4",
        "--flashblocks.addr=0.0.0.0",
        "--flashblocks.port={}".format(BUILDER_FLASHBLOCKS_PORT),
        "--flashblocks.block-time=200",
        "--rollup.chain-block-time=2000",
        "--metrics=0.0.0.0:{}".format(BUILDER_METRICS_PORT),
        "--builder.enable-resource-metering",
        "--builder.max-rejected-txs-per-block={}".format(config["builder_max_rejected_txs_per_block"]),
        "--txpool.nolocals",
        "--rpc.txfeecap=0",
        "--rpc.gascap=600000000",
        "--l1-eth-rpc", config["l1_rpc_url"],
        "--l1-beacon", config["l1_beacon_url"],
        "--l2-config-file", "/genesis/l2/rollup.json",
        "--l1-config-file", "/genesis/el/chain-config.json",
        "--l1-slot-duration-override", str(config["l1_slot_duration"]),
        "--rpc.port", str(BUILDER_CL_RPC_PORT),
        "--rpc.enable-admin",
        "--p2p.listen.tcp", str(BUILDER_CL_P2P_PORT),
        "--p2p.listen.udp", str(BUILDER_CL_P2P_PORT),
        "--p2p.advertise.ip", "base-builder",
        "--p2p.priv.path", "/genesis/l2/builder-p2p-key.txt",
        "--p2p.bootnodes-file", CL_BOOTNODE_ENR_PATH,
        # The shared L1 confirms in seconds; sequence from its head.
        "--sequencer.l1-confs", "0",
        "--p2p.sequencer.key", config["sequencer_private_key"],
        "--p2p.scoring", "Off",
        "-vvv",
    ]

    return plan.add_service(
        name = "base-builder",
        description = "Base sequencer (execution + consensus)",
        config = ServiceConfig(
            image = config["node_image"],
            entrypoint = ["/bin/sh", "-c"],
            cmd = ["mkdir -p /data && exec {}".format(" ".join(argv))],
            env_vars = _chain_env(config),
            files = {
                "/genesis": configs_artifact,
                "/bootnodes": enr_artifact,
            },
            ports = {
                "rpc": PortSpec(number = BUILDER_HTTP_PORT, application_protocol = "http"),
                "ws": PortSpec(number = BUILDER_WS_PORT, application_protocol = "ws"),
                "cl-rpc": PortSpec(number = BUILDER_CL_RPC_PORT, application_protocol = "http"),
                "flashblocks": PortSpec(number = BUILDER_FLASHBLOCKS_PORT, application_protocol = "ws", wait = None),
                "metrics": PortSpec(number = BUILDER_METRICS_PORT, application_protocol = "http"),
                "p2p": _p2p_port(BUILDER_P2P_PORT),
                "cl-p2p": _p2p_port(BUILDER_CL_P2P_PORT),
            },
            ready_conditions = _http_ready("cl-rpc", "/healthz", config["startup_timeout"]),
        ),
    )

def _wait_for_first_l2_block(plan, builder, config):
    """Block until the sequencer has produced a block past genesis.

    The batcher picks its start cursor once, at boot: with the sequencer still
    at block 0 it takes the `cursor_start == latest_l2` branch and polls from
    the latest block, i.e. genesis — which has no transactions, so batch
    composition fails fatally (base/base crates/batcher/service/src/service.rs
    ~line 749). The compose devnet hides this behind `restart: unless-stopped`;
    Kurtosis has no restart policy and would fail the whole run, so wait for
    block 1 instead. Above genesis the batcher takes the backfill branch and
    starts at cursor_start + 1.
    """
    plan.wait(
        description = "Waiting for the sequencer to produce its first block",
        service_name = builder.name,
        recipe = PostHttpRequestRecipe(
            port_id = "rpc",
            endpoint = "",
            content_type = "application/json",
            body = '{"jsonrpc":"2.0","method":"eth_blockNumber","params":[],"id":1}',
            extract = {"block_number": ".result"},
        ),
        field = "extract.block_number",
        assertion = "!=",
        target_value = "0x0",
        timeout = config["startup_timeout"],
        interval = "2s",
    )

def _builder_enode(config, builder):
    """The sequencer's execution-layer enode, at the address Kurtosis gave it."""
    return "enode://{}@{}:{}".format(
        config["builder_enode_id"],
        builder.ip_address,
        BUILDER_P2P_PORT,
    )

def _batcher(plan, config):
    """The batcher, supervised.

    base/base's compose devnet runs the batcher with `restart: unless-stopped`,
    and it needs it: the batcher panics in normal operation (observed on
    main@dab0d0a as `range end index 21 out of range for slice of length 17`
    shortly after a confirmed submission). Kurtosis has no restart policy, so an
    unsupervised batcher stays dead, no further batches reach L1, and the
    follower's safe head silently freezes while the unsafe head keeps climbing —
    which reads exactly like a derivation bug and is not one.

    Restarting is safe: the batcher recomputes its start cursor from the safe
    head and any already-submitted blocks on boot
    (crates/batcher/service/src/service.rs), so it backfills what it missed.
    """
    argv = [
        "/app/base-batcher",
        "--l1-rpc-url", config["l1_rpc_url"],
        "--l2-rpc-url", "http://base-builder:{}".format(BUILDER_HTTP_PORT),
        "--rollup-rpc-url", "http://base-builder:{}".format(BUILDER_CL_RPC_PORT),
        "--private-key", config["batcher_private_key"],
        "--data-availability-type", config["batcher_da_type"],
        # Under DA-backlog throttling the batcher otherwise overrides the
        # configured DA type back to blobs — which a pre-v8 Lighthouse cannot
        # serve back to the follower (see batcher_da_type). Keep the configured
        # type unconditionally.
        "--no-force-blobs-when-throttling",
        "--max-channel-duration", "2",
        "--poll-interval", "1",
        "--sub-safety-margin", "0",
        "--num-confirmations", "1",
        "--metrics.enabled",
        "--metrics.addr", "0.0.0.0",
        "--metrics.port", str(BATCHER_METRICS_PORT),
    ]

    return plan.add_service(
        name = "base-batcher",
        description = "Base batcher posting L2 batches to the L1 (auto-restarting)",
        config = ServiceConfig(
            image = config["batcher_image"],
            entrypoint = ["/bin/sh", "-c"],
            cmd = [
                "while true; do {}; echo \"base-batcher exited ($?); restarting in {}s\"; sleep {}; done".format(
                    " ".join(argv),
                    config["batcher_restart_delay"],
                    config["batcher_restart_delay"],
                ),
            ],
            ports = {
                "metrics": PortSpec(number = BATCHER_METRICS_PORT, application_protocol = "http"),
            },
        ),
    )

def _client(plan, config, configs_artifact, enr_artifact, el_enode, builder):
    argv = [
        "/app/base",
        "rpc",
        "--chain", "dev",
        "--execution-chain=/genesis/l2/genesis.json",
        "--datadir=/data",
        "--http",
        "--http.addr=0.0.0.0",
        "--http.port={}".format(CLIENT_HTTP_PORT),
        "--http.api=admin,eth,web3,net,rpc,debug,txpool,miner",
        "--http.corsdomain=*",
        "--ws",
        "--ws.addr=0.0.0.0",
        "--ws.port={}".format(CLIENT_WS_PORT),
        "--ws.api=eth,web3,net,txpool,debug",
        "--ws.origins=*",
        "--authrpc.port={}".format(CLIENT_AUTH_PORT),
        "--authrpc.addr=0.0.0.0",
        "--authrpc.jwtsecret=/genesis/jwt.hex",
        "--auth-ipc.path=/tmp/base-client-engine.ipc",
        "--port={}".format(CLIENT_P2P_PORT),
        "--discovery.port={}".format(CLIENT_P2P_PORT),
        "--nat=extip:{}".format(_SELF_IP),
        "--metrics=0.0.0.0:{}".format(CLIENT_METRICS_PORT),
        "--txpool.nolocals",
        "--rpc.txfeecap=0",
        "--rpc.gascap=600000000",
        "--rpc.eth-proof-window=1209600",
        "--flashblocks-url=ws://base-builder:{}".format(BUILDER_FLASHBLOCKS_PORT),
        # Dial the sequencer's execution layer directly, in addition to the
        # discovery bootnode. Consensus gossip alone cannot repair a gap: an
        # unsafe payload whose parent is missing is rejected as invalid, and
        # every later one with it, so a single dropped block strands the
        # follower until derivation crawls past it. Recovering needs an
        # execution peer that actually holds the chain — the bootnode holds
        # none, and discovery on Kurtosis's network is not reliable enough to
        # find the builder on its own. The enode id is deterministic
        # (builder_enode_id, derived from builder_p2p_key), so it can be
        # composed here from the address Kurtosis assigned the builder.
        "--bootnodes={},{}".format(el_enode, _builder_enode(config, builder)),
        "--trusted-peers={}".format(_builder_enode(config, builder)),
        "--rollup.discovery.v4",
        "--rpc.forwarding-endpoint=http://base-builder:{}".format(BUILDER_HTTP_PORT),
        "--enable-metering",
        "--metering.gas-limit={}".format(config["metering_gas_limit"]),
        "--metering.da-bytes={}".format(config["metering_da_bytes"]),
        "--metering.target-flashblocks-per-block={}".format(
            config["metering_target_flashblocks_per_block"],
        ),
        "--l1-eth-rpc", config["l1_rpc_url"],
        "--l1-beacon", config["l1_beacon_url"],
        "--l2-config-file", "/genesis/l2/rollup.json",
        "--l1-config-file", "/genesis/el/chain-config.json",
        "--l1-slot-duration-override", str(config["l1_slot_duration"]),
        "--rpc.port", str(CLIENT_CL_RPC_PORT),
        "--p2p.listen.tcp", str(CLIENT_CL_P2P_PORT),
        "--p2p.listen.udp", str(CLIENT_CL_P2P_PORT),
        "--p2p.advertise.ip", "base-client",
        "--p2p.bootnodes-file", CL_BOOTNODE_ENR_PATH,
        "--p2p.scoring", "Off",
        "--l1.verifier-confs", str(config["verifier_l1_confs"]),
        "-vvv",
    ]

    return plan.add_service(
        name = "base-client",
        description = "Base follower node (the attestor's independent verifier)",
        config = ServiceConfig(
            image = config["node_image"],
            entrypoint = ["/bin/sh", "-c"],
            cmd = ["mkdir -p /data && exec {}".format(" ".join(argv))],
            env_vars = _chain_env(config),
            files = {
                "/genesis": configs_artifact,
                "/bootnodes": enr_artifact,
            },
            ports = {
                "rpc": PortSpec(number = CLIENT_HTTP_PORT, application_protocol = "http"),
                "ws": PortSpec(number = CLIENT_WS_PORT, application_protocol = "ws"),
                "cl-rpc": PortSpec(number = CLIENT_CL_RPC_PORT, application_protocol = "http"),
                "metrics": PortSpec(number = CLIENT_METRICS_PORT, application_protocol = "http"),
                "p2p": _p2p_port(CLIENT_P2P_PORT),
                "cl-p2p": _p2p_port(CLIENT_CL_P2P_PORT),
            },
            ready_conditions = _http_ready("cl-rpc", "/healthz", config["startup_timeout"]),
        ),
    )

def run(plan, args = {}):
    config = _config(args)
    plan.print("Attaching Base L2 chain {} to existing L1 chain {}".format(
        config["l2_chain_id"],
        config["l1_chain_id"],
    ))

    configs_artifact = _setup(plan, config)

    bootnode, enr_artifact, el_enode = _bootnode(plan, config, configs_artifact)
    plan.print("Base bootnode EL enode: {}".format(el_enode))

    builder = _builder(plan, config, configs_artifact, enr_artifact, el_enode)
    _wait_for_first_l2_block(plan, builder, config)
    batcher = _batcher(plan, config)
    client = _client(plan, config, configs_artifact, enr_artifact, el_enode, builder)

    plan.print("Base devnet is ready (no dispute games are posted: derived-root path only)")
    return {
        "l1_chain_id": config["l1_chain_id"],
        "l2_chain_id": config["l2_chain_id"],
        "sequencer_rpc_url": "http://{}:{}".format(builder.hostname, BUILDER_HTTP_PORT),
        "sequencer_cl_rpc_url": "http://{}:{}".format(builder.hostname, BUILDER_CL_RPC_PORT),
        "client_rpc_url": "http://{}:{}".format(client.hostname, CLIENT_HTTP_PORT),
        "client_ws_url": "ws://{}:{}".format(client.hostname, CLIENT_WS_PORT),
        "client_cl_rpc_url": "http://{}:{}".format(client.hostname, CLIENT_CL_RPC_PORT),
        "batcher_metrics_url": "http://{}:{}".format(batcher.hostname, BATCHER_METRICS_PORT),
        "bootnode_enode": el_enode,
        "bootnode_ip": bootnode.ip_address,
        "artifacts": {
            "configs": configs_artifact,
            "cl_bootnode_enr": enr_artifact,
        },
    }
