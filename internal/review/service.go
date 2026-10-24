package review

import (
	"context"
	"encoding/json"
	"slices"

	"github.com/google/uuid"
	"smart-credit-backend/internal/application"
	"smart-credit-backend/internal/credit"
	"smart-credit-backend/internal/decision"
	"smart-credit-backend/internal/pipeline"
)

type Service struct{ repo Repository }

func NewService(repo Repository) *Service { return &Service{repo: repo} }

func ValidFilter(f Filter) bool {
	if f.Limit < 1 || f.Limit > 100 || f.Offset < 0 || f.Offset > 2147483647 || (f.Status != nil && (*f.Status == application.Draft || !application.ValidStatus(*f.Status))) {
		return false
	}
	if f.CompanyID != nil {
		id, err := uuid.Parse(*f.CompanyID)
		if err != nil || len(*f.CompanyID) != 36 || id == uuid.Nil {
			return false
		}
	}
	return true
}

func (s *Service) Decision(ctx context.Context, scope application.Scope, id string) (CreditDecision, error) {
	r, err := s.repo.Read(ctx, &scope, id)
	if err != nil {
		return CreditDecision{}, err
	}
	return DecisionView(r)
}

// DecisionView maps one committed or transaction-local snapshot to the public
// decision DTO. Mutations use this same mapping when saving their replay body.
func DecisionView(r Record) (CreditDecision, error) {
	a := r.Application
	v := CreditDecision{ApplicationID: a.ApplicationID, Status: a.Status, Version: a.Version, Reasons: []string{}, Processing: processing(r)}
	if r.Job != nil {
		if r.Job.Scoring != nil && r.Job.ScoringAt != nil {
			score := r.Job.Scoring.Score
			v.Score = &score
		}
		if assessment := r.Job.Assessment; assessment != nil {
			if !validAssessment(assessment) {
				return CreditDecision{}, application.ErrStorage
			}
			v.Score = &assessment.Result.Score
			v.RiskLevel = &assessment.Result.RiskLevel
			if a.Status == application.ManualReview {
				v.Reasons = assessment.Result.Reasons
			}
		}
	}
	// Pending states never masquerade as a final decision, even if inconsistent
	// legacy artifacts exist. signed/closed preserve the last real decision.
	if a.Status != application.Draft && a.Status != application.Submitted && a.Status != application.Processing && a.Status != application.ManualReview && r.Final != nil {
		v.Decision = &r.Final.Outcome
		v.Reasons = r.Final.Reasons
		v.AssessedAt = &r.Final.AssessedAt
		v.ReviewedBy = r.Final.ReviewedBy
		v.RiskLevel = r.Final.RiskLevel
		v.CustomerExplanation = r.Final.CustomerExplanation
		if r.Final.Outcome != "rejected" {
			v.Offer = r.Final.Offer
		}
	}
	if a.Status == application.Draft {
		v.Score = nil
		v.RiskLevel = nil
	}
	if a.Status == application.ManualReview && len(v.Reasons) == 0 {
		v.Reasons = []string{"Заявка передана на ручное рассмотрение сотруднику банка."}
	}
	return v, nil
}

func bankActions(status application.Status) []string {
	if status == application.ManualReview {
		return []string{"decide"}
	}
	return []string{}
}

func frozen(r Record) (pipeline.FinancialSnapshot, credit.Product, error) {
	var snapshot pipeline.FinancialSnapshot
	var product credit.Product
	if r.Application.Status == application.Draft {
		return snapshot, product, application.ErrNotFound
	}
	if json.Unmarshal(r.FinancialSnapshot, &snapshot) != nil || json.Unmarshal(r.ProductSnapshot, &product) != nil ||
		snapshot.SchemaVersion != 1 || snapshot.ApplicationID != r.Application.ApplicationID || snapshot.UserID != r.Application.UserID ||
		snapshot.CompanyID != r.Application.CompanyID || snapshot.Company.Profile.ID != snapshot.CompanyID ||
		snapshot.Fields.ProductID == nil || *snapshot.Fields.ProductID != product.ID || product.ID == "" || product.Name == "" ||
		snapshot.Fields.RequestedAmountRub == nil || snapshot.Fields.TermMonths == nil || snapshot.Fields.Purpose == nil ||
		snapshot.Fields.MonthlyRevenueRub == nil || snapshot.Fields.MonthlyExpensesRub == nil || r.Application.SubmittedAt == nil {
		return snapshot, product, application.ErrStorage
	}
	return snapshot, product, nil
}

func (s *Service) BankDetail(ctx context.Context, id string) (BankDetail, error) {
	r, err := s.repo.Read(ctx, nil, id)
	if err != nil {
		return BankDetail{}, err
	}
	snapshot, product, err := frozen(r)
	if err != nil {
		return BankDetail{}, err
	}
	a := r.Application
	a.Fields = snapshot.Fields
	a.ProductName = &product.Name
	a.AvailableActions = bankActions(a.Status)
	return BankDetail{Application: a, Company: snapshot.Company.Profile, Product: application.ProductSummary{ID: product.ID, Name: product.Name}, ProductTerms: product, ManualReviewRequired: a.Status == application.ManualReview}, nil
}

