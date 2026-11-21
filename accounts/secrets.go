package accounts

import (
	"context"
	"fmt"
	"sync"

	odraz "github.com/odrazdotxyz/odraz-go-sdk"
)

// SecretStore persists secrets per account. Apps implement it on top of
// Keychain, Keystore, an OS keyring or an encrypted database.
type SecretStore interface {
	Put(ctx context.Context, accountID, key string, value []byte) error
	// Get returns an error wrapping odraz.ErrNotFound when absent.
	Get(ctx context.Context, accountID, key string) ([]byte, error)
	// Delete removes every secret of the account.
	Delete(ctx context.Context, accountID string) error
}

// MemoryStore is an in-memory SecretStore. For tests only: it is not
// persistent and not encrypted.
type MemoryStore struct {
	mu sync.Mutex
	m  map[string]map[string][]byte
}

func NewMemoryStore() *MemoryStore { return &MemoryStore{m: map[string]map[string][]byte{}} }

func (s *MemoryStore) Put(_ context.Context, id, key string, v []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.m[id] == nil {
		s.m[id] = map[string][]byte{}
	}
	s.m[id][key] = append([]byte(nil), v...)
	return nil
}

func (s *MemoryStore) Get(_ context.Context, id, key string) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.m[id][key]
	if !ok {
		return nil, fmt.Errorf("secret %s/%s: %w", id, key, odraz.ErrNotFound)
	}
	return append([]byte(nil), v...), nil
}

func (s *MemoryStore) Delete(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.m, id)
	return nil
}
