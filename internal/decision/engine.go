package decision

import (
	"math"
	"slices"
	"strings"
	"time"

	"smart-credit-backend/internal/application"
	"smart-credit-backend/internal/company"
	"smart-credit-backend/internal/credit"
	"smart-credit-backend/internal/scoring"
)

type Engine struct{}

func NewEngine() *Engine { return &Engine{} }

// Evaluate is deterministic: it only uses committed input, including SubmittedAt
// for business age. Malformed persisted input is a technical error, not refusal.
func (e *Engine) Evaluate(input Input) (Result, error) {
	data, err := decodeInput(input)
	if err != nil {
		return Result{}, ErrInvalidData
	}
	result := Result{Score: data.response.Score, RiskLevel: riskLevel(data.response.Score),
		Reasons: []string{}, ManualReviewReasons: []Reason{}, PolicyVersion: PolicyVersion}
	finish := func(status application.Status, reasons ...string) (Result, error) {
		result.Status, result.Reasons = status, reasons
		if err := ValidateResult(result); err != nil {
			return Result{}, err
		}
		return result, nil
	}
	product := data.product
	facts := company.RegistrationChecks(data.snapshot.Company.Profile, data.snapshot.Company.RegistrationStatus, input.SubmittedAt)
	critical := criticalGrounds(data, input.SubmittedAt)
	if len(critical) != 0 {
		return finish(application.Rejected, critical...)
	}
	registered, dateErr := time.Parse(time.DateOnly, data.snapshot.Company.Profile.RegistrationDate)

	manual := checkAmbiguities(data.prechecks.CompanyCheck)
	if profileConflict(data.prechecks.CompanyCheck, facts) {
		manual = append(manual, Reason{"company_profile_conflict", "Сохранённые проверки не подтверждают регистрационные данные компании на момент отправки заявки."})
	}
	if dateErr != nil || registered.After(input.SubmittedAt.UTC()) {
		manual = append(manual, Reason{"registration_date_ambiguous", "Не удалось подтвердить дату регистрации компании на момент отправки заявки."})
	}
	if result.RiskLevel == "MEDIUM" {
		manual = append(manual, Reason{"medium_risk", "Средний кредитный риск требует решения сотрудника банка."})
	}
	if data.request.OverdueCount > 0 {
		manual = append(manual, Reason{"credit_history_overdue", "В кредитной истории имеются просрочки."})
	}
	if conflictingFacts(data.request, data.response) {
		manual = append(manual, Reason{"scoring_facts_conflict", "Факторы скоринга противоречат сохранённым финансовым данным."})
	}
	if conflictingRecommendation(data.request, data.response) {
		manual = append(manual, Reason{"scoring_recommendation_conflict", "Рекомендация скоринга содержит противоречивые сумму или решение."})
	}
	budget := credit.PaymentBudget(data.request.MonthlyRevenue, data.request.MonthlyExpenses, data.request.MonthlyDebtPayments)
	payment, err := credit.MonthlyPayment(data.request.RequestedAmount, product.AnnualRatePercent, data.request.TermMonths)
	if err != nil {
		return Result{}, ErrInvalidData
	}
	affordable := payment <= budget
	var available int64
	if !affordable {
		available, err = credit.AvailableAmount(budget, product.AnnualRatePercent, data.request.TermMonths, product.MaxAmountRub)
		if err != nil {
			return Result{}, ErrInvalidData
		}
		// An explicit LOW-risk affordability refusal takes precedence over an
		// advisory scoring conflict. MEDIUM still requires an employee review.
		if result.RiskLevel == "LOW" && available < product.MinAmountRub {
			return finish(application.Rejected, "Финансовый бюджет не позволяет обслуживать минимальную сумму выбранного продукта.")
		}
	}
	if data.response.Decision == "rejected" || (affordable && data.response.Decision != "approved") {
		manual = append(manual, Reason{"scoring_policy_conflict", "Рекомендация скоринга не согласуется с автоматическим одобрением по кредитной политике."})
	}
	if len(manual) > 0 {
		result.ManualReviewReasons = manual
		reasons := make([]string, len(manual))
		for i, reason := range manual {
			reasons[i] = reason.Message
		}
		return finish(application.ManualReview, reasons...)
	}
	if affordable {
		result.Offer = newOffer(product, data.request.RequestedAmount, data.request.TermMonths, payment)
		return finish(application.Approved, "Компания прошла обязательные проверки.", "Низкий кредитный риск и допустимая финансовая нагрузка.")
	}
	if available < product.MinAmountRub {
		return finish(application.Rejected, "Финансовый бюджет не позволяет обслуживать минимальную сумму выбранного продукта.")
	}
	amount := min(available, data.request.RequestedAmount, data.response.ApprovedAmount)
	if amount < product.MinAmountRub {
		result.ManualReviewReasons = []Reason{{"scoring_amount_below_product_minimum", "Рекомендованная скорингом сумма ниже минимальной суммы продукта."}}
		return finish(application.ManualReview, result.ManualReviewReasons[0].Message)
	}
	if amount <= 0 || amount >= data.request.RequestedAmount {
		return Result{}, ErrInvalidData
	}
	payment, err = credit.MonthlyPayment(amount, product.AnnualRatePercent, data.request.TermMonths)
	if err != nil || payment > budget {
		return Result{}, ErrInvalidData
	}
	result.Offer = newOffer(product, amount, data.request.TermMonths, payment)
	return finish(application.CounterOffer, "Исходная сумма превышает допустимую финансовую нагрузку.", "Предложена меньшая сумма в пределах продукта и доступного бюджета.")
}

