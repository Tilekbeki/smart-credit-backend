package document

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"time"

	"smart-credit-backend/internal/application"
	"smart-credit-backend/internal/auth"
)

type Service struct {
	repo    Repository
	storage Storage
}

func NewService(repo Repository, storage Storage) *Service {
	return &Service{repo: repo, storage: storage}
}

func validScope(scope Scope, write bool) bool {
	if !validKey(scope.UserID) {
		return false
	}
	if scope.Role == auth.RoleClient {
		return validKey(scope.CompanyID)
	}
	return !write && scope.Role == auth.RoleBankEmployee
}

func (s *Service) AuthorizeUpload(ctx context.Context, scope Scope, appID string) error {
	if !validScope(scope, true) {
		return ErrForbidden
	}
	status, err := s.repo.CheckApplication(ctx, scope, appID, true)
	if err != nil {
		return err
	}
	if status != application.Draft {
		return ErrInvalidStatus
	}
	return nil
}

func (s *Service) Upload(ctx context.Context, scope Scope, appID string, docType Type, fileName string, reader io.Reader) (Metadata, error) {
	if err := s.AuthorizeUpload(ctx, scope, appID); err != nil {
		return Metadata{}, err
	}
	if !ValidType(docType) {
		return Metadata{}, ErrInvalidInput
	}
	staged, err := s.storage.Stage(ctx, reader, fileName)
	if err != nil {
		return Metadata{}, err
	}
	metadata := Metadata{DocumentID: staged.Key, ApplicationID: appID, DocumentType: docType,
		FileName: fileName, ContentType: staged.ContentType, SizeBytes: staged.Size,
		StorageKey: staged.Key, Checksum: staged.Checksum, UploadedBy: scope.UserID, Status: "pending"}
	metadata, err = s.repo.Reserve(ctx, scope, metadata)
	if err != nil {
		// A lost commit acknowledgement can leave only an invisible pending row.
		// Startup recovery invalidates it; no published file exists at this point.
		s.storage.Remove(staged.TempName)
		return Metadata{}, err
	}
	if err := s.storage.Publish(staged); err != nil {
		s.compensate(metadata, staged.TempName)
		return Metadata{}, ErrStorage
	}
	ready, err := s.repo.Activate(ctx, scope, appID, metadata.DocumentID)
	if err != nil {
		s.compensate(metadata, staged.TempName)
		return Metadata{}, err
	}
	return ready, nil
}

// Compensation uses a fresh bounded context because a disconnected upload must
// not prevent cleanup. Never remove a file while its row may still be ready.
func (s *Service) compensate(metadata Metadata, temporary string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if s.repo.Invalidate(ctx, metadata.DocumentID) == nil {
		s.storage.Remove(metadata.StorageKey)
		s.storage.Remove(temporary)
	}
}

func (s *Service) verifiedOpen(ctx context.Context, metadata Metadata) (*os.File, error) {
	if !validKey(metadata.StorageKey) {
		return nil, ErrNotFound
	}
	file, err := s.storage.Open(metadata.StorageKey)
	if err != nil {
		return nil, err
	}
	valid := false
	defer func() {
		if !valid {
			file.Close()
		}
	}()
	info, err := file.Stat()
	if err != nil {
		return nil, ErrStorage
	}
	if !info.Mode().IsRegular() || metadata.SizeBytes < 1 || metadata.SizeBytes > MaxFileSize || info.Size() != metadata.SizeBytes {
		return nil, ErrNotFound
	}
	hash := sha256.New()
	read, err := io.Copy(hash, io.LimitReader(contextReader{ctx: ctx, reader: file}, MaxFileSize+1))
	if err != nil || ctx.Err() != nil {
		return nil, ErrStorage
	}
	if read != metadata.SizeBytes || hex.EncodeToString(hash.Sum(nil)) != metadata.Checksum {
		return nil, ErrNotFound
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return nil, ErrStorage
	}
	valid = true
	return file, nil
}

func (s *Service) List(ctx context.Context, scope Scope, appID string) ([]Metadata, error) {
	if !validScope(scope, false) {
		return nil, ErrForbidden
	}
	if _, err := s.repo.CheckApplication(ctx, scope, appID, false); err != nil {
		return nil, err
	}
	records, err := s.repo.List(ctx, scope, appID)
	if err != nil {
		return nil, err
	}
	items := make([]Metadata, 0, len(records))
	for _, metadata := range records {
		file, err := s.verifiedOpen(ctx, metadata)
		if err != nil {
			if ctx.Err() != nil || !errors.Is(err, ErrNotFound) {
				return nil, ErrStorage
			}
			if err := s.repo.Invalidate(ctx, metadata.DocumentID); err != nil {
				return nil, ErrStorage
			}
			continue
		}
		file.Close()
		items = append(items, metadata)
	}
	return items, nil
}

func (s *Service) Download(ctx context.Context, scope Scope, appID, documentID string) (Metadata, *os.File, error) {
	if !validScope(scope, false) {
		return Metadata{}, nil, ErrForbidden
	}
	metadata, err := s.repo.Get(ctx, scope, appID, documentID)
	if err != nil {
		return Metadata{}, nil, err
	}
	file, err := s.verifiedOpen(ctx, metadata)
	if err != nil {
		if ctx.Err() == nil && errors.Is(err, ErrNotFound) {
			if err := s.repo.Invalidate(ctx, metadata.DocumentID); err != nil {
				return Metadata{}, nil, ErrStorage
			}
			return Metadata{}, nil, ErrNotFound
		}
		return Metadata{}, nil, ErrStorage
	}
	return metadata, file, nil
}

// Recover runs before the HTTP listener starts while LocalStorage holds the
// volume lock. Pending rows become failed before their incomplete files vanish.
func (s *Service) Recover(ctx context.Context) error {
	if ctx.Err() != nil {
		return ErrStorage
	}
	records, err := s.repo.RecoveryRecords(ctx)
	if err != nil {
		return err
	}
	keep := make(map[string]bool)
	for _, metadata := range records {
		if metadata.Status == "ready" {
			file, err := s.verifiedOpen(ctx, metadata)
			if err == nil {
				file.Close()
				keep[metadata.StorageKey] = true
				continue
			}
			if !errors.Is(err, ErrNotFound) {
				return ErrStorage
			}
		}
		if ctx.Err() != nil || s.repo.Invalidate(ctx, metadata.DocumentID) != nil {
			return ErrStorage
		}
		// Historical metadata may contain paths from the Python prototype. It is
		// never used to access or remove files in the private Go storage root.
		if !validKey(metadata.StorageKey) {
			continue
		}
		if err := s.storage.Remove(metadata.StorageKey); err != nil {
			return ErrStorage
		}
		if err := s.storage.Remove(tempName(metadata.StorageKey)); err != nil {
			return ErrStorage
		}
	}
	names, err := s.storage.Names()
	if err != nil {
		return err
	}
	for _, name := range names {
		if ctx.Err() != nil {
			return ErrStorage
		}
		if validStoredName(name) && !keep[name] {
			if err := s.storage.Remove(name); err != nil {
				return ErrStorage
			}
		}
	}
	return nil
}
