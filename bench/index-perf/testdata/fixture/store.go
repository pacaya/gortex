package ledger

import "sync"

// Store persists accounts and transactions.
type Store interface {
	GetAccount(id string) (*Account, bool)
	PutAccount(a *Account)
	AppendTransaction(tx Transaction)
	Transactions() []Transaction
	Accounts() []*Account
}

// InMemoryStore is a goroutine-safe in-memory Store.
type InMemoryStore struct {
	mu       sync.RWMutex
	accounts map[string]*Account
	txns     []Transaction
}

// NewInMemoryStore returns an empty store.
func NewInMemoryStore() *InMemoryStore {
	return &InMemoryStore{accounts: make(map[string]*Account)}
}

// GetAccount returns the account with the given ID, if present.
func (s *InMemoryStore) GetAccount(id string) (*Account, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	a, ok := s.accounts[id]
	return a, ok
}

// PutAccount inserts or replaces an account.
func (s *InMemoryStore) PutAccount(a *Account) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.accounts[a.ID] = a
}

// AppendTransaction records a posted transaction.
func (s *InMemoryStore) AppendTransaction(tx Transaction) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.txns = append(s.txns, tx)
}

// Transactions returns a copy of the recorded transactions.
func (s *InMemoryStore) Transactions() []Transaction {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Transaction, len(s.txns))
	copy(out, s.txns)
	return out
}

// Accounts returns every stored account in arbitrary order.
func (s *InMemoryStore) Accounts() []*Account {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*Account, 0, len(s.accounts))
	for _, a := range s.accounts {
		out = append(out, a)
	}
	return out
}