func (s *Service) BankList(ctx context.Context, f Filter) (BankPage, error) {
	if !ValidFilter(f) {
		return BankPage{}, application.ErrInvalidInput
	}
	records, total, err := s.repo.List(ctx, f)
	if err != nil {
		return BankPage{}, err
	}
	page := BankPage{Items: []BankApplication{}, Total: total, Limit: f.Limit, Offset: f.Offset}
	for _, r := range records {
		snapshot, product, err := frozen(r)
		if err != nil {
			return BankPage{}, err
		}
		a, c := r.Application, snapshot.Company.Profile
		page.Items = append(page.Items, BankApplication{ApplicationID: a.ApplicationID, Company: CompanySummary{ID: c.ID, Name: c.Name, INN: c.INN}, Product: application.ProductSummary{ID: product.ID, Name: product.Name}, RequestedAmountRub: *snapshot.Fields.RequestedAmountRub, TermMonths: *snapshot.Fields.TermMonths, Status: a.Status, Version: a.Version, CreatedAt: a.CreatedAt, UpdatedAt: a.UpdatedAt, SubmittedAt: *a.SubmittedAt, ManualReviewRequired: a.Status == application.ManualReview, AvailableActions: bankActions(a.Status)})
	}
	return page, nil
}

func (s *Service) BankAssessment(ctx context.Context, id string) (BankAssessment, error) {
	r, err := s.repo.Read(ctx, nil, id)
	if err != nil {
		return BankAssessment{}, err
	}
	if r.Application.Status == application.Draft {
		return BankAssessment{}, application.ErrNotFound
	}
	a := r.Application
	v := BankAssessment{ApplicationID: a.ApplicationID, Status: a.Status, Version: a.Version, ManualReviewReasons: []decision.Reason{}, Recommendations: []Recommendation{}, Processing: processing(r)}
	if r.Final != nil {
		v.ManualDecision = r.Final.ManualDecision
	}
	if r.Job == nil {
		return v, nil
	}
	j := r.Job
	if j.Prechecks != nil {
		v.CompanyCheck = &j.Prechecks.CompanyCheck
		v.CreditHistory = &j.Prechecks.CreditHistory
	}
	if j.Scoring != nil && j.ScoringAt != nil {
		v.Scoring = &ScoringAssessment{Score: j.Scoring.Score, AssessedAt: *j.ScoringAt,
			Decision: j.Scoring.Decision, ApprovedAmountRub: j.Scoring.ApprovedAmount,
			Reasons: j.Scoring.Reasons, Factors: &j.Scoring.Factors}
	}
	if j.Assessment != nil {
		result := j.Assessment.Result
		if !validAssessment(j.Assessment) {
			return BankAssessment{}, application.ErrStorage
		}
		v.RiskLevel = &result.RiskLevel
		v.GoDecision = &GoDecision{Decision: result.Status, Score: result.Score, RiskLevel: result.RiskLevel,
			Reasons: result.Reasons, AssessedAt: j.Assessment.AssessedAt}
		if a.Status == application.ManualReview {
			v.ManualReviewReasons = result.ManualReviewReasons
		}
		if result.Status != application.ManualReview {
			recommendation := Recommendation{Decision: string(result.Status), Reasons: result.Reasons}
			if result.Offer != nil {
				recommendation.Offer = &RecommendedOffer{AmountRub: result.Offer.AmountRub, TermMonths: result.Offer.TermMonths}
			}
			v.Recommendations = append(v.Recommendations, recommendation)
		}
	}
	return v, nil
}

// Reads validate the stored representation only. Risk thresholds, policy and
// annuity calculations belong to the writer and are never rerun by these GETs.
func validAssessment(a *Assessment) bool {
	return a.SchemaVersion == 1 && a.Source == "automatic" && !a.AssessedAt.IsZero() &&
		a.Result.Score >= 0 && a.Result.Score <= 100 &&
		slices.Contains([]string{"LOW", "MEDIUM", "HIGH"}, a.Result.RiskLevel) &&
		slices.Contains([]application.Status{application.Approved, application.Rejected, application.CounterOffer, application.ManualReview}, a.Result.Status) &&
		a.Result.Reasons != nil && a.Result.ManualReviewReasons != nil
}

func processing(r Record) *Processing {
	if r.Application.Status != application.Submitted && r.Application.Status != application.Processing {
		return nil
	}
	p := &Processing{Stage: "prechecks", State: "waiting"}
	message := "Заявка ожидает предварительной проверки."
	j := r.Job
	if j != nil {
		p.Stage = j.Stage
		if j.Status == pipeline.AwaitingDecision {
			p.Stage = "decision"
		}
		if j.Status == pipeline.AwaitingScoring {
			p.Stage = "scoring"
		}
		if p.Stage != "prechecks" && p.Stage != "scoring" && p.Stage != "decision" {
			p.Stage = "prechecks"
		}
		switch p.Stage {
		case "scoring":
			message = "Ожидается результат оценки."
		case "decision":
			message = "Ожидается кредитное решение."
		}
		switch {
		case j.Status == "failed" || j.Status == "completed":
			p.State = "failed"
			message = "Обработка временно недоступна. Требуется проверка сотрудником банка."
		case j.Status == "running" && j.LeaseUntil != nil && j.LeaseUntil.After(j.ReadAt):
			p.State = "running"
			message = "Выполняется обработка заявки."
		case j.HasError || j.Status == "running":
			p.State = "retrying"
			message = "Обработка будет повторена автоматически."
			if j.Status != "running" && j.AvailableAt.After(j.ReadAt) {
				retry := j.AvailableAt.UTC()
				p.RetryAt = &retry
			}
		}
	}
	p.Message = &message
	return p
}
