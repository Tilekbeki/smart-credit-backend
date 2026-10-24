package postgres

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"smart-credit-backend/internal/application"
	"smart-credit-backend/internal/decision"
	"smart-credit-backend/internal/lending"
	"smart-credit-backend/internal/pipeline"
	"smart-credit-backend/internal/review"
	"smart-credit-backend/internal/scoring"
)

type LendingRepository struct {
	pool    *pgxpool.Pool
	timeout time.Duration
}

var _ lending.Repository = (*LendingRepository)(nil)

func NewLendingRepository(pool *pgxpool.Pool, timeout time.Duration) *LendingRepository {
	return &LendingRepository{pool: pool, timeout: timeout}
}

type lockedLendingApplication struct {
	ownerID            string
	companyID          string
	status             application.Status
	version            int64
	submittedAt        *time.Time
	productID          *string
	financial, product json.RawMessage
}

func (r *LendingRepository) Execute(ctx context.Context, c lending.Command) (lending.Response, error) {
	if !validUUID(c.ApplicationID) || !validUUID(c.ActorID) || !validUUID(c.Key) || c.Version < 1 ||
		(c.Operation != lending.Manual && c.Operation != lending.Accept && c.Operation != lending.Decline) {
		return lending.Response{}, application.ErrInvalidInput
	}
	if hash, err := hex.DecodeString(c.RequestHash); err != nil || len(hash) != 32 {
		return lending.Response{}, application.ErrInvalidInput
	}
	ctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return lending.Response{}, application.ErrStorage
	}
	defer tx.Rollback(ctx)
	a, err := lockLendingApplication(ctx, tx, c)
	if err != nil {
		return lending.Response{}, err
	}
	op := c.Operation + ":" + c.ApplicationID
	// The application lock is always acquired before any idempotency or offer
	// lock. Access is rechecked before replay, including for already signed apps.
	replay, err := beginLendingKey(ctx, tx, c, op)
	if err != nil {
		return lending.Response{}, err
	}
	if replay != nil {
		if tx.Commit(ctx) != nil {
			return lending.Response{}, application.ErrStorage
		}
		return *replay, nil
	}
	status := 200
	if c.Operation == lending.Manual {
		status = 201
		err = manualLendingDecision(ctx, tx, c, a)
	} else {
		err = actOnLendingOffer(ctx, tx, c, a)
	}
	if err != nil {
		return lending.Response{}, err
	}
	var scope *application.Scope
	if c.Operation != lending.Manual {
		scope = &application.Scope{UserID: c.ActorID, CompanyID: c.CompanyID}
	}
	record, err := readReview(ctx, tx, scope, c.ApplicationID)
	if err != nil {
		return lending.Response{}, err
	}
	view, err := review.DecisionView(record)
	if err != nil {
		return lending.Response{}, err
	}
	body, err := json.Marshal(view)
	if err != nil {
		return lending.Response{}, application.ErrStorage
	}
	response := lending.Response{Status: status}
	// Return PostgreSQL's JSONB representation on the first request as well as
	// replays, preserving exactly the same wire body despite JSONB key ordering.
	err = tx.QueryRow(ctx, `UPDATE idempotency_keys SET status='completed',response_status=$4,response_body=$5::jsonb,updated_at=clock_timestamp()
	 WHERE user_id=$1 AND operation=$2 AND key=$3 RETURNING response_body`, c.ActorID, op, c.Key, status, body).Scan(&response.Body)
	if err != nil {
		return lending.Response{}, application.ErrStorage
	}
	if tx.Commit(ctx) != nil {
		return lending.Response{}, application.ErrStorage
	}
	return response, nil
}

