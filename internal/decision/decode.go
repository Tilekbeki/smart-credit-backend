package decision

import (
	"bytes"
	"encoding/json"
	"io"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"smart-credit-backend/internal/application"
	"smart-credit-backend/internal/company"
	"smart-credit-backend/internal/credit"
	"smart-credit-backend/internal/document"
	"smart-credit-backend/internal/scoring"
)

const maxMoney int64 = 1_000_000_000_000

// The decision package owns its minimal persisted wire representation so the
// coordinating pipeline can depend on policy without an import cycle.
type financialSnapshot struct {
	SchemaVersion int                `json:"schemaVersion"`
	ApplicationID string             `json:"applicationId"`
	UserID        string             `json:"userId"`
	CompanyID     string             `json:"companyId"`
	Fields        application.Fields `json:"fields"`
	Company       struct {
		Profile             company.Company `json:"profile"`
		FinancialDataSource string          `json:"financialDataSource"`
		RegistrationStatus  string          `json:"registrationStatus"`
	} `json:"company"`
	Documents []document.Metadata `json:"documents"`
}

type check struct {
	Code   string `json:"code"`
	Passed bool   `json:"passed"`
}

type companyCheck struct {
	Status     string    `json:"status"`
	Checks     []check   `json:"checks"`
	Problems   []Reason  `json:"problems"`
	CheckedAt  time.Time `json:"checkedAt"`
	DurationMS int64     `json:"durationMs"`
	Simulation bool      `json:"simulation"`
}

type prechecks struct {
	SchemaVersion int          `json:"schemaVersion"`
	CompanyCheck  companyCheck `json:"companyCheck"`
	CreditHistory struct {
		ExistingDebtRub            int64     `json:"existingDebtRub"`
		ExistingMonthlyPaymentsRub int64     `json:"existingMonthlyPaymentsRub"`
		OverdueCount               int       `json:"overdueCount"`
		CheckedAt                  time.Time `json:"checkedAt"`
		Simulation                 bool      `json:"simulation"`
	} `json:"creditHistory"`
	CompletedAt time.Time `json:"completedAt"`
}

type evaluationData struct {
	snapshot  financialSnapshot
	product   credit.Product
	prechecks prechecks
	request   scoring.Request
	response  scoring.Response
}

func decodeInput(input Input) (evaluationData, error) {
	var data evaluationData
	if !validID(input.ApplicationID) || input.SubmittedAt.IsZero() {
		return data, ErrInvalidData
	}
	if err := decodeFinancial(input.FinancialSnapshot, &data.snapshot); err != nil {
		return data, err
	}
	if err := decodeProduct(input.ProductSnapshot, &data.product); err != nil {
		return data, err
	}
	if err := decodePrechecks(input.PrecheckResults, &data.prechecks); err != nil {
		return data, err
	}
	if _, err := decodeObject(input.ScoringRequest, &data.request, []string{"application_id", "monthly_revenue", "monthly_expenses", "existing_debt", "monthly_debt_payments", "overdue_count", "requested_amount", "term_months"}); err != nil || scoring.ValidateRequest(data.request) != nil {
		return data, ErrInvalidData
	}
	response, err := scoring.DecodeResponse(input.ScoringResponse)
	if err != nil {
		return data, ErrInvalidData
	}
	data.response = response
	fields, history := data.snapshot.Fields, data.prechecks.CreditHistory
	expected := scoring.Request{ApplicationID: input.ApplicationID, MonthlyRevenue: *fields.MonthlyRevenueRub,
		MonthlyExpenses: *fields.MonthlyExpensesRub, ExistingDebt: history.ExistingDebtRub,
		MonthlyDebtPayments: history.ExistingMonthlyPaymentsRub, OverdueCount: history.OverdueCount,
		RequestedAmount: *fields.RequestedAmountRub, TermMonths: *fields.TermMonths}
	if data.snapshot.ApplicationID != input.ApplicationID || data.snapshot.Company.Profile.ID != data.snapshot.CompanyID ||
		data.request != expected || *fields.ProductID != data.product.ID {
		return data, ErrInvalidData
	}
	return data, nil
}

