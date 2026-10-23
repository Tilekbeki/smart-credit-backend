package scoring

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
)

const validResponseJSON = `{"decision":"approved","score":0,"approved_amount":0,"reasons":["Synthetic evaluation."],"factors":{"operating_cash_flow":0,"operating_margin":0,"estimated_new_monthly_payment":0,"debt_service_share":null,"existing_debt_to_annual_revenue":null,"overdue_count":0}}`

func validRequest() Request {
	return Request{
		ApplicationID:       "40000000-0000-4000-8000-000000000001",
		MonthlyRevenue:      1_000_000,
		MonthlyExpenses:     500_000,
		ExistingDebt:        100_000,
		MonthlyDebtPayments: 10_000,
		OverdueCount:        0,
		RequestedAmount:     200_000,
		TermMonths:          12,
	}
}

func TestRequestContract(t *testing.T) {
	request := validRequest()
	if err := ValidateRequest(request); err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := json.Unmarshal(body, &fields); err != nil {
		t.Fatal(err)
	}
	expected := map[string]any{
		"application_id": request.ApplicationID, "monthly_revenue": float64(1_000_000),
		"monthly_expenses": float64(500_000), "existing_debt": float64(100_000),
		"monthly_debt_payments": float64(10_000), "overdue_count": float64(0),
		"requested_amount": float64(200_000), "term_months": float64(12),
	}
	if len(fields) != len(expected) {
		t.Fatalf("unexpected request field count: %d", len(fields))
	}
	for name, value := range expected {
		if fields[name] != value {
			t.Errorf("incorrect mapping for %s", name)
		}
	}
}

func TestValidateRequest(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(*Request)
	}{
		{"missing application", func(r *Request) { r.ApplicationID = "" }},
		{"long application", func(r *Request) { r.ApplicationID = strings.Repeat("я", 101) }},
		{"invalid UTF-8", func(r *Request) { r.ApplicationID = string([]byte{255}) }},
		{"negative revenue", func(r *Request) { r.MonthlyRevenue = -1 }},
		{"large revenue", func(r *Request) { r.MonthlyRevenue = maxMoney + 1 }},
		{"negative expenses", func(r *Request) { r.MonthlyExpenses = -1 }},
		{"large expenses", func(r *Request) { r.MonthlyExpenses = maxMoney + 1 }},
		{"negative debt", func(r *Request) { r.ExistingDebt = -1 }},
		{"large debt", func(r *Request) { r.ExistingDebt = maxMoney + 1 }},
		{"negative payments", func(r *Request) { r.MonthlyDebtPayments = -1 }},
		{"large payments", func(r *Request) { r.MonthlyDebtPayments = maxMoney + 1 }},
		{"negative overdue", func(r *Request) { r.OverdueCount = -1 }},
		{"large overdue", func(r *Request) { r.OverdueCount = 101 }},
		{"zero amount", func(r *Request) { r.RequestedAmount = 0 }},
		{"large amount", func(r *Request) { r.RequestedAmount = maxMoney + 1 }},
		{"zero term", func(r *Request) { r.TermMonths = 0 }},
		{"large term", func(r *Request) { r.TermMonths = 121 }},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := validRequest()
			test.change(&request)
			if err := ValidateRequest(request); ErrorCode(err) != "invalid_request" || IsRetryable(err) {
				t.Fatalf("expected permanent invalid_request, got %v", err)
			}
		})
	}
	request := validRequest()
	request.ApplicationID = strings.Repeat("я", 100)
	request.MonthlyRevenue, request.MonthlyExpenses, request.ExistingDebt, request.MonthlyDebtPayments = 0, 0, 0, 0
	request.RequestedAmount, request.TermMonths, request.OverdueCount = maxMoney, 120, 100
	if err := ValidateRequest(request); err != nil {
		t.Fatalf("valid boundary rejected: %v", err)
	}
}

