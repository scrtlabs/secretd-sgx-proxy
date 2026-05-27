# secretd-billing

`secretd-billing` is a gRPC transparent proxy sidecar for Secret Network SGX nodes. It gates access to expensive compute endpoints (traces, enclave records) behind time-based subscriptions, allowing node operators to monetize their SGX infrastructure.

## Architecture

```
[ Non-SGX Node / Client ]  ──►  [ secretd-billing :9191 ]  ──►  [ secretd :9091 ]
                                          │
                                 [ LevelDB Store ]
```

The sidecar intercepts all gRPC traffic:
1. **Billing RPCs** (`AddBalance`, `CheckBalance`, `GetInfo`) are handled directly.
2. **Gated methods** (e.g., `BlockTraces`, `EcallRecord`) require an active subscription — the sidecar verifies a secp256k1 signature in gRPC metadata and checks the local store.
3. **Everything else** is transparently forwarded to the backend as raw bytes with zero overhead.

## Features

- **Transparent gRPC Proxy** — forwards non-gated requests with zero-allocation byte pooling (`sync.Pool`)
- **Time-Based Subscriptions** — users pay on-chain (`MsgSend`) and claim time via `AddBalance`
- **On-Chain Verification** — verifies payments against Tendermint RPC
- **Stateless Auth** — secp256k1 signatures in gRPC metadata, no sessions
- **Free Mode** — `--price 0` disables all gating for debugging or free public nodes
- **Keepalive** — aggressive TCP keepalive on backend connection to detect dead nodes instantly

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

## Installation on SGX Node

### 1. Build and copy the binary

```bash
# On your dev machine:
make
scp secretd-billing root@<SGX_NODE_IP>:/usr/local/bin/
```

### 2. Reconfigure secretd

Move the `secretd` gRPC port to a local-only address so the proxy can sit in front:

```bash
# On the SGX node, edit ~/.secretd/config/app.toml:
# Change grpc.address from "0.0.0.0:9090" to "127.0.0.1:9091"
sed -i 's/address = "0.0.0.0:9090"/address = "127.0.0.1:9091"/' ~/.secretd/config/app.toml
systemctl restart secretd
```

### 3. Create systemd service

```bash
cat > /etc/systemd/system/secretd-billing.service << 'EOF'
[Unit]
Description=secretd-billing subscription proxy
After=secretd.service

[Service]
ExecStart=/usr/local/bin/secretd-billing serve \
  --listen ":9191" \
  --backend "127.0.0.1:9091" \
  --rpc "http://127.0.0.1:26657" \
  --operator "secret1YOUR_OPERATOR_ADDRESS" \
  --price 1000000 \
  --period 86400 \
  --db-path "/var/lib/secretd-billing/subscriptions.db"
Restart=always
RestartSec=5

[Install]
WantedBy=multi-user.target
EOF
```

### 4. Enable and start

```bash
mkdir -p /var/lib/secretd-billing
systemctl daemon-reload
systemctl enable --now secretd-billing
```

### 5. Verify

```bash
# From any machine:
./secretd-billing client get-info --url <SGX_NODE_IP>:9191
```

## Server Options

| Flag | Default | Description |
|------|---------|-------------|
| `--listen` | `:9191` | Address to listen on |
| `--backend` | `localhost:9090` | Backend `secretd` gRPC address |
| `--rpc` | `http://localhost:26657` | Tendermint RPC for tx verification |
| `--operator` | *(required)* | Bech32 address that receives payments |
| `--price` | `1000000` | Price per period in uscrt (set to `0` for free mode) |
| `--period` | `3600` | Period duration in seconds |
| `--db-path` | `./subscriptions.db` | Path to LevelDB subscription store |

## Client CLI

### Get pricing info (no auth required)

```bash
./secretd-billing client get-info --url <NODE>:9191
```

### Purchase subscription

1. Send a `MsgSend` on-chain to the operator address (shown by `get-info`).
2. Submit the tx hash:

```bash
./secretd-billing client add-balance \
  --url <NODE>:9191 \
  --tx-hash <TX_HASH> \
  --key-file /path/to/private_key.hex
```

### Check balance

```bash
./secretd-billing client check-balance \
  --url <NODE>:9191 \
  --key-file /path/to/private_key.hex
```

## Gated Methods

The following gRPC methods require an active subscription:

- `/secret.compute.v1beta1.Query/BlockTraces`
- `/secret.compute.v1beta1.Query/EcallRecord`
- `/secret.compute.v1beta1.Query/EcallRecords`
- `/secret.compute.v1beta1.Query/EncryptedSeed`
- `/secret.compute.v1beta1.Query/MachineIDProof`
- `/secret.compute.v1beta1.Query/NetworkPubkey`
- `/secret.compute.v1beta1.Query/BlockCreateResults`
- `/secret.compute.v1beta1.Query/AnalyzeCode`

All other methods are forwarded without authentication.

## Implementation Notes

- **Codec**: Uses the `mwitkow/grpc-proxy` pattern — a custom `proxyCodec` registered globally that checks for `*rawFrame` (raw proxy bytes) and delegates everything else to `proto.Marshal`/`proto.Unmarshal`. All billing message types are `protoc`-generated and implement `proto.Message`, so the codec is trivially simple with no type switches.
- **Connection Management**: The backend connection uses aggressive keepalive (10s ping, 3s timeout) to instantly detect dead TCP connections during network partitions.
- **Buffer Pooling**: `sync.Pool` is used for raw proxy frames to minimize GC pressure during high-throughput syncing.
