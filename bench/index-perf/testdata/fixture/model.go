package ledger

import "time"

// AccountKind classifies an account in the double-entry ledger.
type AccountKind int

const (
	AccountAsset AccountKind = iota
	AccountLiability
	AccountEquity
	AccountRevenue
	AccountExpense
)

// Money is a minor-unit monetary amount in a fixed currency.
type Money struct {
	Minor    int64
	Currency string
}

// Account is a single ledger account.
type Account struct {
	ID      string
	Name    string
	Kind    AccountKind
	Balance Money
	Created time.Time
}

// Entry is one leg of a transaction posted against an account.
type Entry struct {
	AccountID string
	Amount    Money
	Memo      string
}

// Transaction is a balanced set of entries.
type Transaction struct {
	ID      string
	Posted  time.Time
	Entries []Entry
	Memo    string
}

// Add returns the sum of two amounts in the same currency.
func (m Money) Add(other Money) Money {
	return Money{Minor: m.Minor + other.Minor, Currency: m.Currency}
}

// Sub returns the difference of two amounts in the same currency.
func (m Money) Sub(other Money) Money {
	return Money{Minor: m.Minor - other.Minor, Currency: m.Currency}
}

// IsZero reports whether the amount is exactly zero.
func (m Money) IsZero() bool {
	return m.Minor == 0
}

// Negate flips the sign of an amount.
func (m Money) Negate() Money {
	return Money{Minor: -m.Minor, Currency: m.Currency}
}
