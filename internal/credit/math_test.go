package credit

import (
	"encoding/json"
	"testing"
	"time"
)

func TestBusinessAgeMonths(t *testing.T) {
	tests := []struct {
		name, registered, now string
		want                  int
	}{
		{"same day", "2026-10-23", "2026-10-23", 0},
		{"future registration", "2026-10-24", "2026-10-23", 0},
		{"before sixth anniversary", "2026-04-24", "2026-10-23", 5},
		{"sixth anniversary", "2026-04-23", "2026-10-23", 6},
		{"full year", "2025-10-23", "2026-10-23", 12},
		{"year incomplete", "2025-10-24", "2026-10-23", 11},
		{"non leap month end", "2025-01-31", "2025-02-28", 1},
		{"leap month end", "2024-01-31", "2024-02-29", 1},
		{"leap month before end", "2024-01-31", "2024-02-28", 0},
		{"end of April", "2025-01-31", "2025-04-30", 3},
		{"not cumulative clamping", "2025-01-31", "2025-03-30", 1},
		{"leap year registration", "2024-02-29", "2025-02-28", 12},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			registered, err := time.Parse(time.DateOnly, tt.registered)
			if err != nil {
				t.Fatal(err)
			}
			now, err := time.Parse(time.DateOnly, tt.now)
			if err != nil {
				t.Fatal(err)
			}
			if got := BusinessAgeMonths(registered, now); got != tt.want {
				t.Fatalf("BusinessAgeMonths(%s, %s) = %d, want %d", tt.registered, tt.now, got, tt.want)
			}
		})
	}
	t.Run("date uses UTC and ignores time of day", func(t *testing.T) {
		registered, _ := time.Parse(time.RFC3339, "2025-02-01T01:00:00+03:00")
		now, _ := time.Parse(time.RFC3339, "2025-02-28T23:59:59+03:00")
		if got := BusinessAgeMonths(registered, now); got != 1 {
			t.Fatalf("age = %d, want 1", got)
		}
	})
}

func TestPaymentBudget(t *testing.T) {
	for _, tc := range []struct {
		name                          string
		revenue, expenses, debt, want int64
	}{
		{"half surplus less existing payments", 1_000_000, 500_000, 10_000, 240_000},
		{"round down odd surplus", 1_000_001, 500_000, 10_000, 240_000},
		{"no surplus", 1_000_000, 1_000_000, 0, 0},
		{"negative surplus", 0, 1_000_000, 0, 0},
		{"existing debt exhausts budget", 1_000_000, 500_000, 300_000, 0},
		{"legitimate zero", 0, 0, 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := PaymentBudget(tc.revenue, tc.expenses, tc.debt); got != tc.want {
				t.Fatalf("budget = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestMonthlyPayment(t *testing.T) {
	tests := []struct {
		name   string
		amount int64
		rate   json.Number
		term   int
		want   int64
	}{
		{"turnover twelve months", 1_000_000, "22", 12, 93_595},
		{"turnover twenty four months", 1_000_000, "22.0000", 24, 51_879},
		{"investment twelve months", 1_000_000, "20", 12, 92_635},
		{"investment sixty months", 1_000_000, "20", 60, 26_494},
		{"turnover minimum", 100_000, "22", 6, 17_753},
		{"zero rate exact division", 120_000, "0", 12, 10_000},
		{"zero rate round up", 100_000, "0.0", 12, 8_334},
		{"zero principal", 0, "22", 12, 0},
		{"exponential decimal", 1_000_000, "2.2e1", 12, 93_595},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := MonthlyPayment(tt.amount, tt.rate, tt.term)
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Fatalf("payment = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestAvailableAmount(t *testing.T) {
	tests := []struct {
		name   string
		budget int64
		rate   json.Number
		term   int
		max    int64
		want   int64
	}{
		{"rational result floors", 50_000, "22", 12, 3_000_000, 534_220},
		{"capped by product", 500_000, "22", 12, 3_000_000, 3_000_000},
		{"zero budget", 0, "22", 12, 3_000_000, 0},
		{"zero rate", 10_000, "0", 12, 3_000_000, 120_000},
		{"zero cap", 50_000, "22", 12, 0, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := AvailableAmount(tt.budget, tt.rate, tt.term, tt.max)
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Fatalf("available = %d, want %d", got, tt.want)
			}
			payment, err := MonthlyPayment(got, tt.rate, tt.term)
			if err != nil {
				t.Fatal(err)
			}
			if payment > tt.budget {
				t.Fatalf("available amount payment %d exceeds budget %d", payment, tt.budget)
			}
			if got < tt.max {
				nextPayment, err := MonthlyPayment(got+1, tt.rate, tt.term)
				if err != nil {
					t.Fatal(err)
				}
				if nextPayment <= tt.budget {
					t.Fatalf("floor is not maximal: next payment %d <= budget %d", nextPayment, tt.budget)
				}
			}
		})
	}
}

func TestFinancialParameterValidation(t *testing.T) {
	for _, rate := range []json.Number{"", "NaN", "Inf", "-1", "1001", "1/2", "1e100000000", "1e-100000000", "01", "0x10", "null", "22 trailing"} {
		t.Run("rate "+rate.String(), func(t *testing.T) {
			if _, err := MonthlyPayment(100_000, rate, 12); err == nil {
				t.Fatal("invalid rate accepted")
			}
			if _, err := AvailableAmount(10_000, rate, 12, 1_000_000); err == nil {
				t.Fatal("invalid rate accepted for available amount")
			}
		})
	}
	if _, err := MonthlyPayment(-1, "22", 12); err == nil {
		t.Fatal("negative amount accepted")
	}
	if _, err := MonthlyPayment(maxInputAmount+1, "22", 12); err == nil {
		t.Fatal("too large amount accepted")
	}
	if _, err := MonthlyPayment(1, "22", 0); err == nil {
		t.Fatal("zero term accepted")
	}
	if _, err := MonthlyPayment(1, "22", 121); err == nil {
		t.Fatal("too long term accepted")
	}
	if _, err := AvailableAmount(-1, "22", 12, 1_000_000); err == nil {
		t.Fatal("negative budget accepted")
	}
	if _, err := AvailableAmount(1, "22", 12, -1); err == nil {
		t.Fatal("negative cap accepted")
	}
}