func decodeFinancial(raw []byte, snapshot *financialSnapshot) error {
	object, err := decodeObject(raw, snapshot, []string{"schemaVersion", "applicationId", "userId", "companyId", "fields", "company", "documents"})
	if err != nil || snapshot.SchemaVersion != 1 || !validID(snapshot.ApplicationID) || !validID(snapshot.UserID) || !validID(snapshot.CompanyID) {
		return ErrInvalidData
	}
	if _, err := objectFields(object["fields"], []string{"productId", "purpose", "purposeDescription", "requestedAmountRub", "termMonths", "monthlyRevenueRub", "monthlyExpensesRub"}, "purposeDescription"); err != nil {
		return err
	}
	companyObject, err := objectFields(object["company"], []string{"profile", "financialDataSource", "registrationStatus"})
	if err != nil {
		return err
	}
	switch snapshot.Company.RegistrationStatus {
	case "active", "suspended", "closed", "unknown":
	default:
		return ErrInvalidData
	}
	switch snapshot.Company.FinancialDataSource {
	case "demo_bank_feed", "unverified":
	default:
		return ErrInvalidData
	}
	profileKeys := []string{"id", "name", "organizationType", "inn", "registrationNumber", "registrationDate", "okved", "directorName", "contact", "financials", "simulation"}
	var profileShape map[string]json.RawMessage
	if json.Unmarshal(companyObject["profile"], &profileShape) != nil {
		return ErrInvalidData
	}
	for _, key := range []string{"version", "financialProvenance", "registrationDataSource"} {
		if _, ok := profileShape[key]; ok {
			profileKeys = append(profileKeys, key)
		}
	}
	profile, err := objectFields(companyObject["profile"], profileKeys)
	if err != nil {
		return err
	}
	if _, err := objectFields(profile["contact"], []string{"phone", "email", "address"}); err != nil {
		return err
	}
	if _, err := objectFields(profile["financials"], []string{"monthlyRevenueRub", "monthlyExpensesRub", "existingDebtRub", "existingMonthlyPaymentsRub", "overdueCount"}); err != nil {
		return err
	}
	fields, financials := snapshot.Fields, snapshot.Company.Profile.Financials
	if len(application.ValidateFields(&snapshot.Fields)) != 0 {
		return ErrInvalidData
	}
	if fields.ProductID == nil || strings.TrimSpace(*fields.ProductID) == "" || fields.Purpose == nil || strings.TrimSpace(*fields.Purpose) == "" ||
		fields.RequestedAmountRub == nil || *fields.RequestedAmountRub < 1 || *fields.RequestedAmountRub > maxMoney ||
		fields.TermMonths == nil || *fields.TermMonths < 1 || *fields.TermMonths > 120 || fields.MonthlyRevenueRub == nil || fields.MonthlyExpensesRub == nil ||
		!validMoney(*fields.MonthlyRevenueRub) || !validMoney(*fields.MonthlyExpensesRub) ||
		!validMoney(financials.MonthlyRevenueRub) || !validMoney(financials.MonthlyExpensesRub) ||
		!validMoney(financials.ExistingDebtRub) || !validMoney(financials.ExistingMonthlyPaymentsRub) || financials.OverdueCount < 0 || financials.OverdueCount > 100 {
		return ErrInvalidData
	}
	if *fields.Purpose == "other" && (fields.PurposeDescription == nil || strings.TrimSpace(*fields.PurposeDescription) == "") {
		return ErrInvalidData
	}
	var documents []json.RawMessage
	if err := json.Unmarshal(object["documents"], &documents); err != nil {
		return ErrInvalidData
	}
	for i, rawDocument := range documents {
		if _, err := objectFields(rawDocument, []string{"documentId", "applicationId", "documentType", "fileName", "contentType", "sizeBytes", "uploadedAt"}); err != nil {
			return err
		}
		metadata := snapshot.Documents[i]
		if !validID(metadata.DocumentID) || metadata.ApplicationID != snapshot.ApplicationID || !document.ValidType(metadata.DocumentType) ||
			metadata.SizeBytes < 1 || metadata.SizeBytes > document.MaxFileSize || metadata.UploadedAt.IsZero() || metadata.FileName == "" {
			return ErrInvalidData
		}
	}
	return nil
}

func decodeProduct(raw []byte, product *credit.Product) error {
	object, err := decodeObject(raw, product, []string{"id", "name", "purposes", "minAmountRub", "maxAmountRub", "minTermMonths", "maxTermMonths", "annualRatePercent", "minBusinessAgeMonths", "version", "illustrative"})
	if err != nil || strings.TrimSpace(product.ID) == "" || strings.TrimSpace(product.Name) == "" ||
		product.MinAmountRub < 1 || product.MaxAmountRub < product.MinAmountRub || product.MaxAmountRub > maxMoney ||
		product.MinTermMonths < 1 || product.MaxTermMonths < product.MinTermMonths || product.MaxTermMonths > 120 ||
		product.MinBusinessAgeMonths < 0 || product.Version < 1 || len(product.Purposes) == 0 {
		return ErrInvalidData
	}
	// json.Number also accepts a quoted numeric string; persisted product rates
	// must retain the JSON number contract rather than silently coercing it.
	if rate := bytes.TrimSpace(object["annualRatePercent"]); len(rate) == 0 || rate[0] == '"' {
		return ErrInvalidData
	}
	for _, purpose := range product.Purposes {
		switch purpose {
		case "stock", "expenses", "seasonal", "equipment", "upgrade", "other":
		default:
			return ErrInvalidData
		}
	}
	if _, err := credit.MonthlyPayment(0, product.AnnualRatePercent, product.MinTermMonths); err != nil {
		return ErrInvalidData
	}
	return nil
}

