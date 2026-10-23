package httpapi

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"smart-credit-backend/internal/auth"
)

type AuthHandler struct {
	repo       auth.Repository
	tokens     *auth.Tokens
	code       []byte
	middleware *auth.Middleware
	logger     *slog.Logger
}

func NewAuthHandler(repo auth.Repository, tokens *auth.Tokens, code string, dbTimeout time.Duration, logger *slog.Logger) *AuthHandler {
	return &AuthHandler{
		repo: repo, tokens: tokens, code: []byte(code),
		middleware: auth.NewMiddleware(tokens, repo, dbTimeout), logger: logger,
	}
}

func (h *AuthHandler) Register(r chi.Router) {
	r.Get("/api/v1/demo/profiles", h.profiles)
	r.Post("/api/v1/auth/otp/request", h.requestOTP)
	r.Post("/api/v1/auth/otp/verify", h.verifyOTP)
	r.With(h.middleware.Authentication).Get("/api/v1/me", h.me)
}

func (h *AuthHandler) profiles(w http.ResponseWriter, r *http.Request) {
	profiles, err := h.repo.ListProfiles(r.Context())
	if err != nil {
		writeAuthError(w, err)
		return
	}
	if profiles == nil {
		profiles = []auth.Profile{}
	}
	writeJSON(w, http.StatusOK, struct {
		Items []auth.Profile `json:"items"`
	}{Items: profiles})
}

func (h *AuthHandler) requestOTP(w http.ResponseWriter, r *http.Request) {
	var input struct {
		ProfileID string `json:"profileId"`
	}
	if !readJSON(w, r, &input) {
		return
	}
	profileID, ok := parseID(input.ProfileID)
	if !ok {
		writeAPIError(w, http.StatusBadRequest, "invalid_request", "Поле profileId должно содержать UUID пользователя.")
		return
	}
	challenge, err := h.repo.RequestOTP(r.Context(), profileID)
	if err != nil {
		writeAuthError(w, err)
		return
	}
	// No SMS is sent. Neither the code, phone nor JWT is included in logs.
	h.logger.InfoContext(r.Context(), "demo SMS delivery simulated")
	writeJSON(w, http.StatusCreated, struct {
		ID          string    `json:"otpRequestId"`
		ExpiresAt   time.Time `json:"expiresAt"`
		MaskedPhone string    `json:"maskedPhone"`
	}{ID: challenge.ID, ExpiresAt: challenge.ExpiresAt, MaskedPhone: maskPhone(challenge.Phone)})
}

func (h *AuthHandler) verifyOTP(w http.ResponseWriter, r *http.Request) {
	var input struct {
		RequestID string `json:"otpRequestId"`
		Code      string `json:"code"`
	}
	if !readJSON(w, r, &input) {
		return
	}
	requestID, ok := parseID(input.RequestID)
	if !ok || input.Code == "" || utf8.RuneCountInString(input.Code) > 64 {
		writeAPIError(w, http.StatusBadRequest, "invalid_request", "Укажите UUID подтверждения и непустой код длиной до 64 символов.")
		return
	}
	codeMatches := subtle.ConstantTimeCompare([]byte(input.Code), h.code) == 1
	user, err := h.repo.VerifyOTP(r.Context(), requestID, codeMatches)
	if err != nil {
		writeAuthError(w, err)
		return
	}
	token, expiresAt, err := h.tokens.Issue(user)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "internal_error", "Не удалось завершить вход.")
		return
	}
	writeJSON(w, http.StatusOK, struct {
		AccessToken string    `json:"accessToken"`
		TokenType   string    `json:"tokenType"`
		ExpiresAt   time.Time `json:"expiresAt"`
		UserID      string    `json:"userId"`
		Role        auth.Role `json:"role"`
		CompanyID   *string   `json:"companyId"`
	}{token, "Bearer", expiresAt, user.ID, user.Role, user.CompanyID})
}

func (h *AuthHandler) me(w http.ResponseWriter, r *http.Request) {
	user, ok := auth.CurrentUser(r.Context())
	if !ok {
		writeAPIError(w, http.StatusUnauthorized, "unauthorized", "Требуется авторизация.")
		return
	}
	writeJSON(w, http.StatusOK, user)
}

func writeAPIError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]string{"error": code, "message": message})
}

func writeAuthError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, auth.ErrUserNotFound):
		writeAPIError(w, http.StatusNotFound, "profile_not_found", "Демонстрационный профиль не найден.")
	case errors.Is(err, auth.ErrChallengeNotFound):
		writeAPIError(w, http.StatusNotFound, "otp_not_found", "Подтверждение не найдено.")
	case errors.Is(err, auth.ErrOTPInvalid):
		writeAPIError(w, http.StatusBadRequest, "invalid_otp", "Неверный код подтверждения.")
	case errors.Is(err, auth.ErrOTPExpired):
		writeAPIError(w, http.StatusGone, "otp_expired", "Срок действия кода истёк. Запросите новый код.")
	case errors.Is(err, auth.ErrOTPUsed):
		writeAPIError(w, http.StatusConflict, "otp_used", "Подтверждение уже использовано.")
	case errors.Is(err, auth.ErrOTPInvalidated):
		writeAPIError(w, http.StatusConflict, "otp_invalidated", "Запрошен новый код. Используйте последнее подтверждение.")
	case errors.Is(err, auth.ErrOTPLimit):
		writeAPIError(w, http.StatusTooManyRequests, "otp_attempts_exceeded", "Число попыток исчерпано. Запросите новый код.")
	default:
		writeAPIError(w, http.StatusServiceUnavailable, "service_unavailable", "Сервис авторизации временно недоступен.")
	}
}

func parseID(value string) (string, bool) {
	if len(value) != 36 {
		return "", false
	}
	id, err := uuid.Parse(value)
	return id.String(), err == nil && id != uuid.Nil
}

func maskPhone(phone string) string {
	if len(phone) < 6 {
		return "***"
	}
	return phone[:2] + "*** *** " + phone[len(phone)-4:]
}

func readJSON(w http.ResponseWriter, r *http.Request, target any) bool {
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || !strings.EqualFold(mediaType, "application/json") {
		writeAPIError(w, http.StatusUnsupportedMediaType, "unsupported_media_type", "Используйте Content-Type: application/json.")
		return false
	}
	r.Body = http.MaxBytesReader(w, r.Body, 65536)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	err = decoder.Decode(target)
	if err == nil {
		var extra any
		err = decoder.Decode(&extra)
		if err == io.EOF {
			err = nil
		} else if err == nil {
			err = errors.New("trailing JSON")
		}
	}
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeAPIError(w, http.StatusRequestEntityTooLarge, "request_too_large", "Размер JSON не должен превышать 65536 байт.")
		} else {
			writeAPIError(w, http.StatusBadRequest, "invalid_request", "Некорректный JSON или недопустимые поля запроса.")
		}
		return false
	}
	return true
}
