# secretd-sgx-proxy

`secretd-sgx-proxy` is a gRPC transparent proxy sidecar for Secret Network SGX nodes. It gates access to sgx data endpoints (traces, enclave records) behind a block-based consumption billing model.

---

## Protocol Overview

This section explains how non-SGX nodes communicate with SGX nodes and why each gRPC method exists.

### Background: SGX vs Non-SGX Nodes

Secret Network has two classes of nodes:

- **SGX node** — runs the TEE (Trusted Execution Environment) enclave. During each block, the enclave executes secret contracts inside the SGX chip and records the outputs to a local LevelDB store (`ecall_records.db`).
- **Non-SGX node** — a standard Cosmos node that does **not** have an SGX chip. To replay blocks that contain secret contract executions, it must fetch the enclave outputs from a trusted SGX node.

### How a Non-SGX Node Uses the SGX Node

The non-SGX node does **not** make a single request per block. Instead, it calls the SGX node dynamically during block production — whenever it encounters a contract execution it cannot process locally. The relevant gRPC calls are:

| Method | What it returns | When it's called |
|--------|----------------|-----------------|
| `BlockTraces` | Every storage read/write, result, and gas used for all contract executions in a block | Once per block that contains secret contract calls |
| `EcallRecord` | The random seed and validator set evidence fed into the enclave for a specific block | Once per block, for consensus verification |
| `EncryptedSeed` | A node-specific seed encrypted to the requestor's SGX cert | During node bootstrap / certificate rotation |
| `NetworkPubkey` | The IO and node public keys for a given block height | During key rotation or new node setup |
| `MachineIDProof` | A cryptographic proof that a specific machine ID was approved by the enclave at a given height | During SGX machine attestation / approval flows |
| `BlockCreateResults` | The wasm hash and code hash for each `MsgStoreCode` executed in a block | Once per block with contract uploads |
| `AnalyzeCode` | Whether a contract has IBC entry points and its required features | When a non-SGX node indexes a new contract upload |
| `EcallRecords` (batch) | A range of `EcallRecord` entries for bulk historical sync | During initial catch-up sync |

### Read/Write Data Flow

```
┌─────────────────────────────────────────────────────────────────────┐
│                         SGX Node                                     │
│                                                                      │
│   secretd-sgx-proxy :9191                                            │
│          │                                                           │
│          │ Auth check + subscription check                           │
│          │ READS/WRITES subscriptions.db (billing)                   │
│          │                                                           │
│          │ Forwards authorized gated requests via gRPC               │
│          ▼                                                           │
│   secretd :9090                                                      │
│          │                                                           │
│          │ READS ecall_records.db (LevelDB)                          │
│          │ Returns enclave outputs                                    │
│          ▼                                                           │
│   [gated response to client]                                         │
└─────────────────────────────────────────────────────────────────────┘
         ▲
         │ gRPC request (with secp256k1 auth headers)
         │
┌─────────────────┐
│ Non-SGX Node /  │
│ Client          │
└─────────────────┘
```

---

## Architecture

```
[ Non-SGX Node / Client ]  ──►  [ secretd-sgx-proxy :9191 ]  ──►  [ secretd :9090 ]
                                          │
                                  subscriptions.db
                                  (read + write)
```

The sidecar intercepts all gRPC traffic:
1. **Billing RPCs** (`AddBalance`, `CheckBalance`, `GetInfo`) are handled directly.
2. **Gated methods** require block credits — the sidecar verifies a secp256k1 signature in gRPC metadata, checks `subscriptions.db`, deducts a block credit (deduplicated by height), and forwards the request to `secretd` via gRPC.
3. **Everything else** is transparently forwarded to the backend as raw bytes with zero overhead.

---

## Features

- **Transparent gRPC Proxy** — all gated requests are forwarded to `secretd` after auth; non-gated requests pass through with zero-allocation byte pooling (`sync.Pool`)
- **Block-Based Billing** — users pay on-chain (`MsgSend`) and claim a quota of blocks via `AddBalance`. Blocks are deducted per-height automatically.
- **On-Chain Verification** — verifies payments against Tendermint RPC
- **Stateless Auth** — secp256k1 signatures in gRPC metadata, no sessions
- **Free Mode** — `--price 0` disables all gating for debugging or free public nodes