func decodePrechecks(raw []byte, checks *prechecks) error {
	object, err := decodeObject(raw, checks, []string{"schemaVersion", "companyCheck", "creditHistory", "completedAt"})
	if err != nil || checks.SchemaVersion != 1 || checks.CompletedAt.IsZero() {
		return ErrInvalidData
	}
	company, err := objectFields(object["companyCheck"], []string{"status", "checks", "problems", "checkedAt", "durationMs", "simulation"})
	if err != nil || checks.CompanyCheck.CheckedAt.IsZero() || checks.CompanyCheck.DurationMS < 0 {
		return ErrInvalidData
	}
	var items []json.RawMessage
	if err := json.Unmarshal(company["checks"], &items); err != nil {
		return ErrInvalidData
	}
	for _, item := range items {
		if _, err := objectFields(item, []string{"code", "passed"}); err != nil {
			return err
		}
	}
	if err := json.Unmarshal(company["problems"], &items); err != nil {
		return ErrInvalidData
	}
	for _, item := range items {
		if _, err := objectFields(item, []string{"code", "message"}); err != nil {
			return err
		}
	}
	if _, err := objectFields(object["creditHistory"], []string{"existingDebtRub", "existingMonthlyPaymentsRub", "overdueCount", "checkedAt", "simulation"}); err != nil {
		return err
	}
	history := checks.CreditHistory
	if !validMoney(history.ExistingDebtRub) || !validMoney(history.ExistingMonthlyPaymentsRub) || history.OverdueCount < 0 || history.OverdueCount > 100 || history.CheckedAt.IsZero() {
		return ErrInvalidData
	}
	return nil
}

func validMoney(value int64) bool { return value >= 0 && value <= maxMoney }

func validID(value string) bool {
	id, err := uuid.Parse(value)
	return err == nil && len(value) == 36 && id != uuid.Nil
}

func decodeObject(raw []byte, target any, fields []string) (map[string]json.RawMessage, error) {
	if len(raw) > 2<<20 || !utf8.Valid(raw) || !json.Valid(raw) {
		return nil, ErrInvalidData
	}
	if err := walkJSON(json.NewDecoder(bytes.NewReader(raw)), 0); err != nil {
		return nil, err
	}
	object, err := objectFields(raw, fields)
	if err != nil {
		return nil, err
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return nil, ErrInvalidData
	}
	if decoder.Decode(new(any)) != io.EOF {
		return nil, ErrInvalidData
	}
	return object, nil
}

func objectFields(raw []byte, fields []string, nullable ...string) (map[string]json.RawMessage, error) {
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil || object == nil || len(object) != len(fields) {
		return nil, ErrInvalidData
	}
	for _, field := range fields {
		value, exists := object[field]
		if !exists {
			return nil, ErrInvalidData
		}
		if bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			allowed := false
			for _, name := range nullable {
				allowed = allowed || name == field
			}
			if !allowed {
				return nil, ErrInvalidData
			}
		}
	}
	return object, nil
}

func walkJSON(decoder *json.Decoder, depth int) error {
	if depth > 32 {
		return ErrInvalidData
	}
	token, err := decoder.Token()
	if err != nil {
		return ErrInvalidData
	}
	delim, composite := token.(json.Delim)
	if !composite {
		return nil
	}
	switch delim {
	case '{':
		seen := map[string]bool{}
		for decoder.More() {
			key, err := decoder.Token()
			if err != nil {
				return ErrInvalidData
			}
			name, ok := key.(string)
			if !ok || seen[name] {
				return ErrInvalidData
			}
			seen[name] = true
			if err := walkJSON(decoder, depth+1); err != nil {
				return err
			}
		}
	case '[':
		for decoder.More() {
			if err := walkJSON(decoder, depth+1); err != nil {
				return err
			}
		}
	default:
		return ErrInvalidData
	}
	if _, err := decoder.Token(); err != nil {
		return ErrInvalidData
	}
	return nil
}
