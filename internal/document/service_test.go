package document

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"smart-credit-backend/internal/application"
	"smart-credit-backend/internal/auth"
)

const testAppID = "a8107c92-b136-4855-93d9-821b777b7f00"

var clientScope = Scope{UserID: "a8107c92-b136-4855-93d9-821b777b7f01", CompanyID: "a8107c92-b136-4855-93d9-821b777b7f02", Role: auth.RoleClient}
var employeeScope = Scope{UserID: "a8107c92-b136-4855-93d9-821b777b7f03", Role: auth.RoleBankEmployee}

type repoStub struct {
	rows                                                                           map[string]Metadata
	status                                                                         application.Status
	checkErr, reserveErr, activateErr, invalidateErr, listErr, getErr, recoveryErr error
	reserveCalls, activateCalls, invalidations                                     int
}

func newRepoStub() *repoStub {
	return &repoStub{rows: map[string]Metadata{}, status: application.Draft}
}
func (r *repoStub) CheckApplication(context.Context, Scope, string, bool) (application.Status, error) {
	return r.status, r.checkErr
}
func (r *repoStub) Reserve(_ context.Context, _ Scope, m Metadata) (Metadata, error) {
	r.reserveCalls++
	if r.reserveErr != nil {
		return Metadata{}, r.reserveErr
	}
	m.UploadedAt = time.Now().UTC()
	r.rows[m.DocumentID] = m
	return m, nil
}
func (r *repoStub) Activate(_ context.Context, _ Scope, _ string, id string) (Metadata, error) {
	r.activateCalls++
	if r.activateErr != nil {
		return Metadata{}, r.activateErr
	}
	m := r.rows[id]
	m.Status = "ready"
	r.rows[id] = m
	return m, nil
}
func (r *repoStub) Invalidate(ctx context.Context, id string) error {
	r.invalidations++
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if r.invalidateErr != nil {
		return r.invalidateErr
	}
	m := r.rows[id]
	m.Status = "failed"
	r.rows[id] = m
	return nil
}
func (r *repoStub) List(context.Context, Scope, string) ([]Metadata, error) {
	if r.listErr != nil {
		return nil, r.listErr
	}
	rows := []Metadata{}
	for _, m := range r.rows {
		if m.Status == "ready" {
			rows = append(rows, m)
		}
	}
	return rows, nil
}
func (r *repoStub) Get(_ context.Context, _ Scope, appID, id string) (Metadata, error) {
	if r.getErr != nil {
		return Metadata{}, r.getErr
	}
	m, ok := r.rows[id]
	if !ok || m.Status != "ready" || m.ApplicationID != appID {
		return Metadata{}, ErrNotFound
	}
	return m, nil
}
func (r *repoStub) RecoveryRecords(context.Context) ([]Metadata, error) {
	if r.recoveryErr != nil {
		return nil, r.recoveryErr
	}
	rows := []Metadata{}
	for _, m := range r.rows {
		rows = append(rows, m)
	}
	return rows, nil
}

func TestDocumentTypeAndRequiredTypes(t *testing.T) {
	for _, kind := range []Type{BankStatement, EquipmentInvoice} {
		if !ValidType(kind) {
			t.Fatal(kind)
		}
	}
	for _, kind := range []Type{"", "bank_statement", "OTHER"} {
		if ValidType(kind) {
			t.Fatal(kind)
		}
	}
	for _, item := range []struct {
		product  string
		reliable bool
		want     []Type
	}{
		{"working_capital", true, []Type{}}, {"working_capital", false, []Type{BankStatement}},
		{"investment", true, []Type{EquipmentInvoice}}, {"investment", false, []Type{BankStatement, EquipmentInvoice}},
	} {
		actual := RequiredTypes(item.product, item.reliable)
		if len(actual) != len(item.want) {
			t.Fatalf("required types: %v", actual)
		}
		for i := range actual {
			if actual[i] != item.want[i] {
				t.Fatalf("required types: %v", actual)
			}
		}
	}
}