---

## Block Timing Reference

Secret Network produces **1 block every ~6 seconds**. Use this table to estimate how many blocks correspond to real-world durations:

| Duration | Blocks | Example pricing (at 1 SCRT / 14,400 blocks) |
|----------|--------|---------------------------------------------|
| 1 hour   | 600    | ~0.042 SCRT                                 |
| 1 day    | 14,400 | 1 SCRT                                      |
| 1 week   | 100,800| 7 SCRT                                      |
| 1 month  | 432,000| 30 SCRT                                     |

> **Note:** Block credits are only consumed when the subscriber actually queries a block height. If a non-SGX node is offline for a week, zero blocks are deducted. Repeated queries for the same height are also free (deduplicated).

**Example systemd configuration for "1 SCRT buys 1 day" pricing:**
```
--price 1000000 --blocks 14400
```

**Example for "1 SCRT buys 1 week":**
```
--price 1000000 --blocks 100800
```

---

## Building

**Prerequisites:**
- Go 1.22+
- `protoc` with Go plugins (only needed if modifying `billing.proto`)

```bash
# One-time: install protoc Go plugins
make install-tools

# Build (regenerates proto + compiles binary)
make

# Run tests
make test

# Build + test
make check
```

---

## Installation on SGX Node (Provider Setup)

The subscription proxy sidecar runs **on the SGX node (the service provider)**. It sits in front of the actual `secretd` gRPC daemon, intercepts incoming requests, and handles the billing validation.

### 1. Build and copy the binary

```bash
# On your dev machine:
make
scp secretd-sgx-proxy root@<SGX_NODE_IP>:/usr/local/bin/
```

### 2. Create systemd service

```bash
cat > /etc/systemd/system/secretd-sgx-proxy.service << 'EOF'
[Unit]
Description=secretd-sgx-proxy subscription proxy
After=secretd.service

[Service]
ExecStart=/usr/local/bin/secretd-sgx-proxy serve \
  --listen ":9191" \
  --backend "127.0.0.1:9090" \
  --rpc "http://127.0.0.1:26657" \
  --operator "secret1YOUR_OPERATOR_ADDRESS" \
  --price 1000000 \
  --blocks 14400 \
  --db-path "/var/lib/secretd-sgx-proxy/subscriptions.db"
Restart=always
RestartSec=5

[Install]
WantedBy=multi-user.target
EOF
```

### 3. Server CLI Options

The server/provider can configure behavior using these CLI arguments:

| Flag | Default | Description |
|------|---------|-------------|
| `--listen` | `:9191` | Address the proxy should listen on |
| `--backend` | `localhost:9090` | Backend `secretd` gRPC address |
| `--rpc` | `http://localhost:26657` | Tendermint RPC URL for transaction verification |
| `--operator` | *(required)* | Bech32 address of the operator receiving payments |
| `--price` | `1000000` | Price per package in uscrt (set to `0` for free/open mode) |
| `--blocks` | `100000` | Number of blocks of access granted per package |
| `--db-path` | `./subscriptions.db` | Path to the LevelDB subscription store |
| `--disable-dedup` | `false` | Disable block height deduplication (charge for every request, even if the height was queried before) |

### 4. Enable and start

```bash
mkdir -p /var/lib/secretd-sgx-proxy
systemctl daemon-reload
systemctl enable --now secretd-sgx-proxy
```

### 5. Verify

```bash
# From any machine:
./secretd-sgx-proxy client get-info --url <SGX_NODE_IP>:9191
```

---

## Non-SGX Node Setup (Client Setup)

This section covers how to configure a **non-SGX node** (the consumer/client) to connect to a billing-protected SGX node. The consumer does NOT run the proxy server; they only use the client commands and configure their node to talk to the provider's proxy.

### 1. Prepare your billing key

The billing key is a secp256k1 private key in hex format. It must be the **same key** you will pay with on-chain — the signer of the `MsgSend` must match the address making gated requests.