// Critical grounds are shared by automatic policy and manual proposals so an
// employee can resolve advisory uncertainty without bypassing mandatory rules.
func criticalGrounds(data evaluationData, submittedAt time.Time) []string {
	fields, product := data.snapshot.Fields, data.product
	facts := company.RegistrationChecks(data.snapshot.Company.Profile, data.snapshot.Company.RegistrationStatus, submittedAt)
	critical := []string{}
	if riskLevel(data.response.Score) == "HIGH" {
		critical = append(critical, "Высокий кредитный риск по демонстрационной кредитной политике.")
	}
	// These are mandatory lending constraints, not an implementation of the
	// external risk model. Use saved facts rather than rounded model factors.
	request := data.request
	flow := request.MonthlyRevenue - request.MonthlyExpenses
	if request.OverdueCount >= 3 {
		critical = append(critical, "Три или более просрочки в кредитной истории.")
	}
	if flow <= 0 {
		critical = append(critical, "Операционные расходы не покрываются месячной выручкой.")
	} else if flow*10 < request.MonthlyRevenue {
		critical = append(critical, "Операционная маржа ниже обязательного минимума 10%.")
	}
	if request.MonthlyRevenue > 0 && request.ExistingDebt >= request.MonthlyRevenue*18 {
		critical = append(critical, "Текущий долг составляет не менее 150% годовой выручки.")
	}
	for _, fact := range facts {
		if !fact.Passed && criticalRegistrationCheck(fact.Code) {
			critical = append(critical, fact.Message)
		}
	}
	for _, check := range data.prechecks.CompanyCheck.Checks {
		if !check.Passed && criticalRegistrationCheck(check.Code) {
			critical = append(critical, "Не пройдена обязательная проверка регистрационных сведений компании: "+check.Code+".")
		}
	}
	registered, dateErr := time.Parse(time.DateOnly, data.snapshot.Company.Profile.RegistrationDate)
	if dateErr == nil && !registered.After(submittedAt.UTC()) && credit.BusinessAgeMonths(registered, submittedAt) < product.MinBusinessAgeMonths {
		critical = append(critical, "Возраст бизнеса меньше минимального возраста для продукта.")
	}
	if !slices.Contains(product.Purposes, *fields.Purpose) {
		critical = append(critical, "Цель финансирования не поддерживается выбранным продуктом.")
	}
	if *fields.RequestedAmountRub < product.MinAmountRub || *fields.RequestedAmountRub > product.MaxAmountRub {
		critical = append(critical, "Запрошенная сумма находится вне границ выбранного продукта.")
	}
	if *fields.TermMonths < product.MinTermMonths || *fields.TermMonths > product.MaxTermMonths {
		critical = append(critical, "Запрошенный срок находится вне границ выбранного продукта.")
	}
	return critical
}

func newOffer(product credit.Product, amount int64, term int, payment int64) *Offer {
	total := payment * int64(term)
	return &Offer{ProductID: product.ID, AmountRub: amount, TermMonths: term, AnnualRatePercent: product.AnnualRatePercent,
		MonthlyPaymentRub: payment, TotalPaymentRub: total, OverpaymentRub: total - amount}
}

func riskLevel(score int) string {
	if score >= 80 {
		return "LOW"
	}
	if score >= 50 {
		return "MEDIUM"
	}
	return "HIGH"
}

