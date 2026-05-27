package main

import (
	"encoding/json"
	"fmt"
	"sync"

	"github.com/syndtr/goleveldb/leveldb"
)

// BalanceInfo holds subscription state for a single subscriber
type BalanceInfo struct {
	ExpiryUnix int64    `json:"expiry_unix"`
	TxHashes   []string `json:"tx_hashes"` // audit trail of payment tx hashes
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

// GetExpiry returns the expiry unix timestamp for an address.
// Returns 0, false if not found.
func (s *Store) GetExpiry(addr string) (int64, bool) {
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

	return info.ExpiryUnix, true
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

// AddTime extends the subscription for addr by durationSec seconds.
// If the subscription is already active, time is added to the current expiry.
// If expired or nonexistent, time starts from now.
// Returns an error if txHash has already been used (prevents replay).
func (s *Store) AddTime(addr string, durationSec int64, txHash string) error {
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

	// Check for tx hash replay
	for _, h := range info.TxHashes {
		if h == txHash {
			return fmt.Errorf("tx hash %s has already been used", txHash)
		}
	}

	// Calculate new expiry
	now := timeNow().Unix()
	if info.ExpiryUnix > now {
		// Active subscription — extend from current expiry
		info.ExpiryUnix += durationSec
	} else {
		// Expired or new — start from now
		info.ExpiryUnix = now + durationSec
	}

	// Record the tx hash
	info.TxHashes = append(info.TxHashes, txHash)

	// Persist
	newData, err := json.Marshal(&info)
	if err != nil {
		return fmt.Errorf("failed to marshal balance info: %w", err)
	}

	if err := s.db.Put([]byte(keyPrefix+addr), newData, nil); err != nil {
		return fmt.Errorf("failed to write subscription: %w", err)
	}

	return nil
}

// IsActive returns true if the address has an active (non-expired) subscription
func (s *Store) IsActive(addr string) bool {
	expiry, found := s.GetExpiry(addr)
	if !found {
		return false
	}
	return expiry > timeNow().Unix()
}

// Close closes the underlying LevelDB
func (s *Store) Close() error {
	return s.db.Close()
}
