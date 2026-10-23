package httpapi

import (
	"bytes"
	"errors"
	"io"
	"mime"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
	"smart-credit-backend/internal/auth"
	"smart-credit-backend/internal/document"
)

type DocumentHandler struct {
	service    *document.Service
	middleware *auth.Middleware
}

func NewDocumentHandler(service *document.Service, middleware *auth.Middleware) *DocumentHandler {
	return &DocumentHandler{service: service, middleware: middleware}
}

func (h *DocumentHandler) Register(r chi.Router) {
	r.Group(func(r chi.Router) {
		r.Use(h.middleware.Authentication)
		r.With(h.middleware.RequireRole(auth.RoleClient)).Post("/api/v1/applications/{applicationId}/documents", h.upload)
		r.With(h.middleware.RequireRole(auth.RoleClient, auth.RoleBankEmployee)).Get("/api/v1/applications/{applicationId}/documents", h.listDocuments)
		r.With(h.middleware.RequireRole(auth.RoleClient, auth.RoleBankEmployee)).Get("/api/v1/applications/{applicationId}/documents/{documentId}", h.download)
	})
}

func documentScope(r *http.Request) document.Scope {
	u, _ := auth.CurrentUser(r.Context())
	scope := document.Scope{UserID: u.ID, Role: u.Role}
	if u.CompanyID != nil {
		scope.CompanyID = *u.CompanyID
	}
	return scope
}

func documentApplicationID(w http.ResponseWriter, r *http.Request) (string, bool) {
	id, ok := parseID(chi.URLParam(r, "applicationId"))
	if !ok {
		writeDocumentError(w, document.ErrInvalidInput)
	}
	return id, ok
}

func (h *DocumentHandler) upload(w http.ResponseWriter, r *http.Request) {
	appID, ok := documentApplicationID(w, r)
	if !ok {
		return
	}
	scope := documentScope(r)
	if err := h.service.AuthorizeUpload(r.Context(), scope, appID); err != nil {
		writeDocumentError(w, err)
		return
	}
	if r.ContentLength > document.MaxRequestSize {
		writeDocumentError(w, document.ErrTooLarge)
		return
	}
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "multipart/form-data" {
		writeAPIError(w, http.StatusUnsupportedMediaType, "unsupported_media_type", "Используйте multipart/form-data с полями file и documentType.")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, document.MaxRequestSize)
	reader, err := r.MultipartReader()
	if err != nil {
		writeDocumentError(w, document.ErrInvalidInput)
		return
	}
	var file []byte
	var fileName string
	var documentType document.Type
	fileSeen, typeSeen := false, false
	for {
		part, err := reader.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			writeMultipartError(w, err)
			return
		}
		_, params, err := mime.ParseMediaType(part.Header.Get("Content-Disposition"))
		if err != nil {
			writeDocumentError(w, document.ErrInvalidInput)
			return
		}
		switch part.FormName() {
		case "file":
			if fileSeen {
				writeDocumentError(w, document.ErrInvalidInput)
				return
			}
			fileSeen = true
			// FileName() strips directories. Validate the raw decoded parameter first.
			fileName = params["filename"]
			if fileName == "" || strings.ContainsAny(fileName, "/\\") {
				writeDocumentError(w, document.ErrInvalidInput)
				return
			}
			if err := document.ValidateFileName(fileName); err != nil {
				writeDocumentError(w, err)
				return
			}
			file, err = io.ReadAll(io.LimitReader(part, document.MaxFileSize+1))
			if err != nil {
				writeMultipartError(w, err)
				return
			}
			if int64(len(file)) > document.MaxFileSize {
				writeDocumentError(w, document.ErrTooLarge)
				return
			}
		case "documentType":
			if typeSeen || params["filename"] != "" {
				writeDocumentError(w, document.ErrInvalidInput)
				return
			}
			typeSeen = true
			value, err := io.ReadAll(io.LimitReader(part, 65))
			if err != nil {
				writeMultipartError(w, err)
				return
			}
			if len(value) > 64 {
				writeDocumentError(w, document.ErrInvalidInput)
				return
			}
			documentType = document.Type(value)
		default:
			writeDocumentError(w, document.ErrInvalidInput)
			return
		}
		if err := part.Close(); err != nil {
			writeMultipartError(w, err)
			return
		}
	}
	// Consume the epilogue too: chunked requests must not bypass the total limit.
	if _, err := io.Copy(io.Discard, r.Body); err != nil {
		writeMultipartError(w, err)
		return
	}
	if !fileSeen || !typeSeen {
		writeDocumentError(w, document.ErrInvalidInput)
		return
	}
	result, err := h.service.Upload(r.Context(), scope, appID, documentType, fileName, bytes.NewReader(file))
	if err != nil {
		writeDocumentError(w, err)
		return
	}
	w.Header().Set("Location", "/api/v1/applications/"+appID+"/documents/"+result.DocumentID)
	writeJSON(w, http.StatusCreated, result)
}

