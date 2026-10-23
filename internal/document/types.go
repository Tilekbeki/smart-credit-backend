// Package document manages private application attachments and durable upload metadata.
package document

import (
	"context"
	"errors"
	"io"
	"os"
	"time"

	"smart-credit-backend/internal/application"
	"smart-credit-backend/internal/auth"
)

const MaxFileSize int64 = 5 << 20
const MaxRequestSize int64 = MaxFileSize + (64 << 10)

type Type string

const (
	BankStatement    Type = "BANK_STATEMENT"
	EquipmentInvoice Type = "EQUIPMENT_INVOICE"
)

var (
	ErrInvalidInput        = errors.New("invalid document input")
	ErrTooLarge            = errors.New("document too large")
	ErrUnsupportedFormat   = errors.New("unsupported document format")
	ErrApplicationNotFound = errors.New("document application not found")
	ErrNotFound            = errors.New("document not found")
	ErrInvalidStatus       = errors.New("document application is not draft")
	ErrForbidden           = errors.New("document access forbidden")
	ErrStorage             = errors.New("document storage unavailable")
)

type Scope struct {
	UserID    string
	CompanyID string
	Role      auth.Role
}
type Metadata struct {
	DocumentID    string    `json:"documentId"`
	ApplicationID string    `json:"applicationId"`
	DocumentType  Type      `json:"documentType"`
	FileName      string    `json:"fileName"`
	ContentType   string    `json:"contentType"`
	SizeBytes     int64     `json:"sizeBytes"`
	UploadedAt    time.Time `json:"uploadedAt"`
	StorageKey    string    `json:"-"`
	Checksum      string    `json:"-"`
	UploadedBy    string    `json:"-"`
	Status        string    `json:"-"`
}

type Repository interface {
	CheckApplication(context.Context, Scope, string, bool) (application.Status, error)
	Reserve(context.Context, Scope, Metadata) (Metadata, error)
	Activate(context.Context, Scope, string, string) (Metadata, error)
	Invalidate(context.Context, string) error
	List(context.Context, Scope, string) ([]Metadata, error)
	Get(context.Context, Scope, string, string) (Metadata, error)
	RecoveryRecords(context.Context) ([]Metadata, error)
}

type Staged struct {
	Key         string
	TempName    string
	ContentType string
	Size        int64
	Checksum    string
}
type Storage interface {
	Stage(context.Context, io.Reader, string) (Staged, error)
	Publish(Staged) error
	Remove(string) error
	Open(string) (*os.File, error)
	Names() ([]string, error)
}

func ValidType(value Type) bool { return value == BankStatement || value == EquipmentInvoice }

// RequiredTypes prepares the later submit policy without blocking partial drafts.
// Turnover reliability must come from trusted company data, never from upload JSON.
func RequiredTypes(productID string, reliableTurnover bool) []Type {
	types := []Type{}
	if productID == "turnover" && !reliableTurnover {
		types = append(types, BankStatement)
	}
	if productID == "investment" {
		types = append(types, EquipmentInvoice)
	}
	return types
}
