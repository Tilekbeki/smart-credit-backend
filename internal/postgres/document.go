package postgres

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"smart-credit-backend/internal/application"
	"smart-credit-backend/internal/auth"
	"smart-credit-backend/internal/document"
)

// DocumentRepository keeps pending uploads private until their file is published.
type DocumentRepository struct {
	pool    *pgxpool.Pool
	timeout time.Duration
}

var _ document.Repository = (*DocumentRepository)(nil)

func NewDocumentRepository(pool *pgxpool.Pool, timeout time.Duration) *DocumentRepository {
	return &DocumentRepository{pool: pool, timeout: timeout}
}

const documentColumns = `d.id::text, d.application_id::text, COALESCE(d.document_type,''),
	d.original_name, d.content_type, d.size_bytes, d.created_at, d.storage_key,
	d.checksum_sha256, d.uploaded_by::text, d.status`

func scanDocument(row pgx.Row) (document.Metadata, error) {
	var result document.Metadata
	if err := row.Scan(&result.DocumentID, &result.ApplicationID, &result.DocumentType,
		&result.FileName, &result.ContentType, &result.SizeBytes, &result.UploadedAt,
		&result.StorageKey, &result.Checksum, &result.UploadedBy, &result.Status); err != nil {
		return document.Metadata{}, err
	}
	result.UploadedAt = result.UploadedAt.UTC()
	return result, nil
}

func validateDocumentScope(scope document.Scope, write bool) error {
	if scope.Role != auth.RoleClient && scope.Role != auth.RoleBankEmployee || write && scope.Role != auth.RoleClient {
		return document.ErrForbidden
	}
	if !validUUID(scope.UserID) || scope.Role == auth.RoleClient && !validUUID(scope.CompanyID) {
		return document.ErrApplicationNotFound
	}
	return nil
}

// A bank employee may read all applications in this demo. Client access always
// requires both the application owner and the company from authentication.
func documentScopeValues(scope document.Scope) (bool, any) {
	if scope.Role == auth.RoleBankEmployee {
		return true, nil
	}
	return false, scope.CompanyID
}

func checkedDocumentApplication(ctx context.Context, query interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, scope document.Scope, id string, write, lock bool) (application.Status, error) {
	bank, companyID := documentScopeValues(scope)
	statement := `SELECT a.status FROM applications a WHERE a.id=$1
		AND ($2::boolean OR (a.owner_id=$3::uuid AND a.company_id=$4::uuid))`
	if lock {
		statement += ` FOR UPDATE`
	}
	var status application.Status
	err := query.QueryRow(ctx, statement, id, bank, scope.UserID, companyID).Scan(&status)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", document.ErrApplicationNotFound
	}
	if err != nil {
		return "", document.ErrStorage
	}
	if write && status != application.Draft {
		return "", document.ErrInvalidStatus
	}
	return status, nil
}

func (r *DocumentRepository) CheckApplication(ctx context.Context, scope document.Scope, id string, write bool) (application.Status, error) {
	if err := validateDocumentScope(scope, write); err != nil {
		return "", err
	}
	if !validUUID(id) {
		return "", document.ErrApplicationNotFound
	}
	ctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()
	return checkedDocumentApplication(ctx, r.pool, scope, id, write, false)
}

var documentChecksumPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

func validDocumentMetadata(value document.Metadata) bool {
	return validUUID(value.DocumentID) && value.StorageKey == value.DocumentID &&
		document.ValidType(value.DocumentType) && strings.TrimSpace(value.FileName) != "" &&
		utf8.ValidString(value.FileName) && utf8.RuneCountInString(value.FileName) <= 255 &&
		!strings.ContainsAny(value.FileName, "\x00/\\") &&
		(value.ContentType == "application/pdf" || value.ContentType == "image/png" || value.ContentType == "image/jpeg") &&
		value.SizeBytes > 0 && value.SizeBytes <= document.MaxFileSize && documentChecksumPattern.MatchString(value.Checksum)
}

func (r *DocumentRepository) Reserve(ctx context.Context, scope document.Scope, metadata document.Metadata) (document.Metadata, error) {
	if err := validateDocumentScope(scope, true); err != nil {
		return document.Metadata{}, err
	}
	if !validUUID(metadata.ApplicationID) {
		return document.Metadata{}, document.ErrApplicationNotFound
	}
	if !validDocumentMetadata(metadata) {
		return document.Metadata{}, document.ErrInvalidInput
	}
	ctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return document.Metadata{}, document.ErrStorage
	}
	defer tx.Rollback(ctx)
	if _, err := checkedDocumentApplication(ctx, tx, scope, metadata.ApplicationID, true, true); err != nil {
		return document.Metadata{}, err
	}
	result, err := scanDocument(tx.QueryRow(ctx, `INSERT INTO documents AS d
		(id, application_id, document_type, original_name, content_type, size_bytes,
		checksum_sha256, storage_key, status, uploaded_by, created_at, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,'pending',$9,clock_timestamp(),clock_timestamp())
		RETURNING `+documentColumns, metadata.DocumentID, metadata.ApplicationID, string(metadata.DocumentType),
		metadata.FileName, metadata.ContentType, metadata.SizeBytes, metadata.Checksum, metadata.StorageKey, scope.UserID))
	if err != nil {
		return document.Metadata{}, document.ErrStorage
	}
	if err := tx.Commit(ctx); err != nil {
		return document.Metadata{}, document.ErrStorage
	}
	return result, nil
}

