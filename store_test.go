package main

import (
	"path/filepath"
	"testing"
)

func TestStore_AddBlocks_And_HasBlocks(t *testing.T) {
	tempDir := t.TempDir()
	dbPath := filepath.Join(tempDir, "test_sub.db")

	store, err := NewStore(dbPath)
	if err != nil {
		t.Fatalf("failed to create store: %v", err)
	}
	defer store.Close()

	addr := "secret1ap54gsh0s8pxuug8e28509c2vclw6x3j38a2e5"
	txHash1 := "A1B2C3D4E5F6A1B2C3D4E5F6A1B2C3D4E5F6A1B2C3D4E5F6A1B2C3D4E5F6A1B2"

	// 1. Initial balance check
	if store.HasBlocks(addr) {
		t.Error("expected new address to not have blocks")
	}

	// 2. Add blocks
	err = store.AddBlocks(addr, 1000, txHash1)
	if err != nil {
		t.Fatalf("failed to add blocks: %v", err)
	}

	if !store.HasBlocks(addr) {
		t.Error("expected address to have blocks")
	}

	blocks, found := store.GetBlocksRemaining(addr)
	if !found || blocks != 1000 {
		t.Errorf("expected 1000 blocks, got %d (found=%v)", blocks, found)
	}

	// 3. Consume block with deduplication
	deducted, err := store.ConsumeHeight(addr, 123)
	if err != nil || !deducted {
		t.Fatalf("failed to consume height: %v, deducted: %v", err, deducted)
	}

	// 4. Consume same block (should deduplicate)
	deducted, err = store.ConsumeHeight(addr, 123)
	if err != nil {
		t.Fatalf("unexpected error on deduplicated consume: %v", err)
	}
	if deducted {
		t.Error("expected second consume of same height to be deduplicated")
	}

	blocks, _ = store.GetBlocksRemaining(addr)
	if blocks != 999 {
		t.Errorf("expected 999 blocks after one deduction, got %d", blocks)
	}
	
	// 5. Test range consumption
	consumed, err := store.ConsumeHeightRange(addr, 120, 125)
	if err != nil {
		t.Fatalf("failed to consume range: %v", err)
	}
	// heights: 120, 121, 122, 123(already consumed), 124, 125 -> 5 new heights
	if consumed != 5 {
		t.Errorf("expected 5 new heights consumed, got %d", consumed)
	}

	blocks, _ = store.GetBlocksRemaining(addr)
	if blocks != 994 {
		t.Errorf("expected 994 blocks after range deduction, got %d", blocks)
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

	// Add blocks first time
	err = store.AddBlocks(addr, 1000, txHash)
	if err != nil {
		t.Fatalf("first AddBlocks failed: %v", err)
	}

	// Re-add blocks using the same tx hash
	err = store.AddBlocks(addr, 1000, txHash)
	if err == nil {
		t.Error("expected second AddBlocks with same tx hash to fail, but it succeeded")
	}
}
