package ledger

import "errors"

var (
	// ErrUnbalanced is returned when a transaction's entries do not sum to zero.
	ErrUnbalanced = errors.New("ledger: transaction is unbalanced")
	// ErrUnknownAccount is returned when an entry names an account that does not exist.
	ErrUnknownAccount = errors.New("ledger: unknown account")
	// ErrCurrencyMismatch is returned when amounts of different currencies are combined.
	ErrCurrencyMismatch = errors.New("ledger: currency mismatch")
	// ErrEmptyTransaction is returned when a transaction has no entries.
	ErrEmptyTransaction = errors.New("ledger: transaction has no entries")
)

// PostingError wraps a lower-level error with the transaction it came from.
type PostingError struct {
	TxID string
	Err  error
}

func (e *PostingError) Error() string {
	return "ledger: posting " + e.TxID + ": " + e.Err.Error()
}

func (e *PostingError) Unwrap() error {
	return e.Err
}
