package postgres

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"github.com/jackc/pgx/v5"

	"smart-credit-backend/internal/company"
	"smart-credit-backend/internal/lending"
	"smart-credit-backend/internal/pipeline"
	"strings"
	"unicode/utf8"
)

func (q *PrecheckQueue) RecoverJob(ctx context.Context, c pipeline.RecoveryCommand) (pipeline.RecoveryResponse, error) {
	c.Reason = strings.TrimSpace(c.Reason)
	if !validUUID(c.ActorID) || !validUUID(c.ApplicationID) || !validUUID(c.Key) || c.Version < 1 || !utf8.ValidString(c.Reason) || len([]rune(c.Reason)) < 1 || len([]rune(c.Reason)) > 1000 || strings.ContainsRune(c.Reason, 0) {
		return pipeline.RecoveryResponse{}, company.ErrInvalidInput
	}
	ctx, cancel := context.WithTimeout(ctx, q.timeout)
	defer cancel()
	tx, err := q.pool.Begin(ctx)
	if err != nil {
		return pipeline.RecoveryResponse{}, company.ErrStorage
	}
	defer tx.Rollback(ctx)
	var bank bool
	if tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM users WHERE id=$1 AND role='BANK_EMPLOYEE')`, c.ActorID).Scan(&bank) != nil {
		return pipeline.RecoveryResponse{}, company.ErrStorage
	}
	if !bank {
		return pipeline.RecoveryResponse{}, company.ErrForbidden
	}
	// Same job -> application lock order as the worker. No live or expired running
	// lease is stolen by the operator: only durably failed jobs may be recovered.
	var jobID, status, stage string
	var prechecks, response, assessment json.RawMessage
	var count int
	err = tx.QueryRow(ctx, `SELECT id::text,status,processing_stage,precheck_results,scoring_response,assessment_result,recovery_count FROM jobs WHERE application_id=$1 AND type=$2 FOR UPDATE`, c.ApplicationID, pipeline.JobType).Scan(&jobID, &status, &stage, &prechecks, &response, &assessment, &count)
	if errors.Is(err, pgx.ErrNoRows) {
		return pipeline.RecoveryResponse{}, company.ErrNotFound
	}
	if err != nil {
		return pipeline.RecoveryResponse{}, company.ErrStorage
	}
	var version int64
	var appStatus, companyID string
	if tx.QueryRow(ctx, `SELECT version,status,company_id::text FROM applications WHERE id=$1 AND status<>'draft' AND submitted_at IS NOT NULL FOR UPDATE`, c.ApplicationID).Scan(&version, &appStatus, &companyID) != nil {
		return pipeline.RecoveryResponse{}, company.ErrNotFound
	}
	data, _ := json.Marshal(struct {
		Version int64  `json:"version"`
		Reason  string `json:"reason"`
	}{c.Version, c.Reason})
	hash := sha256.Sum256(data)
	key := lending.Command{ActorID: c.ActorID, Key: c.Key, RequestHash: hex.EncodeToString(hash[:])}
	op := "recover:" + c.ApplicationID
	replay, err := beginLendingKey(ctx, tx, key, op)
	if errors.Is(err, lending.ErrIdempotencyConflict) {
		return pipeline.RecoveryResponse{}, company.ErrIdempotencyConflict
	}
	if err != nil {
		return pipeline.RecoveryResponse{}, company.ErrStorage
	}
	if replay != nil {
		if tx.Commit(ctx) != nil {
			return pipeline.RecoveryResponse{}, company.ErrStorage
		}
		return pipeline.RecoveryResponse{Status: replay.Status, Body: replay.Body}, nil
	}
	if version != c.Version {
		return pipeline.RecoveryResponse{}, company.ErrVersionConflict
	}
	if status != "failed" || (appStatus != "processing" && appStatus != "submitted") || count >= 3 || len(assessment) > 0 {
		return pipeline.RecoveryResponse{}, company.ErrInvalidStatus
	}
	var artifacts bool
	if tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM credit_decisions WHERE application_id=$1) OR EXISTS(SELECT 1 FROM offers WHERE application_id=$1)`, c.ApplicationID).Scan(&artifacts) != nil {
		return pipeline.RecoveryResponse{}, company.ErrStorage
	}
	if artifacts {
		return pipeline.RecoveryResponse{}, company.ErrInvalidStatus
	}
	next := "pending"
	stage = "prechecks"
	if len(prechecks) > 0 {
		next = pipeline.AwaitingScoring
		stage = "scoring"
	}
	if len(response) > 0 {
		next = pipeline.AwaitingDecision
		stage = "decision"
	}
	// Only retry budgets expand; successful inputs and results remain untouched.
	if _, err = tx.Exec(ctx, `UPDATE jobs SET status=$2,processing_stage=$3,max_attempts=max_attempts+5,recovery_count=recovery_count+1,available_at=clock_timestamp(),finished_at=NULL,lease_token=NULL,lease_until=NULL,last_error=NULL,updated_at=clock_timestamp() WHERE id=$1`, jobID, next, stage); err != nil {
		return pipeline.RecoveryResponse{}, company.ErrStorage
	}
	if _, err = tx.Exec(ctx, `UPDATE applications SET processing_error=NULL,version=version+1,updated_at=clock_timestamp() WHERE id=$1`, c.ApplicationID); err != nil {
		return pipeline.RecoveryResponse{}, company.ErrStorage
	}
	audit := company.Command{ActorID: c.ActorID, CompanyID: companyID, Action: "retry_processing", Reason: c.Reason}
	old, _ := json.Marshal(map[string]any{"recoveryCount": count, "status": status})
	fresh, _ := json.Marshal(map[string]any{"recoveryCount": count + 1, "status": next, "stage": stage})
	if auditData(ctx, tx, audit, "job", jobID, old, fresh) != nil {
		return pipeline.RecoveryResponse{}, company.ErrStorage
	}
	if _, err = tx.Exec(ctx, `INSERT INTO application_events(application_id,sequence,event_type,status,title,actor_id) SELECT $1,COALESCE(max(sequence),0)+1,'PROCESSING_RECOVERY_REQUESTED',$2,'Сотрудник банка возобновил техническую обработку',$3 FROM application_events WHERE application_id=$1`, c.ApplicationID, appStatus, c.ActorID); err != nil {
		return pipeline.RecoveryResponse{}, company.ErrStorage
	}
	body, _ := json.Marshal(map[string]any{"applicationId": c.ApplicationID, "version": version + 1, "status": appStatus, "stage": stage, "recoveryCount": count + 1})
	if tx.QueryRow(ctx, `UPDATE idempotency_keys SET status='completed',response_status=202,response_body=$4::jsonb,updated_at=clock_timestamp() WHERE user_id=$1 AND operation=$2 AND key=$3 RETURNING response_body`, c.ActorID, op, c.Key, body).Scan(&body) != nil {
		return pipeline.RecoveryResponse{}, company.ErrStorage
	}
	if tx.Commit(ctx) != nil {
		return pipeline.RecoveryResponse{}, company.ErrStorage
	}
	return pipeline.RecoveryResponse{Status: 202, Body: body}, nil
}

var _ pipeline.RecoveryRepository = (*PrecheckQueue)(nil)
