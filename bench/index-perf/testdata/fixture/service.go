package ledger

import (
	"fmt"
	"time"
)

// Service is the ledger's public API over a Store.
type Service struct {
	store  Store
	clock  func() time.Time
	events *Dispatcher
}

// NewService builds a Service backed by store.
func NewService(store Store) *Service {
	return &Service{
		store:  store,
		clock:  time.Now,
		events: NewDispatcher(),
	}
}

// OpenAccount registers a new account and returns it.
func (s *Service) OpenAccount(id, name string, kind AccountKind, currency string) *Account {
	acct := &Account{
		ID:      id,
		Name:    name,
		Kind:    kind,
		Balance: Money{Currency: currency},
		Created: s.clock(),
	}
	s.store.PutAccount(acct)
	s.events.Emit(Event{Kind: EventAccountOpened, Subject: id})
	return acct
}

// Post validates and records a transaction.
func (s *Service) Post(tx Transaction) error {
	if tx.Posted.IsZero() {
		tx.Posted = s.clock()
	}
	if err := post(s.store, tx); err != nil {
		s.events.Emit(Event{Kind: EventPostFailed, Subject: tx.ID})
		return err
	}
	s.events.Emit(Event{Kind: EventPosted, Subject: tx.ID})
	return nil
}

// Balance returns the current balance of an account.
func (s *Service) Balance(id string) (Money, error) {
	acct, ok := s.store.GetAccount(id)
	if !ok {
		return Money{}, fmt.Errorf("balance: %w", ErrUnknownAccount)
	}
	return acct.Balance, nil
}

// Subscribe registers a listener for ledger events.
func (s *Service) Subscribe(l Listener) {
	s.events.Subscribe(l)
}