func lockLendingApplication(ctx context.Context, tx pgx.Tx, c lending.Command) (lockedLendingApplication, error) {
	var a lockedLendingApplication
	err := tx.QueryRow(ctx, `SELECT a.owner_id::text,a.company_id::text,a.status,a.version,a.submitted_at,a.product_id::text,a.financial_snapshot,a.product_snapshot
	 FROM applications a WHERE a.id=$1 AND a.deleted_at IS NULL AND (
	 ($2='manual' AND a.status<>'draft' AND a.submitted_at IS NOT NULL
	  AND EXISTS(SELECT 1 FROM users WHERE id=$3 AND role='BANK_EMPLOYEE')) OR
	 ($2 IN ('accept','decline') AND a.owner_id=$3 AND a.company_id=$4::uuid
	  AND EXISTS(SELECT 1 FROM users WHERE id=$3 AND role='CLIENT' AND company_id=$4::uuid))) FOR UPDATE OF a`,
		c.ApplicationID, c.Operation, c.ActorID, nullableCompany(c.CompanyID)).Scan(&a.ownerID, &a.companyID, &a.status, &a.version, &a.submittedAt, &a.productID, &a.financial, &a.product)
	if errors.Is(err, pgx.ErrNoRows) {
		return a, application.ErrNotFound
	}
	if err != nil {
		return a, application.ErrStorage
	}
	return a, nil
}
func nullableCompany(id string) any {
	if id == "" {
		return nil
	}
	return id
}

func beginLendingKey(ctx context.Context, tx pgx.Tx, c lending.Command, op string) (*lending.Response, error) {
	if _, err := tx.Exec(ctx, `DELETE FROM idempotency_keys WHERE user_id=$1 AND operation=$2 AND key=$3 AND expires_at<=clock_timestamp()`, c.ActorID, op, c.Key); err != nil {
		return nil, application.ErrStorage
	}
	var hash, status string
	var code *int
	var body json.RawMessage
	err := tx.QueryRow(ctx, `SELECT request_hash,status,response_status,response_body FROM idempotency_keys WHERE user_id=$1 AND operation=$2 AND key=$3 FOR UPDATE`, c.ActorID, op, c.Key).Scan(&hash, &status, &code, &body)
	if err == nil {
		if hash != c.RequestHash {
			return nil, lending.ErrIdempotencyConflict
		}
		if status != "completed" || code == nil || len(body) == 0 {
			return nil, application.ErrStorage
		}
		return &lending.Response{Status: *code, Body: body}, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return nil, application.ErrStorage
	}
	if _, err := tx.Exec(ctx, `INSERT INTO idempotency_keys(user_id,operation,key,request_hash,status,created_at,expires_at)
	 VALUES($1,$2,$3,$4,'processing',statement_timestamp(),statement_timestamp()+interval '24 hours')`, c.ActorID, op, c.Key, c.RequestHash); err != nil {
		return nil, application.ErrStorage
	}
	return nil, nil
}

