package main

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	pb "github.com/scrtlabs/secretd-billing/proto/billing"
	"github.com/decred/dcrd/dcrec/secp256k1/v4"
	"google.golang.org/grpc/metadata"
)

func TestBillingService_AddBalance_Success(t *testing.T) {
	tempDir := t.TempDir()
	dbPath := filepath.Join(tempDir, "test_billing.db")

	store, _ := NewStore(dbPath)
	defer store.Close()

	// Mock clock
	mockTime := time.Date(2026, 5, 25, 12, 0, 0, 0, time.UTC)
	timeNow = func() time.Time { return mockTime }
	defer func() { timeNow = time.Now }()

	// Generate subscriber private key
	privKey, _ := secp256k1.GeneratePrivateKey()
	privKeyBytes := privKey.Serialize()
	callerAddr, _ := PubKeyToBech32(privKey.PubKey())

	// Mock verifier
	txHash := "TX_HASH_SUCCESS"
	verifyPayment = func(rpcURL, hash, operatorAddr string, minAmount int64) (int64, string, error) {
		if hash == txHash {
			return 1000000, callerAddr, nil // Paid 1 SCRT, sender matches caller
		}
		return 0, "", fmt.Errorf("tx not found")
	}
	defer func() { verifyPayment = VerifyPayment }()

	// Create service
	pricePerPeriod := int64(1000000) // 1 SCRT
	periodSeconds := int64(3600)    // 1 hour
	service := NewBillingService(store, "http://localhost:26657", "operator_addr", pricePerPeriod, periodSeconds)

	// Prepare metadata context
	md, _ := SignRequest(privKeyBytes, "/billing.Billing/AddBalance")
	ctx := metadata.NewIncomingContext(context.Background(), md)

	// 1. Invoke AddBalance
	req := &pb.AddBalanceRequest{TxHash: txHash}
	resp, err := service.AddBalance(ctx, req)
	if err != nil {
		t.Fatalf("AddBalance failed: %v", err)
	}

	if !resp.GetActive() {
		t.Error("expected response to indicate active subscription")
	}
	expectedExpiry := mockTime.Unix() + 3600
	if resp.GetExpiryUnix() != expectedExpiry {
		t.Errorf("expected expiry %d, got %d", expectedExpiry, resp.GetExpiryUnix())
	}

	// 2. Double registration check
	_, err = service.AddBalance(ctx, req)
	if err == nil {
		t.Error("expected double registration to fail, but it succeeded")
	}
}

func TestBillingService_AddBalance_SenderMismatch(t *testing.T) {
	tempDir := t.TempDir()
	dbPath := filepath.Join(tempDir, "test_billing.db")

	store, _ := NewStore(dbPath)
	defer store.Close()

	// Mock clock
	mockTime := time.Date(2026, 5, 25, 12, 0, 0, 0, time.UTC)
	timeNow = func() time.Time { return mockTime }
	defer func() { timeNow = time.Now }()

	// Generate subscriber private key (caller is A)
	privKeyA, _ := secp256k1.GeneratePrivateKey()
	privKeyBytesA := privKeyA.Serialize()

	// Mock verifier (payment was sent by B)
	txHash := "TX_HASH_MISMATCH"
	verifyPayment = func(rpcURL, hash, operatorAddr string, minAmount int64) (int64, string, error) {
		return 1000000, "secret1different_sender", nil // Sender differs from caller A
	}
	defer func() { verifyPayment = VerifyPayment }()

	service := NewBillingService(store, "http://localhost:26657", "operator_addr", 1000000, 3600)

	// Invoke AddBalance using caller A's credentials
	md, _ := SignRequest(privKeyBytesA, "/billing.Billing/AddBalance")
	ctx := metadata.NewIncomingContext(context.Background(), md)

	req := &pb.AddBalanceRequest{TxHash: txHash}
	_, err := service.AddBalance(ctx, req)
	if err == nil {
		t.Error("expected AddBalance to fail due to sender address mismatch, but it succeeded")
	}
}

func TestBillingService_CheckBalance(t *testing.T) {
	tempDir := t.TempDir()
	dbPath := filepath.Join(tempDir, "test_billing.db")

	store, _ := NewStore(dbPath)
	defer store.Close()

	// Mock clock
	mockTime := time.Date(2026, 5, 25, 12, 0, 0, 0, time.UTC)
	timeNow = func() time.Time { return mockTime }
	defer func() { timeNow = time.Now }()

	privKey, _ := secp256k1.GeneratePrivateKey()
	privKeyBytes := privKey.Serialize()
	callerAddr, _ := PubKeyToBech32(privKey.PubKey())

	service := NewBillingService(store, "http://localhost:26657", "operator_addr", 1000000, 3600)

	md, _ := SignRequest(privKeyBytes, "/billing.Billing/CheckBalance")
	ctx := metadata.NewIncomingContext(context.Background(), md)

	// 1. Check inactive balance
	req := &pb.CheckBalanceRequest{}
	resp, err := service.CheckBalance(ctx, req)
	if err != nil {
		t.Fatalf("CheckBalance failed: %v", err)
	}
	if resp.GetActive() {
		t.Error("expected new subscriber balance to be inactive")
	}

	// 2. Add time directly to store
	_ = store.AddTime(callerAddr, 1800, "DUMMY_TX")

	// 3. Check active balance
	resp, err = service.CheckBalance(ctx, req)
	if err != nil {
		t.Fatalf("CheckBalance failed: %v", err)
	}
	if !resp.GetActive() {
		t.Error("expected subscriber balance to be active")
	}
	if resp.GetRemainingSeconds() != 1800 {
		t.Errorf("expected 1800 remaining seconds, got %d", resp.GetRemainingSeconds())
	}
}