Export an existing `secretd` key to hex:

```bash
mkdir -p ~/.secretd-billing
secretd keys export <your-key-name> --unarmored-hex --unsafe > ~/.secretd-billing/key.hex
chmod 600 ~/.secretd-billing/key.hex
```

Or create a dedicated billing key first:

```bash
secretd keys add billing-key
secretd keys export billing-key --unarmored-hex --unsafe > ~/.secretd-billing/key.hex
chmod 600 ~/.secretd-billing/key.hex
```

> **Keep this file secure.** Anyone with this key can consume your subscription and send transactions from your address.

The default path is `~/.secretd-billing/key.hex`. Override with the `SECRET_BILLING_KEY_FILE` environment variable.

### 2. Get your billing address

```bash
./secretd-sgx-proxy client check-balance \
  --url <SGX_NODE_IP>:9191 \
  --key-file ~/.secretd-billing/key.hex
```

The operator address and pricing info can be fetched first:

```bash
./secretd-sgx-proxy client get-info --url <SGX_NODE_IP>:9191
# Operator:   secret1...
# Price Per Package: 1000000 uscrt
# Blocks Per Package: 100000 blocks
```

### 3. Pay for a subscription

Send a `MsgSend` on-chain to the operator address. Use `secretd` or Keplr:

```bash
secretd tx bank send <YOUR_KEY> <OPERATOR_ADDRESS> <AMOUNT>uscrt \
  --chain-id secret-4 \
  --node https://lcd.mainnet.secretsaturn.net:443 \
  --gas auto
```

Note the transaction hash from the output.

### 4. Register the payment

```bash
./secretd-sgx-proxy client add-balance \
  --url <SGX_NODE_IP>:9191 \
  --tx-hash <TX_HASH> \
  --key-file ~/.secretd-billing/key.hex

# Success! Added 100000 blocks to your subscription.
# Amount Processed: 1000000 uscrt
```

### 5. Configure the non-SGX node

The non-SGX `secretd` process reads its SGX node list from:

```
~/.secretd/config/sgx_nodes.json
```

> **Important:** point this at the **billing proxy** port (`:9191`), not the raw secretd port (`:9090`).

```bash
cat > ~/.secretd/config/sgx_nodes.json << 'EOF'
{
  "nodes": [
    "<SGX_NODE_IP>:9191"
  ]
}
EOF
```

Multiple nodes are supported for redundancy — the client selects randomly and retries failed nodes:

```json
{
  "nodes": [
    "sgx1.example.com:9191",
    "sgx2.example.com:9191"
  ]
}
```

As a fallback (if the file doesn't exist), set the `SECRET_SGX_NODE_GRPC` environment variable:

```bash
export SECRET_SGX_NODE_GRPC="<SGX_NODE_IP>:9191"
```

### 6. Start the non-SGX node

```bash
secretd start
```

The node will automatically load the billing key from `~/.secretd-billing/key.hex` and sign every ecall request with it. You'll see log lines like:

```
INF EcallClient Loaded billing key from /root/.secretd-billing/key.hex
INF EcallClient Initialized with 1 SGX nodes
```

### 7. Client CLI Command Reference

The consumer interacts with the billing sidecar using these commands:

#### Get pricing info (no auth required)

```bash
./secretd-sgx-proxy client get-info --url <NODE>:9191
```

#### Purchase subscription

Submit the transaction hash of your on-chain payment:

```bash
./secretd-sgx-proxy client add-balance \
  --url <NODE>:9191 \
  --tx-hash <TX_HASH> \
  --key-file /path/to/private_key.hex
```

#### Check balance

```bash
./secretd-sgx-proxy client check-balance \
  --url <NODE>:9191 \
  --key-file /path/to/private_key.hex
```

### 8. Monitor and renew

Check remaining blocks:

```bash
./secretd-sgx-proxy client check-balance \
  --url <SGX_NODE_IP>:9191 \
  --key-file ~/.secretd-billing/key.hex

# Status:          ACTIVE
# Blocks Remaining:99999
```

Simply repeat steps 3–4 to add more blocks. Payments stack — blocks are always added on top of the current balance.

---