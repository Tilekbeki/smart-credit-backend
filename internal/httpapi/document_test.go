package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"image"
	"image/jpeg"
	"image/png"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"strings"
	"testing"
	"time"

	"smart-credit-backend/internal/application"
	"smart-credit-backend/internal/auth"
	"smart-credit-backend/internal/document"
)

const documentAppID = "50000000-0000-4000-8000-000000000001"
const documentOtherAppID = "50000000-0000-4000-8000-000000000002"

type documentRepoStub struct {
	records    map[string]document.Metadata
	status     application.Status
	reserveErr error
	accessErr  error
}

func (s *documentRepoStub) CheckApplication(_ context.Context, scope document.Scope, id string, write bool) (application.Status, error) {
	if s.accessErr != nil {
		return "", s.accessErr
	}
	if write && scope.Role != auth.RoleClient {
		return "", document.ErrForbidden
	}
	if id != documentAppID && id != documentOtherAppID {
		return "", document.ErrApplicationNotFound
	}
	if scope.Role == auth.RoleClient && (scope.UserID != testUserID || scope.CompanyID != ownCompanyID) {
		return "", document.ErrApplicationNotFound
	}
	if scope.Role == auth.RoleBankEmployee && s.status == application.Draft {
		return "", document.ErrApplicationNotFound
	}
	if write && s.status != application.Draft {
		return s.status, document.ErrInvalidStatus
	}
	return s.status, nil
}
func (s *documentRepoStub) Reserve(ctx context.Context, scope document.Scope, m document.Metadata) (document.Metadata, error) {
	if s.reserveErr != nil {
		return document.Metadata{}, s.reserveErr
	}
	if _, err := s.CheckApplication(ctx, scope, m.ApplicationID, true); err != nil {
		return document.Metadata{}, err
	}
	m.UploadedBy = scope.UserID
	m.Status = "pending"
	m.UploadedAt = time.Now().UTC()
	s.records[m.DocumentID] = m
	return m, nil
}
func (s *documentRepoStub) Activate(ctx context.Context, scope document.Scope, appID, id string) (document.Metadata, error) {
	if _, err := s.CheckApplication(ctx, scope, appID, true); err != nil {
		return document.Metadata{}, err
	}
	m, ok := s.records[id]
	if !ok || m.ApplicationID != appID {
		return m, document.ErrNotFound
	}
	m.Status = "ready"
	s.records[id] = m
	return m, nil
}
func (s *documentRepoStub) Invalidate(_ context.Context, id string) error {
	m := s.records[id]
	m.Status = "failed"
	s.records[id] = m
	return nil
}
func (s *documentRepoStub) List(ctx context.Context, scope document.Scope, appID string) ([]document.Metadata, error) {
	if _, err := s.CheckApplication(ctx, scope, appID, false); err != nil {
		return nil, err
	}
	items := []document.Metadata{}
	for _, m := range s.records {
		if m.ApplicationID == appID && m.Status == "ready" {
			items = append(items, m)
		}
	}
	return items, nil
}
func (s *documentRepoStub) Get(ctx context.Context, scope document.Scope, appID, id string) (document.Metadata, error) {
	if _, err := s.CheckApplication(ctx, scope, appID, false); err != nil {
		return document.Metadata{}, err
	}
	m, ok := s.records[id]
	if !ok || m.ApplicationID != appID || m.Status != "ready" {
		return document.Metadata{}, document.ErrNotFound
	}
	return m, nil
}
func (s *documentRepoStub) RecoveryRecords(context.Context) ([]document.Metadata, error) {
	items := []document.Metadata{}
	for _, m := range s.records {
		items = append(items, m)
	}
	return items, nil
}

