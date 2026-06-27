package ledger

// post applies a validated transaction's entries to account balances.
func post(store Store, tx Transaction) error {
	if err := validateBalanced(tx); err != nil {
		return &PostingError{TxID: tx.ID, Err: err}
	}
	if err := validateAccounts(store, tx); err != nil {
		return &PostingError{TxID: tx.ID, Err: err}
	}
	for _, e := range tx.Entries {
		acct, ok := store.GetAccount(e.AccountID)
		if !ok {
			return &PostingError{TxID: tx.ID, Err: ErrUnknownAccount}
		}
		acct.Balance = acct.Balance.Add(e.Amount)
		store.PutAccount(acct)
	}
	store.AppendTransaction(tx)
	return nil
}
