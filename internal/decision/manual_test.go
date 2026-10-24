package decision

import (
	"errors"
	"testing"

	"smart-credit-backend/internal/credit"
)

func TestPrepareManualResolvesAdvisoryAmbiguities(t *testing.T) {
	f := newFixture()
	f.response.Score = 62
	f.response.Decision = "rejected"
	f.response.ApprovedAmount = 0
	f.response.Factors.OperatingCashFlow = 42
	f.checks.CreditHistory.OverdueCount = 2
	f.failCheck("director_name")
	for _, tc := range []struct {
		outcome string
		amount  int64
		term    int
	}{{"approved", 1_000_000, 12}, {"counter_offer", 800_000, 12}, {"counter_offer", 1_000_000, 24}, {"rejected", 0, 0}} {
		offer, err := PrepareManual(f.input(t), tc.outcome, tc.amount, tc.term)
		if err != nil {
			t.Fatalf("%s failed: %v", tc.outcome, err)
		}
		if tc.outcome == "rejected" {
			if offer != nil {
				t.Fatal("rejected offer")
			}
			continue
		}
		payment, err := credit.MonthlyPayment(tc.amount, f.product.AnnualRatePercent, tc.term)
		if err != nil || offer.AmountRub != tc.amount || offer.TermMonths != tc.term || offer.AnnualRatePercent != "22" || offer.ProductID != "turnover" || offer.MonthlyPaymentRub != payment || offer.TotalPaymentRub != payment*int64(tc.term) || offer.OverpaymentRub != offer.TotalPaymentRub-offer.AmountRub {
			t.Fatalf("wrong frozen offer: %+v", offer)
		}
	}
}

func TestPrepareManualCannotBypassMandatoryRules(t *testing.T) {
	for _, tc := range []struct {
		name    string
		change  func(*fixture)
		outcome string
		amount  int64
		term    int
	}{
		{"high risk", func(f *fixture) { f.response.Score = 49 }, "approved", 1_000_000, 12},
		{"frozen inactive", func(f *fixture) { f.snapshot.Company.RegistrationStatus = "suspended" }, "approved", 1_000_000, 12},
		{"invalid INN", func(f *fixture) { f.snapshot.Company.Profile.INN = "bad" }, "approved", 1_000_000, 12},
		{"critical saved check", func(f *fixture) { f.failCheck("registration_number_format") }, "approved", 1_000_000, 12},
		{"business age", func(f *fixture) { f.product.MinBusinessAgeMonths = 1000 }, "approved", 1_000_000, 12},
		{"unsupported purpose", func(f *fixture) { f.product.Purposes = []string{"equipment"} }, "approved", 1_000_000, 12},
		{"original out of limits", func(f *fixture) { f.product.MaxAmountRub = 900_000 }, "counter_offer", 800_000, 12},
		{"unaffordable", func(f *fixture) { f.counterOfferBudget() }, "approved", 1_000_000, 12},
		{"above original", func(*fixture) {}, "counter_offer", 1_000_001, 12},
		{"below minimum", func(*fixture) {}, "counter_offer", 99_999, 12},
		{"bad term", func(*fixture) {}, "counter_offer", 800_000, 25},
		{"unchanged counter", func(*fixture) {}, "counter_offer", 1_000_000, 12},
		{"changed approval amount", func(*fixture) {}, "approved", 800_000, 12},
		{"changed approval term", func(*fixture) {}, "approved", 1_000_000, 24},
		{"rejected with offer", func(*fixture) {}, "rejected", 1, 12},
		{"unknown outcome", func(*fixture) {}, "signed", 1_000_000, 12},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture()
			tc.change(&f)
			if offer, err := PrepareManual(f.input(t), tc.outcome, tc.amount, tc.term); !errors.Is(err, ErrManualTerms) || offer != nil {
				t.Fatalf("offer=%+v err=%v", offer, err)
			}
		})
	}
	f := newFixture()
	f.counterOfferBudget()
	offer, err := PrepareManual(f.input(t), "counter_offer", 500_000, 12)
	if err != nil || offer.MonthlyPaymentRub > 50_000 {
		t.Fatalf("feasible counter: %+v %v", offer, err)
	}
}

func TestPrepareManualCorruptionIsTechnicalEvenForRejection(t *testing.T) {
	for _, outcome := range []string{"approved", "counter_offer", "rejected"} {
		f := newFixture()
		input := f.input(t)
		input.ProductSnapshot = []byte(`{"annualRatePercent":22}`)
		if _, err := PrepareManual(input, outcome, 0, 0); !errors.Is(err, ErrInvalidData) {
			t.Fatalf("%s corruption error=%v", outcome, err)
		}
	}
}