func documentHTTPFixture(t *testing.T) (http.Handler, *documentRepoStub, *document.LocalStorage, func(auth.User) string) {
	t.Helper()
	repo := &documentRepoStub{records: map[string]document.Metadata{}, status: application.Draft}
	storage, err := document.NewLocalStorage(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = storage.Close() })
	tokens, err := auth.NewTokens(strings.Repeat("d", 32), "document-test", "document-test")
	if err != nil {
		t.Fatal(err)
	}
	// One mutable user reader lets each request exercise role and ownership middleware.
	users := &authRepoStub{user: demoUser()}
	issue := func(u auth.User) string {
		users.user = u
		token, _, err := tokens.Issue(u)
		if err != nil {
			t.Fatal(err)
		}
		return token
	}
	handler := NewDocumentHandler(document.NewService(repo, storage), auth.NewMiddleware(tokens, users, time.Second))
	return NewHandler(nil, testLogger(), time.Second, nil, handler.Register), repo, storage, issue
}

func documentMultipart(t *testing.T, name string, data []byte, docType string) ([]byte, string) {
	t.Helper()
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	header := textproto.MIMEHeader{}
	header.Set("Content-Disposition", mime.FormatMediaType("form-data", map[string]string{"name": "file", "filename": name}))
	header.Set("Content-Type", "text/plain") // Declared type is untrusted; actual content wins.
	part, err := writer.CreatePart(header)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = part.Write(data); err != nil {
		t.Fatal(err)
	}
	if err = writer.WriteField("documentType", docType); err != nil {
		t.Fatal(err)
	}
	if err = writer.Close(); err != nil {
		t.Fatal(err)
	}
	return body.Bytes(), writer.FormDataContentType()
}
func documentRequest(h http.Handler, method, path string, body []byte, contentType, token string, chunked bool) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, bytes.NewReader(body))
	if chunked {
		req.ContentLength = -1
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}
func documentSamples(t *testing.T) map[string][]byte {
	t.Helper()
	samples := map[string][]byte{"demo.pdf": []byte("%PDF-1.4\n1 0 obj\n<< /Type /Catalog >>\nendobj\n%%EOF\n")}
	var p, j bytes.Buffer
	img := image.NewRGBA(image.Rect(0, 0, 1, 1))
	if err := png.Encode(&p, img); err != nil {
		t.Fatal(err)
	}
	if err := jpeg.Encode(&j, img, nil); err != nil {
		t.Fatal(err)
	}
	samples["demo.png"] = p.Bytes()
	samples["demo.jpg"] = j.Bytes()
	return samples
}

