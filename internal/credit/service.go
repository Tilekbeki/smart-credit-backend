package credit

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"time"
)

type Service struct {
	products  Repository
	companies CompanyReader
	now       func() time.Time
}

func NewService(products Repository, companies CompanyReader) *Service {
	return &Service{products: products, companies: companies, now: time.Now}
}

func (s *Service) ListProducts(ctx context.Context) ([]Product, error) {
	products, err := s.products.ListProducts(ctx)
	if err != nil {
		return nil, ErrStorage
	}
	if products == nil {
		products = []Product{}
	}
	for _, product := range products {
		if !validProduct(product) {
			return nil, ErrStorage
		}
	}
	sort.Slice(products, func(i, j int) bool { return products[i].ID < products[j].ID })
	return products, nil
}

func (s *Service) Product(ctx context.Context, id string) (Product, error) {
	product, err := s.products.ProductByID(ctx, id)
	if errors.Is(err, ErrProductNotFound) {
		return Product{}, ErrProductNotFound
	}
	if err != nil || !validProduct(product) {
		return Product{}, ErrStorage
	}
	return product, nil
}

func (s *Service) Recommend(ctx context.Context, companyID string, input Input) (Result, error) {
	if err := validateInput(input); err != nil {
		return Result{}, err
	}
	if companyID == "" {
		return Result{}, ErrInvalidInput
	}
	company, err := s.companies.CompanyByID(ctx, companyID)
	if err != nil {
		return Result{}, ErrStorage
	}
	registered, err := time.Parse(time.DateOnly, company.RegistrationDate)
	if err != nil || company.Financials.ExistingMonthlyPaymentsRub < 0 || company.Financials.ExistingMonthlyPaymentsRub > maxInputAmount {
		return Result{}, ErrStorage
	}
	products, err := s.ListProducts(ctx)
	if err != nil {
		return Result{}, err
	}
	surplus := input.MonthlyRevenueRub - input.MonthlyExpensesRub
	budget := PaymentBudget(input.MonthlyRevenueRub, input.MonthlyExpensesRub, company.Financials.ExistingMonthlyPaymentsRub)
	result := Result{
		CompanyID:                  companyID,
		BusinessAgeMonths:          BusinessAgeMonths(registered, s.now()),
		SurplusRub:                 surplus,
		ExistingMonthlyPaymentsRub: company.Financials.ExistingMonthlyPaymentsRub,
		PaymentBudgetRub:           budget,
		Products:                   make([]ProductRecommendation, 0, len(products)),
		Disclaimer:                 "Предварительный подбор финансирования не является кредитным решением и не гарантирует одобрение. Условия синтетические и не являются действующими условиями Альфа-Банка.",
		Simulation:                 true,
	}
	var bestPayment int64
	for _, product := range products {
		recommendation, err := recommendProduct(product, input, result.BusinessAgeMonths, budget)
		if err != nil {
			return Result{}, ErrStorage
		}
		result.Products = append(result.Products, recommendation)
		// Products are sorted by ID, so equal payments retain a stable choice.
		if recommendation.Eligible && (result.RecommendedProductID == nil || *recommendation.MonthlyPaymentRub < bestPayment) {
			id := product.ID
			result.RecommendedProductID = &id
			bestPayment = *recommendation.MonthlyPaymentRub
		}
	}
	return result, nil
}

func validateInput(input Input) error {
	if !validPurpose(input.Purpose) {
		return fmt.Errorf("%w: purpose", ErrInvalidInput)
	}
	if input.RequestedAmountRub < 1 || input.RequestedAmountRub > maxInputAmount {
		return fmt.Errorf("%w: requestedAmountRub", ErrInvalidInput)
	}
	if input.TermMonths < 1 || input.TermMonths > 120 {
		return fmt.Errorf("%w: termMonths", ErrInvalidInput)
	}
	if input.MonthlyRevenueRub < 0 || input.MonthlyRevenueRub > maxInputAmount {
		return fmt.Errorf("%w: monthlyRevenueRub", ErrInvalidInput)
	}
	if input.MonthlyExpensesRub < 0 || input.MonthlyExpensesRub > maxInputAmount {
		return fmt.Errorf("%w: monthlyExpensesRub", ErrInvalidInput)
	}
	return nil
}

func validPurpose(value string) bool {
	return slices.Contains([]string{"stock", "expenses", "seasonal", "equipment", "upgrade", "other"}, value)
}

func validProduct(p Product) bool {
	if p.ID == "" || p.Name == "" || p.MinAmountRub < 1 || p.MaxAmountRub < p.MinAmountRub || p.MaxAmountRub > maxInputAmount || p.MinTermMonths < 1 || p.MaxTermMonths < p.MinTermMonths || p.MaxTermMonths > 120 || p.MinBusinessAgeMonths < 0 || p.Version < 1 || len(p.Purposes) == 0 {
		return false
	}
	if _, err := parseRate(p.AnnualRatePercent); err != nil {
		return false
	}
	for _, purpose := range p.Purposes {
		if !validPurpose(purpose) {
			return false
		}
	}
	return true
}