func conflictingRecommendation(request scoring.Request, response scoring.Response) bool {
	switch response.Decision {
	case "approved":
		return response.ApprovedAmount != request.RequestedAmount
	case "counter_offer":
		return response.ApprovedAmount <= 0 || response.ApprovedAmount >= request.RequestedAmount
	case "rejected":
		return response.ApprovedAmount != 0
	default:
		return true
	}
}

func conflictingFacts(request scoring.Request, response scoring.Response) bool {
	expected := float64(request.MonthlyRevenue - request.MonthlyExpenses)
	tolerance := max(1.0, math.Abs(expected)*1e-9)
	// Rounded model ratios and the illustrative 20% payment never define the
	// product rate or replace the backend's calculations from saved facts.
	return response.Factors.OverdueCount != request.OverdueCount || math.Abs(response.Factors.OperatingCashFlow-expected) > tolerance
}

func criticalRegistrationCheck(code string) bool {
	return code == "inn_format" || code == "registration_number_format" || code == "registration_active"
}

func profileConflict(check companyCheck, facts []company.RegistrationCheck) bool {
	for _, fact := range facts {
		if !fact.Passed {
			return true
		}
		for _, saved := range check.Checks {
			if saved.Code == fact.Code && saved.Passed != fact.Passed {
				return true
			}
		}
	}
	return false
}

func checkAmbiguities(check companyCheck) []Reason {
	expected := []string{"company_name", "organization_type", "inn_format", "registration_number_format", "registration_date", "okved_format", "director_name", "registration_active"}
	seen, failed := map[string]bool{}, map[string]bool{}
	ambiguous := check.Status != "passed" && check.Status != "failed"
	for _, item := range check.Checks {
		if seen[item.Code] || !slices.Contains(expected, item.Code) {
			ambiguous = true
		}
		seen[item.Code] = true
		if !item.Passed {
			failed[item.Code] = true
		}
	}
	for _, name := range expected {
		if !seen[name] {
			ambiguous = true
		}
	}
	if (check.Status == "passed" && len(failed) > 0) || (check.Status == "failed" && len(failed) == 0) {
		ambiguous = true
	}
	problems := map[string]bool{}
	for _, problem := range check.Problems {
		if problems[problem.Code] || !failed[problem.Code] || strings.TrimSpace(problem.Message) == "" {
			ambiguous = true
		}
		problems[problem.Code] = true
	}
	for code := range failed {
		if !problems[code] {
			ambiguous = true
		}
	}
	if ambiguous || len(failed) > 0 {
		return []Reason{{"company_check_ambiguous", "Результаты проверки компании требуют уточнения сотрудником банка."}}
	}
	return []Reason{}
}

// ValidateResult guards the internal persistence boundary without re-running
// policy or accepting a fabricated final decision for manual review.
func ValidateResult(result Result) error {
	if (result.PolicyVersion != PolicyVersion && result.PolicyVersion != "go-demo-v1") || result.Score < 0 || result.Score > 100 || result.RiskLevel != riskLevel(result.Score) || len(result.Reasons) == 0 || result.ManualReviewReasons == nil {
		return ErrInvalidData
	}
	for _, reason := range result.Reasons {
		if strings.TrimSpace(reason) == "" {
			return ErrInvalidData
		}
	}
	for _, reason := range result.ManualReviewReasons {
		if strings.TrimSpace(reason.Code) == "" || strings.TrimSpace(reason.Message) == "" {
			return ErrInvalidData
		}
	}
	switch result.Status {
	case application.ManualReview:
		if result.Offer != nil || len(result.ManualReviewReasons) == 0 || result.RiskLevel == "HIGH" {
			return ErrInvalidData
		}
		return nil
	case application.Rejected:
		if result.Offer != nil || len(result.ManualReviewReasons) != 0 {
			return ErrInvalidData
		}
		return nil
	case application.Approved, application.CounterOffer:
		if result.Offer == nil || result.RiskLevel != "LOW" || len(result.ManualReviewReasons) != 0 {
			return ErrInvalidData
		}
	default:
		return ErrInvalidData
	}
	offer := result.Offer
	payment, err := credit.MonthlyPayment(offer.AmountRub, offer.AnnualRatePercent, offer.TermMonths)
	if err != nil || strings.TrimSpace(offer.ProductID) == "" || offer.AmountRub <= 0 || offer.MonthlyPaymentRub != payment || offer.TotalPaymentRub != payment*int64(offer.TermMonths) || offer.OverpaymentRub != offer.TotalPaymentRub-offer.AmountRub || offer.OverpaymentRub < 0 {
		return ErrInvalidData
	}
	return nil
}