func TestServiceUploadListDownloadAndRepeatedType(t *testing.T) {
	storage, _ := localTestStorage(t)
	repo := newRepoStub()
	service := NewService(repo, storage)
	first, err := service.Upload(t.Context(), clientScope, testAppID, BankStatement, "statement.pdf", bytes.NewReader(testPDF()))
	if err != nil {
		t.Fatal(err)
	}
	second, err := service.Upload(t.Context(), clientScope, testAppID, BankStatement, "statement.pdf", bytes.NewReader(testPDF()))
	if err != nil || first.DocumentID == second.DocumentID {
		t.Fatal("repeat upload did not get its own ID", err)
	}
	if first.Status != "ready" || first.FileName != "statement.pdf" || first.UploadedBy != clientScope.UserID || first.DocumentID != first.StorageKey || first.UploadedAt.IsZero() {
		t.Fatalf("incorrect metadata: %+v", first)
	}
	for _, scope := range []Scope{clientScope, employeeScope} {
		items, err := service.List(t.Context(), scope, testAppID)
		if err != nil || len(items) != 2 {
			t.Fatalf("list: %d %v", len(items), err)
		}
		metadata, file, err := service.Download(t.Context(), scope, testAppID, first.DocumentID)
		if err != nil {
			t.Fatal(err)
		}
		actual, err := io.ReadAll(file)
		file.Close()
		if err != nil || !bytes.Equal(actual, testPDF()) || metadata.DocumentID != first.DocumentID {
			t.Fatal("download integrity", err)
		}
	}
	if _, _, err := service.Download(t.Context(), clientScope, "a8107c92-b136-4855-93d9-821b777b7f04", first.DocumentID); !errors.Is(err, ErrNotFound) {
		t.Fatal("document of other application was returned")
	}
}

func TestServiceAuthorizationAndEarlyValidation(t *testing.T) {
	for _, item := range []struct {
		name     string
		scope    Scope
		status   application.Status
		checkErr error
		kind     Type
		want     error
	}{
		{"employee upload", employeeScope, application.Draft, nil, BankStatement, ErrForbidden},
		{"unknown role", Scope{UserID: clientScope.UserID, CompanyID: clientScope.CompanyID, Role: "OTHER"}, application.Draft, nil, BankStatement, ErrForbidden},
		{"missing company", Scope{UserID: clientScope.UserID, Role: auth.RoleClient}, application.Draft, nil, BankStatement, ErrForbidden},
		{"foreign application", clientScope, application.Draft, ErrApplicationNotFound, BankStatement, ErrApplicationNotFound},
		{"not draft", clientScope, application.Submitted, nil, BankStatement, ErrInvalidStatus},
		{"invalid type", clientScope, application.Draft, nil, "OTHER", ErrInvalidInput},
	} {
		t.Run(item.name, func(t *testing.T) {
			storage, _ := localTestStorage(t)
			repo := newRepoStub()
			repo.status = item.status
			repo.checkErr = item.checkErr
			_, err := NewService(repo, storage).Upload(t.Context(), item.scope, testAppID, item.kind, "statement.pdf", bytes.NewReader(testPDF()))
			if !errors.Is(err, item.want) || repo.reserveCalls != 0 {
				t.Fatalf("authorization/validation: %v", err)
			}
			assertNoDocumentFiles(t, storage)
		})
	}
	storage, _ := localTestStorage(t)
	repo := newRepoStub()
	service := NewService(repo, storage)
	if _, err := service.List(t.Context(), Scope{}, testAppID); !errors.Is(err, ErrForbidden) {
		t.Fatal(err)
	}
	if _, _, err := service.Download(t.Context(), Scope{}, testAppID, "x"); !errors.Is(err, ErrForbidden) {
		t.Fatal(err)
	}
	repo.checkErr = ErrApplicationNotFound
	if _, err := service.List(t.Context(), clientScope, testAppID); !errors.Is(err, ErrApplicationNotFound) {
		t.Fatal(err)
	}
}

type faultStorage struct {
	Storage
	stageErr, publishErr, openErr error
}

func (s faultStorage) Open(key string) (*os.File, error) {
	if s.openErr != nil {
		return nil, s.openErr
	}
	return s.Storage.Open(key)
}