func manualLendingDecision(ctx context.Context, tx pgx.Tx, c lending.Command, a lockedLendingApplication) error {
	if a.status != application.ManualReview {
		return application.ErrInvalidStatus
	}
	if a.version != c.Version {
		return application.ErrVersionConflict
	}
	if a.submittedAt == nil || a.productID == nil {
		return application.ErrStorage
	}
	input := decision.Input{ApplicationID: c.ApplicationID, FinancialSnapshot: a.financial, ProductSnapshot: a.product, SubmittedAt: *a.submittedAt}
	var jobID, status, stage string
	var assessmentJSON []byte
	err := tx.QueryRow(ctx, `SELECT id::text,status,processing_stage,precheck_results,scoring_request,scoring_response,assessment_result
	 FROM jobs WHERE application_id=$1 AND type=$2`, c.ApplicationID, pipeline.JobType).
		Scan(&jobID, &status, &stage, &input.PrecheckResults, &input.ScoringRequest, &input.ScoringResponse, &assessmentJSON)
	if err != nil || status != "completed" || stage != "decision" {
		return application.ErrStorage
	}
	var assessment review.Assessment
	if json.Unmarshal(assessmentJSON, &assessment) != nil || assessment.SchemaVersion != 1 || assessment.Source != "automatic" ||
		assessment.AssessedAt.IsZero() || assessment.Result.Status != application.ManualReview || decision.ValidateResult(assessment.Result) != nil {
		return application.ErrStorage
	}
	var frozen pipeline.FinancialSnapshot
	response, decodeErr := scoring.DecodeResponse(input.ScoringResponse)
	if json.Unmarshal(a.financial, &frozen) != nil || frozen.UserID != a.ownerID || frozen.CompanyID != a.companyID ||
		decodeErr != nil || assessment.Result.Score != response.Score {
		return application.ErrStorage
	}
	amount, term := int64(0), 0
	if c.Offer != nil {
		amount, term = c.Offer.AmountRub, c.Offer.TermMonths
	}
	offer, err := decision.PrepareManual(input, c.Decision, amount, term)
	if errors.Is(err, decision.ErrManualTerms) {
		return errors.Join(lending.ErrValidation, err)
	}
	if err != nil {
		return application.ErrStorage
	}
	// The application carries an internal product UUID; policy and public DTOs
	// carry its frozen code. Never derive pricing from the mutable catalog row.
	if offer != nil {
		var code string
		if tx.QueryRow(ctx, `SELECT code FROM credit_products WHERE id=$1`, *a.productID).Scan(&code) != nil || code != offer.ProductID {
			return application.ErrStorage
		}
	}
	var exists bool
	if tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM offers WHERE application_id=$1 AND (status='active' OR status='accepted'))`, c.ApplicationID).Scan(&exists) != nil || exists {
		return application.ErrStorage
	}
	var decidedAt time.Time
	if tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&decidedAt) != nil {
		return application.ErrStorage
	}
	explanation := c.CustomerExplanation
	if explanation == "" {
		explanation = manualPublicReasons(c.Decision)[0]
	}
	reasons, _ := json.Marshal([]string{explanation})
	var decisionID string
	err = tx.QueryRow(ctx, `INSERT INTO credit_decisions(application_id,version,source,outcome,risk_category,approved_amount,
 reasons,policy_version,scoring_job_id,decided_by,internal_comment,created_at,customer_explanation)
 SELECT $1,COALESCE(max(version),0)+1,'MANUAL',$2,$3,$4,$5,$6,$7,$8,$9,$10,$11 FROM credit_decisions WHERE application_id=$1 RETURNING id::text`,
		c.ApplicationID, c.Decision, assessment.Result.RiskLevel, amount, reasons, decision.PolicyVersion, jobID, c.ActorID, c.Comment, decidedAt, explanation).Scan(&decisionID)
	if err != nil {
		return application.ErrStorage
	}
	var offerID *string
	if offer != nil {
		var id string
		err = tx.QueryRow(ctx, `INSERT INTO offers(application_id,decision_id,product_id,version,is_primary,status,amount,term_months,
		 annual_rate_percent,monthly_payment,total_payment,overpayment,created_at)
		 SELECT $1,$2,$3,COALESCE(max(version),0)+1,true,'active',$4,$5,$6::numeric,$7,$8,$9,$10 FROM offers WHERE application_id=$1 RETURNING id::text`,
			c.ApplicationID, decisionID, *a.productID, offer.AmountRub, offer.TermMonths, offer.AnnualRatePercent.String(), offer.MonthlyPaymentRub, offer.TotalPaymentRub, offer.OverpaymentRub, decidedAt).Scan(&id)
		if err != nil {
			return application.ErrStorage
		}
		offerID = &id
	}
	if _, err := tx.Exec(ctx, `UPDATE applications SET status=$2,version=version+1,processing_error=NULL,updated_at=$3 WHERE id=$1`, c.ApplicationID, c.Decision, decidedAt); err != nil {
		return application.ErrStorage
	}
	kind, title := manualEvent(c.Decision)
	return appendLendingEvent(ctx, tx, c, kind, title, c.Decision, offerID, decidedAt)
}

func manualPublicReasons(outcome string) []string {
	switch outcome {
	case "approved":
		return []string{"Исходные условия кредита одобрены сотрудником банка."}
	case "counter_offer":
		return []string{"Сотрудник банка предложил альтернативные условия кредита."}
	default:
		return []string{"По результатам рассмотрения сотрудником банка принято решение об отказе."}
	}
}
func manualEvent(outcome string) (string, string) {
	switch outcome {
	case "approved":
		return "MANUAL_DECISION_APPROVED", "Сотрудник банка одобрил заявку"
	case "counter_offer":
		return "MANUAL_COUNTER_OFFER_CREATED", "Сотрудник банка предложил альтернативные условия"
	default:
		return "MANUAL_DECISION_REJECTED", "Сотрудник банка отказал по заявке"
	}
}

func actOnLendingOffer(ctx context.Context, tx pgx.Tx, c lending.Command, a lockedLendingApplication) error {
	if a.status != application.Approved && a.status != application.CounterOffer && a.status != application.Signed && a.status != application.Closed {
		return application.ErrInvalidStatus
	}
	var currentID, decisionID, offerStatus string
	var primary bool
	err := tx.QueryRow(ctx, `SELECT o.id::text,o.decision_id::text,o.status,o.is_primary FROM offers o
	 WHERE o.application_id=$1 AND o.id=$2 AND o.decision_id=(SELECT id FROM credit_decisions WHERE application_id=$1 ORDER BY version DESC LIMIT 1)
	 FOR UPDATE OF o`, c.ApplicationID, c.OfferID).Scan(&currentID, &decisionID, &offerStatus, &primary)
	if errors.Is(err, pgx.ErrNoRows) {
		return lending.ErrOfferNotActive
	}
	if err != nil {
		return application.ErrStorage
	}
	if !primary {
		return lending.ErrOfferNotActive
	}
	targetStatus, targetOffer, kind, title := "signed", "accepted", "OFFER_ACCEPTED", "Предложение подтверждено предпринимателем"
	if c.Operation == lending.Decline {
		targetStatus, targetOffer, kind, title = "closed", "withdrawn", "OFFER_DECLINED", "Предложение отклонено предпринимателем"
	}
	if string(a.status) == targetStatus && offerStatus == targetOffer {
		// The event binds actor, exact offer, action and original version. This
		// durable evidence makes a retry safe after key expiry or a new key.
		var version int64
		err := tx.QueryRow(ctx, `SELECT action_version FROM application_events WHERE application_id=$1 AND actor_id=$2
		 AND offer_id=$3 AND action=$4 AND event_type=$5`, c.ApplicationID, c.ActorID, c.OfferID, c.Operation, kind).Scan(&version)
		if errors.Is(err, pgx.ErrNoRows) {
			return application.ErrInvalidStatus
		}
		if err != nil {
			return application.ErrStorage
		}
		if a.version != version+1 || (c.Version != version && c.Version != a.version) {
			return application.ErrVersionConflict
		}
		return nil
	}
	if a.status != application.Approved && a.status != application.CounterOffer {
		return application.ErrInvalidStatus
	}
	if a.version != c.Version {
		return application.ErrVersionConflict
	}
	if offerStatus != "active" {
		return lending.ErrOfferNotActive
	}
	var now time.Time
	if tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&now) != nil {
		return application.ErrStorage
	}
	if _, err := tx.Exec(ctx, `UPDATE offers SET status=$2 WHERE id=$1`, c.OfferID, targetOffer); err != nil {
		return application.ErrStorage
	}
	if c.Operation == lending.Accept {
		_, err = tx.Exec(ctx, `UPDATE applications SET status='signed',accepted_offer_id=$2,signed_by=$3,signed_at=$4,
		 version=version+1,updated_at=$4 WHERE id=$1`, c.ApplicationID, c.OfferID, c.ActorID, now)
	} else {
		_, err = tx.Exec(ctx, `UPDATE applications SET status='closed',version=version+1,updated_at=$2 WHERE id=$1`, c.ApplicationID, now)
	}
	if err != nil {
		return application.ErrStorage
	}
	return appendLendingEvent(ctx, tx, c, kind, title, targetStatus, &currentID, now)
}

func appendLendingEvent(ctx context.Context, tx pgx.Tx, c lending.Command, kind, title, status string, offerID *string, at time.Time) error {
	_, err := tx.Exec(ctx, `INSERT INTO application_events(application_id,sequence,event_type,status,title,actor_id,offer_id,action,action_version,created_at)
	 SELECT $1,COALESCE(max(sequence),0)+1,$2,$3,$4,$5,$6,$7,$8,$9 FROM application_events WHERE application_id=$1`,
		c.ApplicationID, kind, status, title, c.ActorID, offerID, c.Operation, c.Version, at)
	if err != nil {
		return application.ErrStorage
	}
	return nil
}