func writeMultipartError(w http.ResponseWriter, err error) {
	var maxBytes *http.MaxBytesError
	if errors.As(err, &maxBytes) {
		writeDocumentError(w, document.ErrTooLarge)
		return
	}
	writeDocumentError(w, document.ErrInvalidInput)
}

func (h *DocumentHandler) listDocuments(w http.ResponseWriter, r *http.Request) {
	appID, ok := documentApplicationID(w, r)
	if !ok {
		return
	}
	result, err := h.service.List(r.Context(), documentScope(r), appID)
	if err != nil {
		writeDocumentError(w, err)
		return
	}
	if result == nil {
		result = []document.Metadata{}
	}
	writeJSON(w, http.StatusOK, struct {
		Items []document.Metadata `json:"items"`
	}{result})
}

func (h *DocumentHandler) download(w http.ResponseWriter, r *http.Request) {
	appID, ok := documentApplicationID(w, r)
	if !ok {
		return
	}
	docID, ok := parseID(chi.URLParam(r, "documentId"))
	if !ok {
		writeDocumentError(w, document.ErrInvalidInput)
		return
	}
	meta, file, err := h.service.Download(r.Context(), documentScope(r), appID, docID)
	if err != nil {
		writeDocumentError(w, err)
		return
	}
	defer file.Close()
	if _, err = file.Seek(0, io.SeekStart); err != nil {
		writeDocumentError(w, document.ErrStorage)
		return
	}
	w.Header().Set("Content-Type", meta.ContentType)
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": meta.FileName}))
	w.Header().Set("Content-Length", strconv.FormatInt(meta.SizeBytes, 10))
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(w, file)
}

func writeDocumentError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, document.ErrInvalidInput):
		writeAPIError(w, http.StatusBadRequest, "invalid_request", "Проверьте поля multipart, тип документа и имя файла.")
	case errors.Is(err, document.ErrTooLarge):
		writeAPIError(w, http.StatusRequestEntityTooLarge, "request_too_large", "Размер файла не должен превышать 5 МиБ, запроса — 5 МиБ + 64 КиБ.")
	case errors.Is(err, document.ErrUnsupportedFormat):
		writeAPIError(w, http.StatusUnsupportedMediaType, "unsupported_file_type", "Допустимы PDF, PNG и JPEG; расширение должно соответствовать содержимому.")
	case errors.Is(err, document.ErrApplicationNotFound):
		writeAPIError(w, http.StatusNotFound, "application_not_found", "Заявка не найдена.")
	case errors.Is(err, document.ErrNotFound):
		writeAPIError(w, http.StatusNotFound, "document_not_found", "Документ не найден.")
	case errors.Is(err, document.ErrInvalidStatus):
		writeAPIError(w, http.StatusConflict, "invalid_status", "Загрузка документов доступна только для черновика.")
	case errors.Is(err, document.ErrForbidden):
		writeAPIError(w, http.StatusForbidden, "forbidden", "Недостаточно прав")
	default:
		writeAPIError(w, http.StatusServiceUnavailable, "service_unavailable", "Хранилище документов временно недоступно.")
	}
}