func TestDecodeResponseZeroAndNullableFields(t *testing.T) {
	response, err := DecodeResponse([]byte(validResponseJSON))
	if err != nil {
		t.Fatal(err)
	}
	if response.Score != 0 || response.ApprovedAmount != 0 || response.Factors.DebtServiceShare != nil || response.Factors.ExistingDebtToAnnualRevenue != nil {
		t.Fatal("valid zero or null values not preserved")
	}
	for _, decision := range []string{"approved", "counter_offer", "rejected"} {
		body := strings.Replace(validResponseJSON, `"decision":"approved"`, `"decision":"`+decision+`"`, 1)
		body = strings.Replace(body, `"score":0`, `"score":100`, 1)
		body = strings.ReplaceAll(body, `:null`, `:0.25`)
		body = strings.Replace(body, `"operating_margin":0`, `"operating_margin":-0.5`, 1)
		response, err := DecodeResponse([]byte(body))
		if err != nil || response.Score != 100 || response.Factors.DebtServiceShare == nil || *response.Factors.DebtServiceShare != 0.25 {
			t.Fatalf("valid response rejected: %v", err)
		}
	}
}

func TestDecodeResponseRequiredFields(t *testing.T) {
	var original map[string]json.RawMessage
	if err := json.Unmarshal([]byte(validResponseJSON), &original); err != nil {
		t.Fatal(err)
	}
	for name := range original {
		for _, nullValue := range []bool{false, true} {
			t.Run(name+map[bool]string{false: " missing", true: " null"}[nullValue], func(t *testing.T) {
				copy := cloneJSONFields(original)
				if nullValue {
					copy[name] = json.RawMessage("null")
				} else {
					delete(copy, name)
				}
				body, err := json.Marshal(copy)
				if err != nil {
					t.Fatal(err)
				}
				assertDecodeError(t, string(body), "invalid_response")
			})
		}
	}
	var factors map[string]json.RawMessage
	if err := json.Unmarshal(original["factors"], &factors); err != nil {
		t.Fatal(err)
	}
	for name := range factors {
		t.Run("missing factor "+name, func(t *testing.T) {
			copy := cloneJSONFields(factors)
			delete(copy, name)
			changed := cloneJSONFields(original)
			changed["factors"], _ = json.Marshal(copy)
			body, _ := json.Marshal(changed)
			assertDecodeError(t, string(body), "invalid_response")
		})
		if name == "debt_service_share" || name == "existing_debt_to_annual_revenue" {
			continue
		}
		t.Run("null factor "+name, func(t *testing.T) {
			copy := cloneJSONFields(factors)
			copy[name] = json.RawMessage("null")
			changed := cloneJSONFields(original)
			changed["factors"], _ = json.Marshal(copy)
			body, _ := json.Marshal(changed)
			assertDecodeError(t, string(body), "invalid_response")
		})
	}
}