func (s faultStorage) Stage(ctx context.Context, r io.Reader, name string) (Staged, error) {
	if s.stageErr != nil {
		return Staged{}, s.stageErr
	}
	return s.Storage.Stage(ctx, r, name)
}
func (s faultStorage) Publish(m Staged) error {
	if s.publishErr != nil {
		return s.publishErr
	}
	return s.Storage.Publish(m)
}

func TestServiceStorageAndMetadataFailureCompensation(t *testing.T) {
	for _, step := range []string{"stage", "reserve", "publish", "activate", "ambiguous activate"} {
		t.Run(step, func(t *testing.T) {
			storage, _ := localTestStorage(t)
			repo := newRepoStub()
			faults := faultStorage{Storage: storage}
			switch step {
			case "stage":
				faults.stageErr = ErrStorage
			case "reserve":
				repo.reserveErr = ErrStorage
			case "publish":
				faults.publishErr = ErrStorage
			case "activate":
				repo.activateErr = ErrStorage
			case "ambiguous activate":
				repo.activateErr = ErrStorage
				repo.invalidateErr = ErrStorage
			}
			service := NewService(repo, faults)
			if _, err := service.Upload(t.Context(), clientScope, testAppID, BankStatement, "statement.pdf", bytes.NewReader(testPDF())); !errors.Is(err, ErrStorage) {
				t.Fatal(err)
			}
			if step == "stage" && repo.reserveCalls != 0 {
				t.Fatal("active metadata created after filesystem failure")
			}
			if step == "ambiguous activate" {
				for _, m := range repo.rows {
					if file, err := storage.Open(m.StorageKey); err != nil {
						t.Fatal("possibly ready file removed after DB failure")
					} else {
						file.Close()
					}
				}
				repo.invalidateErr = nil
				if err := service.Recover(t.Context()); err != nil {
					t.Fatal(err)
				}
			}
			assertNoDocumentFiles(t, storage)
			for _, m := range repo.rows {
				if m.Status != "failed" {
					t.Fatal("incomplete metadata is active")
				}
			}
		})
	}
}