func (r *DocumentRepository) Activate(ctx context.Context, scope document.Scope, applicationID, documentID string) (document.Metadata, error) {
	if err := validateDocumentScope(scope, true); err != nil {
		return document.Metadata{}, err
	}
	if !validUUID(applicationID) {
		return document.Metadata{}, document.ErrApplicationNotFound
	}
	if !validUUID(documentID) {
		return document.Metadata{}, document.ErrNotFound
	}
	ctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return document.Metadata{}, document.ErrStorage
	}
	defer tx.Rollback(ctx)
	if _, err := checkedDocumentApplication(ctx, tx, scope, applicationID, true, true); err != nil {
		return document.Metadata{}, err
	}
	result, err := scanDocument(tx.QueryRow(ctx, `UPDATE documents AS d SET status='ready', updated_at=clock_timestamp()
		WHERE d.id=$1 AND d.application_id=$2 AND d.status='pending' AND d.uploaded_by=$3 RETURNING `+documentColumns,
		documentID, applicationID, scope.UserID))
	if errors.Is(err, pgx.ErrNoRows) {
		return document.Metadata{}, document.ErrNotFound
	}
	if err != nil {
		return document.Metadata{}, document.ErrStorage
	}
	if err := tx.Commit(ctx); err != nil {
		return document.Metadata{}, document.ErrStorage
	}
	return result, nil
}

// Invalidate is internal compensation and startup recovery, never a public API.
func (r *DocumentRepository) Invalidate(ctx context.Context, id string) error {
	if !validUUID(id) {
		return document.ErrNotFound
	}
	ctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()
	if _, err := r.pool.Exec(ctx, `UPDATE documents SET status='failed', updated_at=clock_timestamp() WHERE id=$1`, id); err != nil {
		return document.ErrStorage
	}
	return nil
}

func (r *DocumentRepository) List(ctx context.Context, scope document.Scope, applicationID string) ([]document.Metadata, error) {
	if err := validateDocumentScope(scope, false); err != nil {
		return nil, err
	}
	if !validUUID(applicationID) {
		return nil, document.ErrApplicationNotFound
	}
	ctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, document.ErrStorage
	}
	defer tx.Rollback(ctx)
	if _, err := checkedDocumentApplication(ctx, tx, scope, applicationID, false, false); err != nil {
		return nil, err
	}
	bank, companyID := documentScopeValues(scope)
	rows, err := tx.Query(ctx, `SELECT `+documentColumns+` FROM documents d
		JOIN applications a ON a.id=d.application_id WHERE d.application_id=$1 AND d.status='ready'
		AND ($2::boolean OR (a.owner_id=$3::uuid AND a.company_id=$4::uuid))
		ORDER BY d.created_at, d.id`, applicationID, bank, scope.UserID, companyID)
	if err != nil {
		return nil, document.ErrStorage
	}
	defer rows.Close()
	items := []document.Metadata{}
	for rows.Next() {
		item, err := scanDocument(rows)
		if err != nil {
			return nil, document.ErrStorage
		}
		items = append(items, item)
	}
	if rows.Err() != nil {
		return nil, document.ErrStorage
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, document.ErrStorage
	}
	return items, nil
}

func (r *DocumentRepository) Get(ctx context.Context, scope document.Scope, applicationID, documentID string) (document.Metadata, error) {
	if err := validateDocumentScope(scope, false); err != nil {
		return document.Metadata{}, err
	}
	if !validUUID(applicationID) {
		return document.Metadata{}, document.ErrApplicationNotFound
	}
	if !validUUID(documentID) {
		return document.Metadata{}, document.ErrNotFound
	}
	ctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return document.Metadata{}, document.ErrStorage
	}
	defer tx.Rollback(ctx)
	if _, err := checkedDocumentApplication(ctx, tx, scope, applicationID, false, false); err != nil {
		return document.Metadata{}, err
	}
	bank, companyID := documentScopeValues(scope)
	result, err := scanDocument(tx.QueryRow(ctx, `SELECT `+documentColumns+` FROM documents d
		JOIN applications a ON a.id=d.application_id WHERE d.id=$1 AND d.application_id=$2 AND d.status='ready'
		AND ($3::boolean OR (a.owner_id=$4::uuid AND a.company_id=$5::uuid))`,
		documentID, applicationID, bank, scope.UserID, companyID))
	if errors.Is(err, pgx.ErrNoRows) {
		return document.Metadata{}, document.ErrNotFound
	}
	if err != nil {
		return document.Metadata{}, document.ErrStorage
	}
	if err := tx.Commit(ctx); err != nil {
		return document.Metadata{}, document.ErrStorage
	}
	return result, nil
}

func (r *DocumentRepository) RecoveryRecords(ctx context.Context) ([]document.Metadata, error) {
	ctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()
	rows, err := r.pool.Query(ctx, `SELECT `+documentColumns+` FROM documents d ORDER BY d.created_at, d.id`)
	if err != nil {
		return nil, document.ErrStorage
	}
	defer rows.Close()
	items := []document.Metadata{}
	for rows.Next() {
		item, err := scanDocument(rows)
		if err != nil {
			return nil, document.ErrStorage
		}
		items = append(items, item)
	}
	if rows.Err() != nil {
		return nil, document.ErrStorage
	}
	return items, nil
}
