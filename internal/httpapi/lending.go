package httpapi

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"
	"smart-credit-backend/internal/application"
	"smart-credit-backend/internal/auth"
	"smart-credit-backend/internal/decision"
	"smart-credit-backend/internal/lending"
)

type LendingHandler struct {
	service    *lending.Service
	middleware *auth.Middleware
}

func NewLendingHandler(service *lending.Service, middleware *auth.Middleware) *LendingHandler {
	return &LendingHandler{service: service, middleware: middleware}
}

func (h *LendingHandler) Register(r chi.Router) {
	r.With(h.middleware.Authentication, h.middleware.RequireRole(auth.RoleBankEmployee)).Post("/api/v1/bank/applications/{applicationId}/decisions", h.manual)
	r.Group(func(r chi.Router) {
		r.Use(h.middleware.Authentication, h.middleware.RequireRole(auth.RoleClient))
		r.Post("/api/v1/applications/{applicationId}/accept", h.accept)
		r.Post("/api/v1/applications/{applicationId}/decline", h.decline)
	})
}

func lendingCommand(w http.ResponseWriter, r *http.Request, operation string) (lending.Command, bool) {
	id, ok := applicationID(w, r)
	if !ok {
		return lending.Command{}, false
	}
	keys := r.Header.Values("Idempotency-Key")
	if len(keys) != 1 {
		writeLendingError(w, application.ErrInvalidInput)
		return lending.Command{}, false
	}
	key, ok := parseID(keys[0])
	if !ok {
		writeLendingError(w, application.ErrInvalidInput)
		return lending.Command{}, false
	}
	u, _ := auth.CurrentUser(r.Context())
	command := lending.Command{ApplicationID: id, ActorID: u.ID, Key: key, Operation: operation}
	if u.CompanyID != nil {
		command.CompanyID = *u.CompanyID
	}
	return command, true
}

func (h *LendingHandler) manual(w http.ResponseWriter, r *http.Request) {
	command, ok := lendingCommand(w, r, lending.Manual)
	if !ok {
		return
	}
	var raw json.RawMessage
	if !readJSON(w, r, &raw) {
		return
	}
	var input struct {
		Version  int64                `json:"version"`
		Decision string               `json:"decision"`
		Comment  string               `json:"comment"`
		Offer    *lending.ManualOffer `json:"offer"`
	}
	object, ok := strictLendingObject(raw, &input, []string{"version", "decision", "comment", "offer"}, "offer")
	if !ok {
		writeLendingError(w, application.ErrInvalidInput)
		return
	}
	if input.Offer != nil {
		if _, ok := strictLendingObject(object["offer"], new(lending.ManualOffer), []string{"amountRub", "termMonths"}, ""); !ok {
			writeLendingError(w, application.ErrInvalidInput)
			return
		}
	}
	command.Version, command.Decision, command.Comment, command.Offer = input.Version, input.Decision, input.Comment, input.Offer
	h.execute(w, r, command)
}

func (h *LendingHandler) accept(w http.ResponseWriter, r *http.Request) {
	h.offerAction(w, r, lending.Accept)
}
func (h *LendingHandler) decline(w http.ResponseWriter, r *http.Request) {
	h.offerAction(w, r, lending.Decline)
}

func (h *LendingHandler) offerAction(w http.ResponseWriter, r *http.Request, operation string) {
	command, ok := lendingCommand(w, r, operation)
	if !ok {
		return
	}
	var raw json.RawMessage
	if !readJSON(w, r, &raw) {
		return
	}
	var input struct {
		Version int64  `json:"version"`
		OfferID string `json:"offerId"`
	}
	if _, ok := strictLendingObject(raw, &input, []string{"version", "offerId"}, ""); !ok {
		writeLendingError(w, application.ErrInvalidInput)
		return
	}
	command.Version, command.OfferID = input.Version, input.OfferID
	h.execute(w, r, command)
}

