package ledger

import "sort"

// Balances returns the balance of every account keyed by ID.
func Balances(store Store) map[string]Money {
	out := make(map[string]Money)
	for _, a := range store.Accounts() {
		out[a.ID] = a.Balance
	}
	return out
}

// TrialBalance sums balances by account kind.
func TrialBalance(store Store) map[AccountKind]Money {
	out := make(map[AccountKind]Money)
	for _, a := range store.Accounts() {
		cur := out[a.Kind]
		if cur.Currency == "" {
			cur.Currency = a.Balance.Currency
		}
		out[a.Kind] = cur.Add(a.Balance)
	}
	return out
}

// RecentTransactions returns up to n most recently posted transactions.
func RecentTransactions(store Store, n int) []Transaction {
	txns := store.Transactions()
	sort.Slice(txns, func(i, j int) bool {
		return txns[i].Posted.After(txns[j].Posted)
	})
	if n > 0 && len(txns) > n {
		txns = txns[:n]
	}
	return txns
}
