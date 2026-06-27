package ledger

import "strings"

// NormalizeID trims and lowercases an account or transaction identifier.
func NormalizeID(id string) string {
	return strings.ToLower(strings.TrimSpace(id))
}

// Entries is a convenience constructor for a slice of entries.
func Entries(es ...Entry) []Entry {
	out := make([]Entry, 0, len(es))
	out = append(out, es...)
	return out
}

// Debit builds an entry that increases an asset or expense account.
func Debit(accountID string, amount Money, memo string) Entry {
	return Entry{AccountID: NormalizeID(accountID), Amount: amount, Memo: memo}
}

// Credit builds an entry that decreases an asset or expense account.
func Credit(accountID string, amount Money, memo string) Entry {
	return Entry{AccountID: NormalizeID(accountID), Amount: amount.Negate(), Memo: memo}
}
