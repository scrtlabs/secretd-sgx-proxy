package main

import (
	"encoding/json"
	"fmt"
	"sync"

	"github.com/syndtr/goleveldb/leveldb"
)

// BalanceInfo holds subscription state for a single subscriber
type BalanceInfo struct {
	BlocksRemaining int64 `json:"blocks_remaining"`
	// TxHashes removed — replay prevention uses separate "txhash:" keys for O(1) lookup
}

// Store is a LevelDB-backed subscription store
type Store struct {
	db *leveldb.DB
	mu sync.RWMutex
}

const keyPrefix = "sub:"

// NewStore opens (or creates) a LevelDB at the given path
func NewStore(dbPath string) (*Store, error) {
	db, err := leveldb.OpenFile(dbPath, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to open subscription DB at %s: %w", dbPath, err)
	}
	return &Store{db: db}, nil
}

// GetBlocksRemaining returns the blocks remaining for an address.
// Returns 0, false if not found.
func (s *Store) GetBlocksRemaining(addr string) (int64, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	data, err := s.db.Get([]byte(keyPrefix+addr), nil)
	if err != nil {
		return 0, false
	}

	var info BalanceInfo
	if err := json.Unmarshal(data, &info); err != nil {
		return 0, false
	}

	return info.BlocksRemaining, true
}

// GetBalance returns the full balance info for an address.
// Returns nil if not found.
func (s *Store) GetBalance(addr string) *BalanceInfo {
	s.mu.RLock()
	defer s.mu.RUnlock()

	data, err := s.db.Get([]byte(keyPrefix+addr), nil)
	if err != nil {
		return nil
	}

	var info BalanceInfo
	if err := json.Unmarshal(data, &info); err != nil {
		return nil
	}

	return &info
}

// AddBlocks adds block credits to the subscription for addr.
// Returns an error if txHash has already been used (prevents replay).
func (s *Store) AddBlocks(addr string, blocks int64, txHash string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	var info BalanceInfo

	// Load existing
	data, err := s.db.Get([]byte(keyPrefix+addr), nil)
	if err == nil {
		if err := json.Unmarshal(data, &info); err != nil {
			// Corrupted entry, reset
			info = BalanceInfo{}
		}
	}

	// Check for tx hash replay (O(1) — stored as separate key)
	if _, err := s.db.Get([]byte("txhash:"+txHash), nil); err == nil {
		return fmt.Errorf("tx hash %s has already been used", txHash)
	}

	// Calculate new block balance
	info.BlocksRemaining += blocks

	// Record the tx hash as a separate key (prevents replay, never grows BalanceInfo)
	if err := s.db.Put([]byte("txhash:"+txHash), []byte(addr), nil); err != nil {
		return fmt.Errorf("failed to record tx hash: %w", err)
	}

	// Persist subscription
	newData, err := json.Marshal(&info)
	if err != nil {
		return fmt.Errorf("failed to marshal balance info: %w", err)
	}

	if err := s.db.Put([]byte(keyPrefix+addr), newData, nil); err != nil {
		return fmt.Errorf("failed to write subscription: %w", err)
	}

	return nil
}

// HasBlocks returns true if the address has a positive block balance
func (s *Store) HasBlocks(addr string) bool {
	blocks, found := s.GetBlocksRemaining(addr)
	if !found {
		return false
	}
	return blocks > 0
}

// ConsumeGeneric deducts 1 block without deduplication (for requests without height)
func (s *Store) ConsumeGeneric(addr string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	data, err := s.db.Get([]byte(keyPrefix+addr), nil)
	if err != nil {
		return fmt.Errorf("no active subscription found")
	}

	var info BalanceInfo
	if err := json.Unmarshal(data, &info); err != nil {
		return fmt.Errorf("corrupted subscription data")
	}

	if info.BlocksRemaining <= 0 {
		return fmt.Errorf("insufficient blocks")
	}

	info.BlocksRemaining -= 1
	newData, _ := json.Marshal(&info)
	return s.db.Put([]byte(keyPrefix+addr), newData, nil)
}

// ConsumeHeight deducts 1 block if the height hasn't been requested before by this user.
// Returns true if a block was deducted, false if it was deduplicated.
func (s *Store) ConsumeHeight(addr string, height int64) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	heightKey := []byte(fmt.Sprintf("sub:%s:height:%d", addr, height))
	if _, err := s.db.Get(heightKey, nil); err == nil {
		// Already consumed this height, deduplicate
		return false, nil
	}

	data, err := s.db.Get([]byte(keyPrefix+addr), nil)
	if err != nil {
		return false, fmt.Errorf("no active subscription found")
	}

	var info BalanceInfo
	if err := json.Unmarshal(data, &info); err != nil {
		return false, fmt.Errorf("corrupted subscription data")
	}

	if info.BlocksRemaining <= 0 {
		return false, fmt.Errorf("insufficient blocks")
	}

	info.BlocksRemaining -= 1
	newData, _ := json.Marshal(&info)
	
	if err := s.db.Put([]byte(keyPrefix+addr), newData, nil); err != nil {
		return false, err
	}
	if err := s.db.Put(heightKey, []byte("1"), nil); err != nil {
		return false, err
	}

	return true, nil
}

// ConsumeHeightRange deducts blocks for a range of heights, ignoring already consumed ones.
func (s *Store) ConsumeHeightRange(addr string, start, end int64) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	data, err := s.db.Get([]byte(keyPrefix+addr), nil)
	if err != nil {
		return 0, fmt.Errorf("no active subscription found")
	}

	var info BalanceInfo
	if err := json.Unmarshal(data, &info); err != nil {
		return 0, fmt.Errorf("corrupted subscription data")
	}

	var heightsToConsume []int64
	for h := start; h <= end; h++ {
		heightKey := []byte(fmt.Sprintf("sub:%s:height:%d", addr, h))
		if _, err := s.db.Get(heightKey, nil); err != nil {
			heightsToConsume = append(heightsToConsume, h)
		}
	}

	newHeightsCount := int64(len(heightsToConsume))
	if newHeightsCount == 0 {
		return 0, nil
	}

	if info.BlocksRemaining < newHeightsCount {
		return 0, fmt.Errorf("insufficient blocks to query range (needs %d, has %d)", newHeightsCount, info.BlocksRemaining)
	}

	info.BlocksRemaining -= newHeightsCount
	newData, _ := json.Marshal(&info)

	// Write updates
	if err := s.db.Put([]byte(keyPrefix+addr), newData, nil); err != nil {
		return 0, err
	}
	for _, h := range heightsToConsume {
		heightKey := []byte(fmt.Sprintf("sub:%s:height:%d", addr, h))
		s.db.Put(heightKey, []byte("1"), nil)
	}

	return newHeightsCount, nil
}

// Close closes the underlying LevelDB
func (s *Store) Close() error {
	return s.db.Close()
}
