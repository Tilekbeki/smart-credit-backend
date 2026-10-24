package scoring

import (
	"bytes"
	"encoding/json"
	"io"
	"math"
	"strings"
	"unicode/utf8"
)

const maxMoney = int64(1_000_000_000_000)

// Request uses whole rubles from the submitted application's immutable snapshot.
type Request struct {
	ApplicationID       string `json:"application_id"`
	MonthlyRevenue      int64  `json:"monthly_revenue"`
	MonthlyExpenses     int64  `json:"monthly_expenses"`
	ExistingDebt        int64  `json:"existing_debt"`
	MonthlyDebtPayments int64  `json:"monthly_debt_payments"`
	OverdueCount        int    `json:"overdue_count"`
	RequestedAmount     int64  `json:"requested_amount"`
	TermMonths          int    `json:"term_months"`
}

// Response is a risk recommendation, not a final credit decision.
type Response struct {
	Decision       string   `json:"decision"`
	Score          int      `json:"score"`
	ApprovedAmount int64    `json:"approved_amount"`
	Reasons        []string `json:"reasons"`
	Factors        Factors  `json:"factors"`
}

type Factors struct {
	OperatingCashFlow           float64  `json:"operating_cash_flow"`
	OperatingMargin             float64  `json:"operating_margin"`
	EstimatedNewMonthlyPayment  float64  `json:"estimated_new_monthly_payment"`
	DebtServiceShare            *float64 `json:"debt_service_share"`
	ExistingDebtToAnnualRevenue *float64 `json:"existing_debt_to_annual_revenue"`
	OverdueCount                int      `json:"overdue_count"`
}

func ValidateRequest(request Request) error {
	length := utf8.RuneCountInString(request.ApplicationID)
	if !utf8.ValidString(request.ApplicationID) || length < 1 || length > 100 || strings.TrimSpace(request.ApplicationID) == "" {
		return failure("invalid_request", false)
	}
	for _, value := range []int64{request.MonthlyRevenue, request.MonthlyExpenses, request.ExistingDebt, request.MonthlyDebtPayments} {
		if value < 0 || value > maxMoney {
			return failure("invalid_request", false)
		}
	}
	if request.RequestedAmount < 1 || request.RequestedAmount > maxMoney || request.OverdueCount < 0 || request.OverdueCount > 100 || request.TermMonths < 1 || request.TermMonths > 120 {
		return failure("invalid_request", false)
	}
	return nil
}

func ValidateResponse(response Response) error {
	switch response.Decision {
	case "approved", "counter_offer", "rejected":
	default:
		return failure("invalid_response", false)
	}
	if response.Score < 0 || response.Score > 100 || response.ApprovedAmount < 0 || len(response.Reasons) == 0 || response.Factors.OverdueCount < 0 {
		return failure("invalid_response", false)
	}
	for _, reason := range response.Reasons {
		if reason == "" || !utf8.ValidString(reason) {
			return failure("invalid_response", false)
		}
	}
	factors := response.Factors
	for _, value := range []float64{factors.OperatingCashFlow, factors.OperatingMargin, factors.EstimatedNewMonthlyPayment} {
		if !finite(value) {
			return failure("invalid_response", false)
		}
	}
	if factors.EstimatedNewMonthlyPayment < 0 {
		return failure("invalid_response", false)
	}
	for _, value := range []*float64{factors.DebtServiceShare, factors.ExistingDebtToAnnualRevenue} {
		if value != nil && !finite(*value) {
			return failure("invalid_response", false)
		}
	}
	return nil
}

func finite(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0)
}

// DecodeResponse checks presence separately from values so zero score, zero debt
// and the two explicitly nullable ratios cannot be confused with missing fields.
func DecodeResponse(body []byte) (Response, error) {
	var response Response
	if len(body) > maxResponseBytes {
		return response, failure("invalid_response", false)
	}
	if !utf8.Valid(body) || !json.Valid(body) {
		return response, failure("invalid_json", false)
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	if err := rejectDuplicateKeys(decoder); err != nil {
		return response, err
	}
	if _, err := decoder.Token(); err != io.EOF {
		return response, failure("invalid_json", false)
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(body, &object); err != nil || object == nil {
		return response, failure("invalid_response", false)
	}
	if err := requiredObject(object, []string{"decision", "score", "approved_amount", "reasons", "factors"}, nil); err != nil {
		return response, err
	}
	var factors map[string]json.RawMessage
	if err := json.Unmarshal(object["factors"], &factors); err != nil || factors == nil {
		return response, failure("invalid_response", false)
	}
	if err := requiredObject(factors, []string{"operating_cash_flow", "operating_margin", "estimated_new_monthly_payment", "debt_service_share", "existing_debt_to_annual_revenue", "overdue_count"}, map[string]bool{"debt_service_share": true, "existing_debt_to_annual_revenue": true}); err != nil {
		return response, err
	}
	// encoding/json accepts null into a string. Check each reason explicitly.
	var reasons []json.RawMessage
	if err := json.Unmarshal(object["reasons"], &reasons); err != nil {
		return response, failure("invalid_response", false)
	}
	for _, reason := range reasons {
		if bytes.Equal(bytes.TrimSpace(reason), []byte("null")) {
			return response, failure("invalid_response", false)
		}
	}
	if err := json.Unmarshal(body, &response); err != nil {
		return Response{}, failure("invalid_response", false)
	}
	if err := ValidateResponse(response); err != nil {
		return Response{}, err
	}
	return response, nil
}

func requiredObject(object map[string]json.RawMessage, names []string, nullable map[string]bool) error {
	if len(object) != len(names) {
		return failure("invalid_response", false)
	}
	for _, name := range names {
		value, present := object[name]
		if !present || (!nullable[name] && strings.TrimSpace(string(value)) == "null") {
			return failure("invalid_response", false)
		}
	}
	return nil
}

func rejectDuplicateKeys(decoder *json.Decoder) error {
	token, err := decoder.Token()
	if err != nil {
		return failure("invalid_json", false)
	}
	delim, composite := token.(json.Delim)
	if !composite {
		return nil
	}
	switch delim {
	case '{':
		seen := make(map[string]bool)
		for decoder.More() {
			key, err := decoder.Token()
			if err != nil {
				return failure("invalid_json", false)
			}
			name, ok := key.(string)
			if !ok || seen[name] {
				return failure("invalid_response", false)
			}
			seen[name] = true
			if err := rejectDuplicateKeys(decoder); err != nil {
				return err
			}
		}
	case '[':
		for decoder.More() {
			if err := rejectDuplicateKeys(decoder); err != nil {
				return err
			}
		}
	default:
		return failure("invalid_json", false)
	}
	if _, err := decoder.Token(); err != nil {
		return failure("invalid_json", false)
	}
	return nil
}
