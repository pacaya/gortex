package ledger

// sumEntries adds up the entry amounts, requiring a single currency.
func sumEntries(entries []Entry) (Money, error) {
	if len(entries) == 0 {
		return Money{}, ErrEmptyTransaction
	}
	total := Money{Currency: entries[0].Amount.Currency}
	for _, e := range entries {
		if e.Amount.Currency != total.Currency {
			return Money{}, ErrCurrencyMismatch
		}
		total = total.Add(e.Amount)
	}
	return total, nil
}

// validateBalanced checks that a transaction's entries net to zero.
func validateBalanced(tx Transaction) error {
	total, err := sumEntries(tx.Entries)
	if err != nil {
		return err
	}
	if !total.IsZero() {
		return ErrUnbalanced
	}
	return nil
}

// validateAccounts checks that every entry references a known account.
func validateAccounts(store Store, tx Transaction) error {
	for _, e := range tx.Entries {
		if _, ok := store.GetAccount(e.AccountID); !ok {
			return ErrUnknownAccount
		}
	}
	return nil
}