func TestServiceDetectsMissingAndCorruptFiles(t *testing.T) {
	for _, mode := range []string{"missing list", "corrupt list", "missing download", "corrupt download"} {
		t.Run(mode, func(t *testing.T) {
			storage, path := localTestStorage(t)
			repo := newRepoStub()
			service := NewService(repo, storage)
			m, err := service.Upload(t.Context(), clientScope, testAppID, BankStatement, "statement.pdf", bytes.NewReader(testPDF()))
			if err != nil {
				t.Fatal(err)
			}
			if mode == "missing list" || mode == "missing download" {
				if err := storage.Remove(m.StorageKey); err != nil {
					t.Fatal(err)
				}
			} else {
				corrupt := append([]byte{}, testPDF()...)
				corrupt[len(corrupt)-1] = 'X'
				if err := os.WriteFile(filepath.Join(path, m.StorageKey), corrupt, 0600); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "missing list" || mode == "corrupt list" {
				items, err := service.List(t.Context(), clientScope, testAppID)
				if err != nil || len(items) != 0 {
					t.Fatalf("unreadable document advertised: %v %v", items, err)
				}
			} else {
				if _, file, err := service.Download(t.Context(), clientScope, testAppID, m.DocumentID); !errors.Is(err, ErrNotFound) {
					if file != nil {
						file.Close()
					}
					t.Fatal("unreadable document accepted", err)
				}
			}
			if repo.rows[m.DocumentID].Status != "failed" {
				t.Fatal("unreadable document retained ready status")
			}
		})
	}
}

func TestServiceStorageOutagePreservesReadyMetadata(t *testing.T) {
	for _, operation := range []string{"list", "download", "recover"} {
		t.Run(operation, func(t *testing.T) {
			storage, _ := localTestStorage(t)
			repo := newRepoStub()
			service := NewService(repo, storage)
			metadata, err := service.Upload(t.Context(), clientScope, testAppID, BankStatement, "statement.pdf", bytes.NewReader(testPDF()))
			if err != nil {
				t.Fatal(err)
			}
			service = NewService(repo, faultStorage{Storage: storage, openErr: ErrStorage})
			switch operation {
			case "list":
				_, err = service.List(t.Context(), clientScope, testAppID)
			case "download":
				_, _, err = service.Download(t.Context(), clientScope, testAppID, metadata.DocumentID)
			case "recover":
				err = service.Recover(t.Context())
			}
			if !errors.Is(err, ErrStorage) {
				t.Fatal("filesystem outage must be unavailable", err)
			}
			if repo.invalidations != 0 || repo.rows[metadata.DocumentID].Status != "ready" {
				t.Fatal("filesystem outage invalidated a valid document")
			}
			if _, file, err := NewService(repo, storage).Download(t.Context(), clientScope, testAppID, metadata.DocumentID); err != nil {
				t.Fatal("document lost after storage recovery", err)
			} else {
				file.Close()
			}
		})
	}
}

type cancelledActivationRepo struct {
	Repository
	cancel context.CancelFunc
}

func (r cancelledActivationRepo) Activate(context.Context, Scope, string, string) (Metadata, error) {
	r.cancel()
	return Metadata{}, ErrStorage
}

func TestServiceCompensatesAfterUploadContextCancellation(t *testing.T) {
	storage, _ := localTestStorage(t)
	repo := newRepoStub()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	service := NewService(cancelledActivationRepo{Repository: repo, cancel: cancel}, storage)
	if _, err := service.Upload(ctx, clientScope, testAppID, BankStatement, "statement.pdf", bytes.NewReader(testPDF())); !errors.Is(err, ErrStorage) {
		t.Fatal(err)
	}
	if repo.invalidations != 1 {
		t.Fatal("cancelled request skipped compensation")
	}
	for _, metadata := range repo.rows {
		if metadata.Status != "failed" {
			t.Fatal("cancelled request retained pending metadata")
		}
	}
	assertNoDocumentFiles(t, storage)
}

func TestServiceRecoveryPreservesReadyAndCleansIncomplete(t *testing.T) {
	storage, path := localTestStorage(t)
	repo := newRepoStub()
	service := NewService(repo, storage)
	ready, err := service.Upload(t.Context(), clientScope, testAppID, BankStatement, "statement.pdf", bytes.NewReader(testPDF()))
	if err != nil {
		t.Fatal(err)
	}
	for _, status := range []string{"pending", "failed"} {
		staged, err := storage.Stage(t.Context(), bytes.NewReader(testPDF()), "statement.pdf")
		if err != nil {
			t.Fatal(err)
		}
		repo.rows[staged.Key] = Metadata{DocumentID: staged.Key, StorageKey: staged.Key, Status: status}
		if status == "pending" {
			if err := storage.Publish(staged); err != nil {
				t.Fatal(err)
			}
		}
	}
	orphan, err := storage.Stage(t.Context(), bytes.NewReader(testPDF()), "orphan.pdf")
	if err != nil {
		t.Fatal(err)
	}
	if err := storage.Publish(orphan); err != nil {
		t.Fatal(err)
	}
	if _, err := storage.Stage(t.Context(), bytes.NewReader(testPDF()), "orphan-temp.pdf"); err != nil {
		t.Fatal(err)
	}
	legacyID := "a8107c92-b136-4855-93d9-821b777b7f07"
	repo.rows[legacyID] = Metadata{DocumentID: legacyID, StorageKey: "../../legacy.pdf", Status: "failed"}
	if err := os.WriteFile(filepath.Join(path, "legacy-file.txt"), []byte("preserve unknown legacy file"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := service.Recover(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := service.Recover(t.Context()); err != nil {
		t.Fatal("recovery is not repeatable", err)
	}
	names, err := storage.Names()
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != 3 {
		t.Fatalf("unexpected recovered files: %v", names)
	}
	for _, name := range names {
		if name != ".lock" && name != "legacy-file.txt" && name != ready.StorageKey {
			t.Fatalf("incomplete upload retained: %s", name)
		}
	}
	if _, file, err := service.Download(t.Context(), clientScope, testAppID, ready.DocumentID); err != nil {
		t.Fatal("ready upload lost during recovery", err)
	} else {
		file.Close()
	}
}
