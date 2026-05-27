package main

import (
	"context"
	"encoding/hex"
	"strconv"
	"testing"
	"time"

	"github.com/decred/dcrd/dcrec/secp256k1/v4"
	"google.golang.org/grpc/metadata"
)

func TestPubKeyToBech32(t *testing.T) {
	// A known private key and corresponding address for testing
	// Private key hex: 0101010101010101010101010101010101010101010101010101010101010101
	privKeyBytes, err := hex.DecodeString("0101010101010101010101010101010101010101010101010101010101010101")
	if err != nil {
		t.Fatalf("failed to decode private key hex: %v", err)
	}

	privKey := secp256k1.PrivKeyFromBytes(privKeyBytes)
	pubKey := privKey.PubKey()

	addr, err := PubKeyToBech32(pubKey)
	if err != nil {
		t.Fatalf("failed to derive address: %v", err)
	}

	// Verify derived secret address prefix and length
	expectedPrefix := "secret"
	if addr[:len(expectedPrefix)] != expectedPrefix {
		t.Errorf("expected address to start with %s, got %s", expectedPrefix, addr)
	}
}

func TestVerifyRequest_Valid(t *testing.T) {
	// Setup mock clock
	mockTime := time.Date(2026, 5, 25, 12, 0, 0, 0, time.UTC)
	timeNow = func() time.Time { return mockTime }
	defer func() { timeNow = time.Now }()

	// Generate private key
	privKey, err := secp256k1.GeneratePrivateKey()
	if err != nil {
		t.Fatalf("failed to generate private key: %v", err)
	}

	privKeyBytes := privKey.Serialize()
	fullMethod := "/secret.compute.v1beta1.Query/BlockTraces"

	// Sign request
	md, err := SignRequest(privKeyBytes, fullMethod)
	if err != nil {
		t.Fatalf("SignRequest failed: %v", err)
	}

	// Prepare incoming context
	ctx := metadata.NewIncomingContext(context.Background(), md)

	// Verify request
	addr, err := VerifyRequest(ctx, fullMethod)
	if err != nil {
		t.Fatalf("VerifyRequest failed: %v", err)
	}

	expectedAddr, _ := PubKeyToBech32(privKey.PubKey())
	if addr != expectedAddr {
		t.Errorf("expected address %s, got %s", expectedAddr, addr)
	}
}

func TestVerifyRequest_ReplayDrift(t *testing.T) {
	// Setup mock clock for client signing
	signingTime := time.Date(2026, 5, 25, 12, 0, 0, 0, time.UTC)
	timeNow = func() time.Time { return signingTime }

	privKey, _ := secp256k1.GeneratePrivateKey()
	privKeyBytes := privKey.Serialize()
	fullMethod := "/secret.compute.v1beta1.Query/BlockTraces"

	md, _ := SignRequest(privKeyBytes, fullMethod)

	// Case 1: Drift within 30s limit (e.g., 20s drift)
	timeNow = func() time.Time { return signingTime.Add(20 * time.Second) }
	ctx := metadata.NewIncomingContext(context.Background(), md)
	_, err := VerifyRequest(ctx, fullMethod)
	if err != nil {
		t.Errorf("expected verification to succeed with 20s drift, got err: %v", err)
	}

	// Case 2: Drift exceeded (e.g., 35s drift)
	timeNow = func() time.Time { return signingTime.Add(35 * time.Second) }
	ctx = metadata.NewIncomingContext(context.Background(), md)
	_, err = VerifyRequest(ctx, fullMethod)
	if err == nil {
		t.Error("expected verification to fail with 35s drift, but it succeeded")
	}

	// Restore real clock
	timeNow = time.Now
}

func TestVerifyRequest_TamperedMethod(t *testing.T) {
	// Setup mock clock
	mockTime := time.Date(2026, 5, 25, 12, 0, 0, 0, time.UTC)
	timeNow = func() time.Time { return mockTime }
	defer func() { timeNow = time.Now }()

	privKey, _ := secp256k1.GeneratePrivateKey()
	privKeyBytes := privKey.Serialize()

	// Sign for Method A
	md, _ := SignRequest(privKeyBytes, "/Query/MethodA")

	// Attempt to authenticate for Method B using Method A's signature
	ctx := metadata.NewIncomingContext(context.Background(), md)
	_, err := VerifyRequest(ctx, "/Query/MethodB")
	if err == nil {
		t.Error("expected verification to fail when query method is tampered/mismatched")
	}
}

func TestVerifyRequest_InvalidSignature(t *testing.T) {
	// Missing required headers
	ctx := context.Background()
	_, err := VerifyRequest(ctx, "/Query/Method")
	if err == nil {
		t.Error("expected verification to fail with missing metadata headers")
	}

	// Malformed headers
	badMd := metadata.Pairs(
		metaTimestamp, strconv.FormatInt(time.Now().Unix(), 10),
		metaPubkey, "invalid_hex",
		metaSignature, "invalid_hex",
	)
	ctx = metadata.NewIncomingContext(context.Background(), badMd)
	_, err = VerifyRequest(ctx, "/Query/Method")
	if err == nil {
		t.Error("expected verification to fail with malformed hex data")
	}
}
