package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strconv"
	"time"

	"github.com/cosmos/btcutil/bech32"
	"github.com/decred/dcrd/dcrec/secp256k1/v4"
	"github.com/decred/dcrd/dcrec/secp256k1/v4/ecdsa"
	"golang.org/x/crypto/ripemd160"
	"google.golang.org/grpc/metadata"
)

const (
	// gRPC metadata keys
	metaTimestamp = "x-sub-timestamp"
	metaPubkey    = "x-sub-pubkey"
	metaSignature = "x-sub-signature"

	// Anti-replay window
	maxTimestampDrift = 30 * time.Second

	// Bech32 prefix for Secret Network addresses
	bech32Prefix = "secret"
)

// VerifyRequest extracts and verifies the signature from gRPC metadata.
// Returns the caller's bech32 address or an error.
func VerifyRequest(ctx context.Context, fullMethod string) (string, error) {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return "", fmt.Errorf("missing metadata")
	}

	tsVals := md.Get(metaTimestamp)
	pkVals := md.Get(metaPubkey)
	sigVals := md.Get(metaSignature)

	if len(tsVals) == 0 || len(pkVals) == 0 || len(sigVals) == 0 {
		return "", fmt.Errorf("missing required metadata: %s, %s, %s", metaTimestamp, metaPubkey, metaSignature)
	}

	timestampStr := tsVals[0]
	pubkeyHex := pkVals[0]
	signatureHex := sigVals[0]

	// 1. Verify timestamp freshness (anti-replay)
	tsUnix, err := strconv.ParseInt(timestampStr, 10, 64)
	if err != nil {
		return "", fmt.Errorf("invalid timestamp: %w", err)
	}

	now := timeNow().Unix()
	drift := now - tsUnix
	if drift < 0 {
		drift = -drift
	}
	if drift > int64(maxTimestampDrift.Seconds()) {
		return "", fmt.Errorf("timestamp too old or too far in the future (drift: %ds)", drift)
	}

	// 2. Decode pubkey
	pkBytes, err := hex.DecodeString(pubkeyHex)
	if err != nil {
		return "", fmt.Errorf("invalid pubkey hex: %w", err)
	}

	pubKey, err := secp256k1.ParsePubKey(pkBytes)
	if err != nil {
		return "", fmt.Errorf("invalid secp256k1 pubkey: %w", err)
	}

	// 3. Decode 64-byte compact signature (R || S)
	sigBytes, err := hex.DecodeString(signatureHex)
	if err != nil {
		return "", fmt.Errorf("invalid signature hex: %w", err)
	}

	if len(sigBytes) != 64 {
		return "", fmt.Errorf("invalid signature length: expected 64, got %d", len(sigBytes))
	}

	var r, s secp256k1.ModNScalar
	r.SetByteSlice(sigBytes[:32])
	s.SetByteSlice(sigBytes[32:])
	sig := ecdsa.NewSignature(&r, &s)

	// 4. Reconstruct signed payload: "timestamp|fullMethod"
	payload := timestampStr + "|" + fullMethod
	hash := sha256.Sum256([]byte(payload))

	// 5. Verify signature
	if !sig.Verify(hash[:], pubKey) {
		return "", fmt.Errorf("signature verification failed")
	}

	// 6. Derive bech32 address from pubkey
	addr, err := PubKeyToBech32(pubKey)
	if err != nil {
		return "", fmt.Errorf("failed to derive address: %w", err)
	}

	return addr, nil
}

// PubKeyToBech32 derives a bech32 address from a secp256k1 public key.
// Uses the standard Cosmos address derivation: SHA256 → RIPEMD160 → bech32.
func PubKeyToBech32(pubKey *secp256k1.PublicKey) (string, error) {
	compressed := pubKey.SerializeCompressed()

	// SHA256
	sha := sha256.Sum256(compressed)

	// RIPEMD160
	rip := ripemd160.New()
	rip.Write(sha[:])
	addrBytes := rip.Sum(nil) // 20 bytes

	// Convert to 5-bit groups for bech32
	conv, err := bech32.ConvertBits(addrBytes, 8, 5, true)
	if err != nil {
		return "", fmt.Errorf("bech32 ConvertBits failed: %w", err)
	}

	addr, err := bech32.Encode(bech32Prefix, conv)
	if err != nil {
		return "", fmt.Errorf("bech32 Encode failed: %w", err)
	}

	return addr, nil
}

// SignRequest creates gRPC metadata with a valid signature for the given method.
// This is used by clients and tests.
func SignRequest(privKeyBytes []byte, fullMethod string) (metadata.MD, error) {
	privKey := secp256k1.PrivKeyFromBytes(privKeyBytes)
	pubKey := privKey.PubKey()

	timestamp := strconv.FormatInt(timeNow().Unix(), 10)

	// Sign: SHA256("timestamp|fullMethod")
	payload := timestamp + "|" + fullMethod
	hash := sha256.Sum256([]byte(payload))

	sig := ecdsa.Sign(privKey, hash[:])

	var sigBytes [64]byte
	r := sig.R()
	s := sig.S()
	rBytes := r.Bytes()
	sBytes := s.Bytes()
	copy(sigBytes[0:32], rBytes[:])
	copy(sigBytes[32:64], sBytes[:])

	md := metadata.Pairs(
		metaTimestamp, timestamp,
		metaPubkey, hex.EncodeToString(pubKey.SerializeCompressed()),
		metaSignature, hex.EncodeToString(sigBytes[:]),
	)

	return md, nil
}