func TestDecodeResponseInvalidJSONAndContract(t *testing.T) {
	for _, test := range []struct{ name, body, code string }{
		{"malformed", `{"decision":`, "invalid_json"},
		{"trailing object", validResponseJSON + `{}`, "invalid_json"},
		{"trailing text", validResponseJSON + `text`, "invalid_json"},
		{"invalid UTF-8", string([]byte{255}), "invalid_json"},
		{"null", `null`, "invalid_response"},
		{"array", `[]`, "invalid_response"},
		{"empty", `{}`, "invalid_response"},
		{"string score", strings.Replace(validResponseJSON, `"score":0`, `"score":"0"`, 1), "invalid_response"},
		{"fractional score", strings.Replace(validResponseJSON, `"score":0`, `"score":0.5`, 1), "invalid_response"},
		{"negative score", strings.Replace(validResponseJSON, `"score":0`, `"score":-1`, 1), "invalid_response"},
		{"large score", strings.Replace(validResponseJSON, `"score":0`, `"score":101`, 1), "invalid_response"},
		{"decision enum", strings.Replace(validResponseJSON, `"approved"`, `"manual_review"`, 1), "invalid_response"},
		{"negative amount", strings.Replace(validResponseJSON, `"approved_amount":0`, `"approved_amount":-1`, 1), "invalid_response"},
		{"fractional amount", strings.Replace(validResponseJSON, `"approved_amount":0`, `"approved_amount":0.1`, 1), "invalid_response"},
		{"empty reasons", strings.Replace(validResponseJSON, `["Synthetic evaluation."]`, `[]`, 1), "invalid_response"},
		{"number reason", strings.Replace(validResponseJSON, `["Synthetic evaluation."]`, `[1]`, 1), "invalid_response"},
		{"null reason", strings.Replace(validResponseJSON, `["Synthetic evaluation."]`, `[null]`, 1), "invalid_response"},
		{"negative payment", strings.Replace(validResponseJSON, `"estimated_new_monthly_payment":0`, `"estimated_new_monthly_payment":-1`, 1), "invalid_response"},
		{"negative overdue", strings.Replace(validResponseJSON, `"overdue_count":0`, `"overdue_count":-1`, 1), "invalid_response"},
		{"string factor", strings.Replace(validResponseJSON, `"operating_margin":0`, `"operating_margin":"0"`, 1), "invalid_response"},
		{"infinite factor", strings.Replace(validResponseJSON, `"operating_margin":0`, `"operating_margin":1e999`, 1), "invalid_response"},
		{"string nullable factor", strings.Replace(validResponseJSON, `"debt_service_share":null`, `"debt_service_share":"0"`, 1), "invalid_response"},
		{"unknown", strings.Replace(validResponseJSON, `"score":0`, `"score":0,"extra":1`, 1), "invalid_response"},
		{"unknown factor", strings.Replace(validResponseJSON, `"operating_margin":0`, `"operating_margin":0,"extra":1`, 1), "invalid_response"},
		{"duplicate", strings.Replace(validResponseJSON, `"score":0`, `"score":0,"score":99`, 1), "invalid_response"},
		{"escaped duplicate", strings.Replace(validResponseJSON, `"score":0`, `"score":0,"sc\u006fre":99`, 1), "invalid_response"},
		{"duplicate factor", strings.Replace(validResponseJSON, `"operating_margin":0`, `"operating_margin":0,"operating_margin":1`, 1), "invalid_response"},
	} {
		t.Run(test.name, func(t *testing.T) { assertDecodeError(t, test.body, test.code) })
	}
}

func TestValidateResponseNonFiniteNumbers(t *testing.T) {
	for _, nonfinite := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
		for _, change := range []func(*Factors){
			func(f *Factors) { f.OperatingCashFlow = nonfinite },
			func(f *Factors) { f.OperatingMargin = nonfinite },
			func(f *Factors) { f.EstimatedNewMonthlyPayment = nonfinite },
			func(f *Factors) { f.DebtServiceShare = &nonfinite },
			func(f *Factors) { f.ExistingDebtToAnnualRevenue = &nonfinite },
		} {
			response, err := DecodeResponse([]byte(validResponseJSON))
			if err != nil {
				t.Fatal(err)
			}
			change(&response.Factors)
			if err := ValidateResponse(response); ErrorCode(err) != "invalid_response" {
				t.Fatalf("nonfinite factor accepted: %v", err)
			}
		}
	}
}

func assertDecodeError(t *testing.T, body, code string) {
	t.Helper()
	if _, err := DecodeResponse([]byte(body)); ErrorCode(err) != code || IsRetryable(err) {
		t.Fatalf("expected permanent %s, got %v", code, err)
	}
}

func cloneJSONFields(source map[string]json.RawMessage) map[string]json.RawMessage {
	result := make(map[string]json.RawMessage, len(source))
	for key, value := range source {
		result[key] = value
	}
	return result
}