func (h *LendingHandler) execute(w http.ResponseWriter, r *http.Request, command lending.Command) {
	result, err := h.service.Execute(r.Context(), command)
	if err != nil {
		writeLendingError(w, err)
		return
	}
	if (result.Status != http.StatusOK && result.Status != http.StatusCreated) || !json.Valid(result.Body) {
		writeLendingError(w, application.ErrStorage)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(result.Status)
	_, _ = w.Write(result.Body)
}

func writeLendingError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, application.ErrInvalidInput):
		writeAPIError(w, http.StatusBadRequest, "invalid_request", "Некорректный JSON или недопустимые поля запроса.")
	case errors.Is(err, lending.ErrValidation), errors.Is(err, decision.ErrManualTerms):
		details := []lending.FieldError{{Field: "offer.amountRub", Code: "invalid_value", Message: "Проверьте допустимость условий кредитного решения."}}
		var validation *lending.ValidationError
		var terms *decision.ManualTermsError
		if errors.As(err, &validation) && len(validation.Details) != 0 {
			details = validation.Details
		}
		if errors.As(err, &terms) {
			details = []lending.FieldError{{Field: terms.Field, Code: terms.Code, Message: terms.Message}}
		}
		writeJSON(w, http.StatusUnprocessableEntity, struct {
			Error   string               `json:"error"`
			Message string               `json:"message"`
			Details []lending.FieldError `json:"details"`
		}{"validation_failed", "Проверьте комментарий и допустимость условий кредитного решения.", details})
	case errors.Is(err, application.ErrNotFound):
		writeAPIError(w, http.StatusNotFound, "application_not_found", "Заявка не найдена.")
	case errors.Is(err, application.ErrVersionConflict):
		writeAPIError(w, http.StatusConflict, "version_conflict", "Заявка была изменена. Обновите данные.")
	case errors.Is(err, application.ErrInvalidStatus):
		writeAPIError(w, http.StatusConflict, "invalid_status", "Действие недоступно в текущем статусе заявки.")
	case errors.Is(err, lending.ErrOfferNotActive):
		writeAPIError(w, http.StatusConflict, "offer_not_active", "Предложение больше недоступно.")
	case errors.Is(err, lending.ErrIdempotencyConflict):
		writeAPIError(w, http.StatusConflict, "idempotency_conflict", "Ключ идемпотентности уже использован с другим запросом.")
	default:
		writeAPIError(w, http.StatusServiceUnavailable, "service_unavailable", "Сервис кредитных решений временно недоступен.")
	}
}

// The ordinary decoder rejects unknown fields; this boundary additionally
// rejects duplicate keys and distinguishes omitted fields from explicit null.
func strictLendingObject(raw []byte, target any, required []string, nullable string) (map[string]json.RawMessage, bool) {
	if !utf8.Valid(raw) || !json.Valid(raw) || !uniqueLendingKeys(json.NewDecoder(bytes.NewReader(raw)), 0) {
		return nil, false
	}
	var object map[string]json.RawMessage
	if json.Unmarshal(raw, &object) != nil || object == nil || len(object) != len(required) {
		return nil, false
	}
	for _, key := range required {
		value, exists := object[key]
		if !exists || (key != nullable && bytes.Equal(bytes.TrimSpace(value), []byte("null"))) {
			return nil, false
		}
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if decoder.Decode(target) != nil || decoder.Decode(new(any)) != io.EOF {
		return nil, false
	}
	return object, true
}

func uniqueLendingKeys(decoder *json.Decoder, depth int) bool {
	if depth > 16 {
		return false
	}
	token, err := decoder.Token()
	if err != nil {
		return false
	}
	delim, composite := token.(json.Delim)
	if !composite {
		return true
	}
	switch delim {
	case '{':
		seen := map[string]bool{}
		for decoder.More() {
			key, err := decoder.Token()
			name, ok := key.(string)
			if err != nil || !ok || seen[name] {
				return false
			}
			seen[name] = true
			if !uniqueLendingKeys(decoder, depth+1) {
				return false
			}
		}
	case '[':
		for decoder.More() {
			if !uniqueLendingKeys(decoder, depth+1) {
				return false
			}
		}
	default:
		return false
	}
	_, err = decoder.Token()
	return err == nil
}
