package ledger

import (
	"fmt"
	"strings"
)

// FormatMoney renders an amount as a decimal string with its currency.
func FormatMoney(m Money) string {
	sign := ""
	minor := m.Minor
	if minor < 0 {
		sign = "-"
		minor = -minor
	}
	major := minor / 100
	cents := minor % 100
	return fmt.Sprintf("%s%d.%02d %s", sign, major, cents, m.Currency)
}

// FormatTransaction renders a transaction as a multi-line string.
func FormatTransaction(tx Transaction) string {
	var b strings.Builder
	fmt.Fprintf(&b, "tx %s: %s\n", tx.ID, tx.Memo)
	for _, e := range tx.Entries {
		fmt.Fprintf(&b, "  %s %s %s\n", e.AccountID, FormatMoney(e.Amount), e.Memo)
	}
	return b.String()
}

// FormatKind returns a human label for an account kind.
func FormatKind(k AccountKind) string {
	switch k {
	case AccountAsset:
		return "asset"
	case AccountLiability:
		return "liability"
	case AccountEquity:
		return "equity"
	case AccountRevenue:
		return "revenue"
	case AccountExpense:
		return "expense"
	default:
		return "unknown"
	}
}
