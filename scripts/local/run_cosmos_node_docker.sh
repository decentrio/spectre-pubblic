#!/bin/sh

set -euxo pipefail

cd "$(dirname "$0")/../.."

CONTAINER_NAME="ibc-wasm-simd"
IMAGE="ghcr.io/cosmos/ibc-go-wasm-simd:modules-light-clients-08-wasm-v10.3.0"
CHAIN_ID="test-ibc-eth"

# Stop and remove existing container
docker rm -f $CONTAINER_NAME 2>/dev/null || true

# Run setup inside the container
docker run -d --name $CONTAINER_NAME \
  -p 26657:26657 \
  -p 1317:1317 \
  -p 9090:9090 \
  -p 26656:26656 \
  --entrypoint "" \
  $IMAGE sleep infinity

# Wait for container to be ready
sleep 2

# Initialize chain
docker exec $CONTAINER_NAME simd init test-ibc --chain-id $CHAIN_ID

# Add keys
docker exec $CONTAINER_NAME simd keys add test --keyring-backend test
docker exec $CONTAINER_NAME simd keys add test1 --keyring-backend test
docker exec $CONTAINER_NAME simd keys add test2 --keyring-backend test
docker exec $CONTAINER_NAME simd keys add test3 --keyring-backend test

# Modify genesis - gov params (copy out, fix on host with python3, copy back)
docker cp $CONTAINER_NAME:/root/.simapp/config/genesis.json /tmp/genesis_fix.json
python3 -c "
import json
with open('/tmp/genesis_fix.json') as f:
    data = json.load(f)
data['app_state']['gov']['params']['voting_period'] = '30s'
data['app_state']['gov']['params']['expedited_voting_period'] = '20s'
with open('/tmp/genesis_fix.json', 'w') as f:
    json.dump(data, f)
"
docker cp /tmp/genesis_fix.json $CONTAINER_NAME:/root/.simapp/config/genesis.json

# Set minimum gas prices
docker exec $CONTAINER_NAME sh -c "sed -i 's/^minimum-gas-prices *= .*/minimum-gas-prices = \"0stake\"/' /root/.simapp/config/app.toml"

# Enable API server (for account queries)
docker exec $CONTAINER_NAME sh -c "sed -i '/^\[api\]/,/^\[/{s/^enable *= .*/enable = true/}' /root/.simapp/config/app.toml"

# Allow CORS and external connections
docker exec $CONTAINER_NAME sh -c "sed -i 's/^laddr *= \"tcp:\/\/127.0.0.1:26657\"/laddr = \"tcp:\/\/0.0.0.0:26657\"/' /root/.simapp/config/config.toml"
docker exec $CONTAINER_NAME sh -c "sed -i 's/^cors_allowed_origins *= .*/cors_allowed_origins = [\"*\"]/' /root/.simapp/config/config.toml"

# Add genesis accounts
docker exec $CONTAINER_NAME simd genesis add-genesis-account test 1100000000000stake --keyring-backend test
docker exec $CONTAINER_NAME simd genesis add-genesis-account test1 2000000000000stake --keyring-backend test
docker exec $CONTAINER_NAME simd genesis add-genesis-account test2 2000000000000stake --keyring-backend test
docker exec $CONTAINER_NAME simd genesis add-genesis-account test3 200000000000stake --keyring-backend test

# Create gentx and collect
docker exec $CONTAINER_NAME simd genesis gentx test 1000000000000stake --chain-id $CHAIN_ID --keyring-backend test
docker exec $CONTAINER_NAME simd genesis collect-gentxs
docker exec $CONTAINER_NAME simd genesis validate-genesis

# Export test1 private key (for relayer)
echo ""
echo "=== test1 private key (hex) ==="
docker exec $CONTAINER_NAME simd keys export test1 --unarmored-hex --unsafe --keyring-backend test -y
echo ""

# Print test1 address
echo "=== test1 address ==="
docker exec $CONTAINER_NAME simd keys show test1 -a --keyring-backend test
echo ""

# Start the node
docker exec -d $CONTAINER_NAME simd start

echo ""
echo "=== Cosmos node started ==="
echo "RPC:    http://localhost:26657"
echo "API:    http://localhost:1317"
echo "gRPC:   localhost:9090"
echo "Chain:  $CHAIN_ID"
echo ""
echo "To view logs:  docker logs -f $CONTAINER_NAME"
echo "To stop:       docker rm -f $CONTAINER_NAME"