func recommendProduct(product Product, input Input, age int, budget int64) (ProductRecommendation, error) {
	result := ProductRecommendation{
		Product: product,
		Matches: Matches{
			Purpose:     slices.Contains(product.Purposes, input.Purpose),
			Amount:      input.RequestedAmountRub >= product.MinAmountRub && input.RequestedAmountRub <= product.MaxAmountRub,
			Term:        input.TermMonths >= product.MinTermMonths && input.TermMonths <= product.MaxTermMonths,
			BusinessAge: age >= product.MinBusinessAgeMonths,
		},
		Reasons:     []string{},
		Limitations: []string{},
		Adjustments: []Adjustment{},
	}
	if result.Matches.Purpose {
		result.Reasons = append(result.Reasons, "Продукт соответствует цели финансирования.")
	} else {
		result.Limitations = append(result.Limitations, "Цель финансирования не поддерживается продуктом.")
	}
	if result.Matches.Amount {
		result.Reasons = append(result.Reasons, "Сумма соответствует лимитам продукта.")
	} else {
		result.Limitations = append(result.Limitations, fmt.Sprintf("Запрашиваемая сумма должна быть от %d до %d рублей.", product.MinAmountRub, product.MaxAmountRub))
	}
	if result.Matches.Term {
		payment, err := MonthlyPayment(input.RequestedAmountRub, product.AnnualRatePercent, input.TermMonths)
		if err != nil {
			return ProductRecommendation{}, err
		}
		available, err := AvailableAmount(budget, product.AnnualRatePercent, input.TermMonths, product.MaxAmountRub)
		if err != nil {
			return ProductRecommendation{}, err
		}
		result.MonthlyPaymentRub = &payment
		result.PreliminaryAvailableAmountRub = available
		// Affordability describes the payment budget independently of product
		// limits; the Amount match already reports a principal above the cap.
		result.Matches.Affordability = payment <= budget
		result.Reasons = append(result.Reasons, "Срок соответствует условиям продукта.")
		if result.Matches.Affordability {
			result.Reasons = append(result.Reasons, "Ежемесячный платёж укладывается в рассчитанный бюджет.")
		} else if budget == 0 {
			result.Limitations = append(result.Limitations, "Нет свободного бюджета для обслуживания нового кредита.")
		} else {
			result.Limitations = append(result.Limitations, "Запрашиваемая сумма превышает предварительно доступную сумму.")
		}
		if available < product.MinAmountRub {
			result.Limitations = append(result.Limitations, "Предварительно доступная сумма ниже минимальной суммы продукта.")
		}
	} else {
		result.Limitations = append(result.Limitations, fmt.Sprintf("Срок должен быть от %d до %d месяцев. Доступность для запрошенного срока не рассчитывается.", product.MinTermMonths, product.MaxTermMonths))
	}
	if result.Matches.BusinessAge {
		result.Reasons = append(result.Reasons, "Возраст бизнеса соответствует условиям продукта.")
	} else {
		result.Limitations = append(result.Limitations, fmt.Sprintf("Возраст бизнеса — %d месяцев; требуется не менее %d месяцев.", age, product.MinBusinessAgeMonths))
	}
	result.Eligible = result.Matches.Purpose && result.Matches.Amount && result.Matches.Term && result.Matches.BusinessAge && result.Matches.Affordability
	if !result.Eligible && result.Matches.Purpose && result.Matches.BusinessAge {
		adjustment, err := possibleAdjustment(product, input, budget)
		if err != nil {
			return ProductRecommendation{}, err
		}
		if adjustment != nil {
			result.Adjustments = append(result.Adjustments, *adjustment)
		}
	}
	return result, nil
}

func possibleAdjustment(product Product, input Input, budget int64) (*Adjustment, error) {
	amount := min(product.MaxAmountRub, max(product.MinAmountRub, input.RequestedAmountRub))
	startTerm := min(product.MaxTermMonths, max(product.MinTermMonths, input.TermMonths))
	for term := startTerm; term <= product.MaxTermMonths; term++ {
		available, err := AvailableAmount(budget, product.AnnualRatePercent, term, product.MaxAmountRub)
		if err != nil {
			return nil, err
		}
		adjustedAmount := amount
		if available < amount {
			if term < product.MaxTermMonths {
				continue
			}
			adjustedAmount = available
		}
		if adjustedAmount < product.MinAmountRub {
			return nil, nil
		}
		payment, err := MonthlyPayment(adjustedAmount, product.AnnualRatePercent, term)
		if err != nil {
			return nil, err
		}
		return &Adjustment{
			AmountRub:         adjustedAmount,
			TermMonths:        term,
			MonthlyPaymentRub: payment,
			Explanation:       "Возможные условия после корректировки суммы или срока; предварительный расчёт не гарантирует одобрение.",
		}, nil
	}
	return nil, nil
}