func TestDocumentHTTPUploadListDownloadAndRepeat(t *testing.T) {
	h, _, _, issue := documentHTTPFixture(t)
	token := issue(demoUser())
	base := "/api/v1/applications/" + documentAppID + "/documents"
	for name, data := range documentSamples(t) {
		for range 2 {
			body, ct := documentMultipart(t, name, data, "BANK_STATEMENT")
			w := documentRequest(h, "POST", base, body, ct, token, false)
			var meta document.Metadata
			if w.Code != 201 || json.Unmarshal(w.Body.Bytes(), &meta) != nil {
				t.Fatalf("upload%d %s", w.Code, w.Body)
			}
			if meta.DocumentID == "" || meta.SizeBytes != int64(len(data)) || meta.ApplicationID != documentAppID || meta.FileName != name || meta.UploadedAt.IsZero() {
				t.Fatal("metadata mismatch")
			}
			for _, hidden := range []string{"storageKey", "checksum", "uploadedBy", "status"} {
				if bytes.Contains(w.Body.Bytes(), []byte(`"`+hidden+`"`)) {
					t.Fatal("internal metadata leaked")
				}
			}
			w = documentRequest(h, "GET", base+"/"+meta.DocumentID, nil, "", token, false)
			if w.Code != 200 || !bytes.Equal(w.Body.Bytes(), data) || w.Header().Get("Content-Type") != meta.ContentType || w.Header().Get("X-Content-Type-Options") != "nosniff" || w.Header().Get("Cache-Control") != "no-store" {
				t.Fatal("download mismatch")
			}
			disposition, params, err := mime.ParseMediaType(w.Header().Get("Content-Disposition"))
			if err != nil || disposition != "attachment" || params["filename"] != name {
				t.Fatal("download filename")
			}
		}
	}
	w := documentRequest(h, "GET", base, nil, "", token, false)
	var list struct {
		Items []document.Metadata `json:"items"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &list) != nil || len(list.Items) != 6 {
		t.Fatal("repeat uploads notlisted")
	}
}

func TestDocumentHTTPAccessAndDocumentApplicationBinding(t *testing.T) {
	h, repo, _, issue := documentHTTPFixture(t)
	token := issue(demoUser())
	base := "/api/v1/applications/" + documentAppID + "/documents"
	data := documentSamples(t)["demo.pdf"]
	body, ct := documentMultipart(t, "demo.pdf", data, "BANK_STATEMENT")
	w := documentRequest(h, "POST", base, body, ct, token, false)
	var meta document.Metadata
	if w.Code != 201 || json.Unmarshal(w.Body.Bytes(), &meta) != nil {
		t.Fatal("upload fixture")
	}
	for _, path := range []string{base, base + "/" + meta.DocumentID} {
		if documentRequest(h, "GET", path, nil, "", "", false).Code != 401 {
			t.Fatal("JWT required")
		}
	}
	if documentRequest(h, "POST", base, body, ct, "", false).Code != 401 {
		t.Fatal("upload JWT required")
	}
	for _, path := range []string{base, base + "/" + meta.DocumentID} {
		if documentRequest(h, "GET", path, nil, "", token, false).Code != 200 {
			t.Fatal("owner cannot read draft documents")
		}
	}
	employee := demoUser()
	employee.ID = "20000000-0000-4000-8000-000000000004"
	employee.Role = auth.RoleBankEmployee
	employee.CompanyID = nil
	bank := issue(employee)
	if documentRequest(h, "POST", base, body, ct, bank, false).Code != 403 {
		t.Fatal("employee upload accepted")
	}
	for _, path := range []string{base, base + "/" + meta.DocumentID} {
		w = documentRequest(h, "GET", path, nil, "", bank, false)
		if w.Code != 404 || responseJSON(t, w)["error"] != "application_not_found" {
			t.Fatalf("bank accessed draft documents: %d %s", w.Code, w.Body)
		}
	}
	foreign := demoUser()
	foreign.ID = "20000000-0000-4000-8000-000000000002"
	other := otherCompanyID
	foreign.CompanyID = &other
	foreignToken := issue(foreign)
	for _, path := range []string{base, base + "/" + meta.DocumentID} {
		w = documentRequest(h, "GET", path, nil, "", foreignToken, false)
		if w.Code != 404 || responseJSON(t, w)["error"] != "application_not_found" {
			t.Fatal("foreign data leaked")
		}
	}
	if documentRequest(h, "POST", base, body, ct, foreignToken, false).Code != 404 {
		t.Fatal("foreign write accepted")
	}
	token = issue(demoUser())
	w = documentRequest(h, "GET", "/api/v1/applications/"+documentOtherAppID+"/documents/"+meta.DocumentID, nil, "", token, false)
	if w.Code != 404 || responseJSON(t, w)["error"] != "document_not_found" {
		t.Fatal("document not bound to application")
	}
	repo.status = application.Submitted
	if documentRequest(h, "POST", base, body, ct, token, false).Code != 409 {
		t.Fatal("non-draft upload accepted")
	}
	if documentRequest(h, "GET", base+"/"+meta.DocumentID, nil, "", token, false).Code != 200 {
		t.Fatal("submitted attachment should remainreadable")
	}
	bank = issue(employee)
	for _, path := range []string{base, base + "/" + meta.DocumentID} {
		if w := documentRequest(h, "GET", path, nil, "", bank, false); w.Code != 200 {
			t.Fatalf("bank cannot read submitted documents: %d %s", w.Code, w.Body)
		}
	}
}

func TestDocumentHTTPValidationAndBodyLimits(t *testing.T) {
	pdf := documentSamples(t)["demo.pdf"]
	for _, tc := range []struct {
		name, fileName, kind string
		data                 []byte
		status               int
	}{
		{"empty", "demo.pdf", "BANK_STATEMENT", nil, 400},
		{"unknown type", "demo.pdf", "OTHER", pdf, 400},
		{"wrongextension", "demo.png", "BANK_STATEMENT", pdf, 415},
		{"unsupported", "demo.txt", "BANK_STATEMENT", []byte("hello"), 415},
		{"path traversal", "../demo.pdf", "BANK_STATEMENT", pdf, 400},
		{"Windows path", "..\\demo.pdf", "BANK_STATEMENT", pdf, 400},
		{"oversize", "demo.pdf", "BANK_STATEMENT", append(bytes.Repeat([]byte("x"), int(document.MaxFileSize)), pdf...), 413},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, _, _, issue := documentHTTPFixture(t)
			body, ct := documentMultipart(t, tc.fileName, tc.data, tc.kind)
			w := documentRequest(h, "POST", "/api/v1/applications/"+documentAppID+"/documents", body, ct, issue(demoUser()), true)
			if w.Code != tc.status {
				t.Fatalf("got%d want%d %s", w.Code, tc.status, w.Body)
			}
		})
	}
	h, _, _, issue := documentHTTPFixture(t)
	token := issue(demoUser())
	base := "/api/v1/applications/" + documentAppID + "/documents"
	maxPDF := append([]byte("%PDF-1.4\n"), bytes.Repeat([]byte("x"), int(document.MaxFileSize)-9)...)
	body, ct := documentMultipart(t, "max.pdf", maxPDF, "BANK_STATEMENT")
	if w := documentRequest(h, "POST", base, body, ct, token, true); w.Code != 201 {
		t.Fatalf("exact5MiB rejected%d %s", w.Code, w.Body)
	}
	small, ct := documentMultipart(t, "demo.pdf", pdf, "BANK_STATEMENT")
	hugeEpilogue := append(small, bytes.Repeat([]byte("x"), int(document.MaxRequestSize))...)
	for _, chunked := range []bool{false, true} {
		if w := documentRequest(h, "POST", base, hugeEpilogue, ct, token, chunked); w.Code != 413 {
			t.Fatalf("fullrequest limit bypassed: %d", w.Code)
		}
	}
}

func TestDocumentHTTPMalformedMultipartAndStorageErrors(t *testing.T) {
	h, repo, storage, issue := documentHTTPFixture(t)
	token := issue(demoUser())
	base := "/api/v1/applications/" + documentAppID + "/documents"
	for _, tc := range []struct {
		body, ct string
		want     int
	}{{`{}`, "application/json", 415}, {"oops", "multipart/form-data", 400}, {"truncated", "multipart/form-data; boundary=missing", 400}, {"--x--\r\n", "multipart/form-data; boundary=x", 400}} {
		if w := documentRequest(h, "POST", base, []byte(tc.body), tc.ct, token, false); w.Code != tc.want {
			t.Fatalf("malformed accepted%d", w.Code)
		}
	}
	for _, path := range []string{"/api/v1/applications/not-a-uuid/documents", base + "/not-a-uuid"} {
		if w := documentRequest(h, "GET", path, nil, "", token, false); w.Code != 400 {
			t.Fatal("invalid UUID accepted")
		}
	}
	if w := documentRequest(h, "GET", base+"/ffffffff-ffff-4fff-8fff-ffffffffffff", nil, "", token, false); w.Code != 404 {
		t.Fatal("missingdocument accepted")
	}
	body, ct := documentMultipart(t, "demo.pdf", documentSamples(t)["demo.pdf"], "BANK_STATEMENT")
	repo.reserveErr = document.ErrStorage
	w := documentRequest(h, "POST", base, body, ct, token, false)
	if w.Code != 503 {
		t.Fatal("DBerror exposed")
	}
	names, err := storage.Names()
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range names {
		if name != ".lock" {
			t.Fatal("file leftafter DBerror")
		}
	}
	repo.reserveErr = nil
	_ = storage.Close()
	w = documentRequest(h, "POST", base, body, ct, token, false)
	if w.Code != 503 {
		t.Fatalf("FSerror status%d", w.Code)
	}
	if strings.Contains(w.Body.String(), "/tmp") {
		t.Fatal("internal path leaked")
	}
	repo.accessErr = errors.New("secret database error")
	w = documentRequest(h, "GET", base, nil, "", token, false)
	if w.Code != 503 || strings.Contains(w.Body.String(), "secret") {
		t.Fatal("internalerror leaked")
	}
}
