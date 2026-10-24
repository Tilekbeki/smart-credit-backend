package decision

import (
	"errors"

	"smart-credit-backend/internal/credit"
)

var ErrManualTerms = errors.New("manual terms violate mandatory credit conditions")

type ManualTermsError struct{ Field, Code, Message string }

func (e *ManualTermsError) Error() string { return ErrManualTerms.Error() }
func (e *ManualTermsError) Unwrap() error { return ErrManualTerms }

func invalidManualTerm(field, code, message string) error {
	return &ManualTermsError{Field: field, Code: code, Message: message}
}

// PrepareManual validates persisted assessment integrity and mandatory product
// policy, without replacing or recalculating the saved scoring assessment.
// Employees may resolve advisory ambiguities; critical business constraints and
// affordability still apply to every proposal.
func PrepareManual(input Input, outcome string, amount int64, term int) (*Offer, error) {
	data, err := decodeInput(input)
	if err != nil {
		return nil, ErrInvalidData
	}
	if outcome == "rejected" {
		if amount != 0 || term != 0 {
			return nil, invalidManualTerm("offer.amountRub", "invalid_value", "Отказ не должен содержать предложение.")
		}
		return nil, nil
	}
	if outcome != "approved" && outcome != "counter_offer" {
		return nil, invalidManualTerm("offer.amountRub", "invalid_value", "Недопустимый вид ручного решения.")
	}
	if len(criticalGrounds(data, input.SubmittedAt)) != 0 {
		return nil, invalidManualTerm("offer.amountRub", "invalid_value", "Предложение не соответствует обязательным условиям кредитной политики.")
	}
	product, request := data.product, data.request
	if amount < product.MinAmountRub || amount > product.MaxAmountRub || amount > request.RequestedAmount {
		return nil, invalidManualTerm("offer.amountRub", "out_of_range", "Сумма выходит за границы продукта или превышает исходную сумму заявки.")
	}
	if term < product.MinTermMonths || term > product.MaxTermMonths {
		return nil, invalidManualTerm("offer.termMonths", "out_of_range", "Срок выходит за границы выбранного продукта.")
	}
	if outcome == "approved" && amount != request.RequestedAmount {
		return nil, invalidManualTerm("offer.amountRub", "invalid_value", "При одобрении сумма должна совпадать с исходной заявкой.")
	}
	if outcome == "approved" && term != request.TermMonths {
		return nil, invalidManualTerm("offer.termMonths", "invalid_value", "При одобрении срок должен совпадать с исходной заявкой.")
	}
	if outcome == "counter_offer" && amount == request.RequestedAmount && term == request.TermMonths {
		return nil, invalidManualTerm("offer.amountRub", "unchanged_offer", "В альтернативном предложении должна измениться сумма или срок.")
	}
	payment, err := credit.MonthlyPayment(amount, product.AnnualRatePercent, term)
	if err != nil {
		return nil, ErrInvalidData
	}
	budget := credit.PaymentBudget(request.MonthlyRevenue, request.MonthlyExpenses, request.MonthlyDebtPayments)
	if payment > budget {
		return nil, invalidManualTerm("offer.amountRub", "invalid_value", "Платёж превышает допустимый бюджет по сохранённым финансовым данным.")
	}
	return newOffer(product, amount, term, payment), nil
}
