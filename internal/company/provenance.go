package company

import "time"

// Provenance describes a pair of values from one immutable observation. A file
// reference is supporting material, never proof that its contents were checked.
type Provenance struct {
	DeclarationID        string     `json:"declarationId"`
	Source               string     `json:"source"`
	Status               string     `json:"status"`
	MonthlyRevenueRub    int64      `json:"monthlyRevenueRub"`
	MonthlyExpensesRub   int64      `json:"monthlyExpensesRub"`
	ReceivedAt           time.Time  `json:"receivedAt"`
	VerifiedAt           *time.Time `json:"verifiedAt"`
	DeclaredBy           *string    `json:"declaredBy"`
	ReviewedBy           *string    `json:"reviewedBy"`
	SupportingDocumentID *string    `json:"supportingDocumentId"`
}

func FinancialsVerified(profile Company, source string, revenue, expenses int64) bool {
	if p := profile.FinancialProvenance; p != nil {
		return p.Status == "verified" && p.VerifiedAt != nil && !p.VerifiedAt.IsZero() && p.MonthlyRevenueRub == revenue && p.MonthlyExpensesRub == expenses
	}
	// Legacy snapshots predate per-value evidence. Only exact original values from
	// their frozen trusted source qualify; caller overrides never inherit trust.
	return source == "demo_bank_feed" && profile.Financials.MonthlyRevenueRub == revenue && profile.Financials.MonthlyExpensesRub == expenses
}
