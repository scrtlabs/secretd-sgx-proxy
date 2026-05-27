package main

import (
	"path/filepath"
	"testing"
	"time"
)

func TestStore_AddTime_And_IsActive(t *testing.T) {
	// Create temporary DB path
	tempDir := t.TempDir()
	dbPath := filepath.Join(tempDir, "test_sub.db")

	store, err := NewStore(dbPath)
	if err != nil {
		t.Fatalf("failed to create store: %v", err)
	}
	defer store.Close()

	// Mock clock
	mockTime := time.Date(2026, 5, 25, 12, 0, 0, 0, time.UTC)
	timeNow = func() time.Time { return mockTime }
	defer func() { timeNow = time.Now }()

	addr := "secret1ap54gsh0s8pxuug8e28509c2vclw6x3j38a2e5"
	txHash1 := "A1B2C3D4E5F6A1B2C3D4E5F6A1B2C3D4E5F6A1B2C3D4E5F6A1B2C3D4E5F6A1B2"

	// 1. Initial balance check (should be inactive)
	if store.IsActive(addr) {
		t.Error("expected new address to be inactive")
	}

	// 2. Add time (e.g., 3600 seconds)
	err = store.AddTime(addr, 3600, txHash1)
	if err != nil {
		t.Fatalf("failed to add time: %v", err)
	}

	// Should be active
	if !store.IsActive(addr) {
		t.Error("expected address to be active after adding time")
	}

	expiry, found := store.GetExpiry(addr)
	if !found {
		t.Error("expected address to have expiry")
	}
	expectedExpiry := mockTime.Unix() + 3600
	if expiry != expectedExpiry {
		t.Errorf("expected expiry %d, got %d", expectedExpiry, expiry)
	}

	// 3. Extend active subscription (add another 1800 seconds)
	txHash2 := "B2C3D4E5F6A1B2C3D4E5F6A1B2C3D4E5F6A1B2C3D4E5F6A1B2C3D4E5F6A1B2C3"
	err = store.AddTime(addr, 1800, txHash2)
	if err != nil {
		t.Fatalf("failed to extend active subscription: %v", err)
	}

	expiry, _ = store.GetExpiry(addr)
	expectedExpiry = expectedExpiry + 1800
	if expiry != expectedExpiry {
		t.Errorf("expected extended expiry %d, got %d", expectedExpiry, expiry)
	}

	// 4. Test expiration
	// Mock time moves past expiry (e.g. mockTime + 6000 seconds)
	timeNow = func() time.Time { return mockTime.Add(6000 * time.Second) }
	if store.IsActive(addr) {
		t.Error("expected subscription to be inactive after mock time moves past expiry")
	}

	// 5. Test expired extension (start from now)
	txHash3 := "C3D4E5F6A1B2C3D4E5F6A1B2C3D4E5F6A1B2C3D4E5F6A1B2C3D4E5F6A1B2C3D4"
	err = store.AddTime(addr, 1000, txHash3)
	if err != nil {
		t.Fatalf("failed to extend expired subscription: %v", err)
	}

	// New expiry should be relative to current mock time (mockTime + 6000s) + 1000s
	expiry, _ = store.GetExpiry(addr)
	expectedNewExpiry := mockTime.Add(6000*time.Second).Unix() + 1000
	if expiry != expectedNewExpiry {
		t.Errorf("expected expiry from expired extension to be %d, got %d", expectedNewExpiry, expiry)
	}
}

func TestStore_ReplayPrevention(t *testing.T) {
	tempDir := t.TempDir()
	dbPath := filepath.Join(tempDir, "test_sub.db")

	store, err := NewStore(dbPath)
	if err != nil {
		t.Fatalf("failed to create store: %v", err)
	}
	defer store.Close()

	addr := "secret1ap54gsh0s8pxuug8e28509c2vclw6x3j38a2e5"
	txHash := "A1B2C3D4E5F6A1B2C3D4E5F6A1B2C3D4E5F6A1B2C3D4E5F6A1B2C3D4E5F6A1B2"

	// Add time first time
	err = store.AddTime(addr, 3600, txHash)
	if err != nil {
		t.Fatalf("first AddTime failed: %v", err)
	}

	// Re-add time using the same tx hash
	err = store.AddTime(addr, 1800, txHash)
	if err == nil {
		t.Error("expected second AddTime with same tx hash to fail, but it succeeded")
	}
}
