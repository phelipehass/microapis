// Package httpx tem o encanamento HTTP compartilhado por todos os produtos que
// vão morar neste binário: resposta JSON, erro padronizado e middleware.
package httpx

import (
	"encoding/json"
	"log/slog"
	"net/http"
)

// ErrorCode é um identificador estável de erro. O cliente programa contra ele,
// então mudar um valor destes quebra integração — trate como contrato público.
type ErrorCode string

const (
	CodeInvalidRequest ErrorCode = "invalid_request"
	CodeInvalidURL     ErrorCode = "invalid_url"
	CodeBlockedHost    ErrorCode = "blocked_host"
	CodeFetchFailed    ErrorCode = "fetch_failed"
	CodeTooLarge       ErrorCode = "content_too_large"
	CodeUnsupportedCT  ErrorCode = "unsupported_content_type"
	CodeNoContent      ErrorCode = "no_readable_content"
	CodeTimeout        ErrorCode = "timeout"
	CodeRateLimited    ErrorCode = "rate_limited"
	CodeUnauthorized   ErrorCode = "unauthorized"
	CodeInternal       ErrorCode = "internal_error"
)

type ErrorBody struct {
	Code    ErrorCode `json:"code"`
	Message string    `json:"message"`
}

type ErrorResponse struct {
	Error ErrorBody `json:"error"`
}

func JSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if payload == nil {
		return
	}
	if err := json.NewEncoder(w).Encode(payload); err != nil {
		// Header já foi enviado; só resta registrar. Escrever de novo
		// produziria uma resposta corrompida.
		slog.Error("falha ao serializar resposta", "erro", err)
	}
}

func Error(w http.ResponseWriter, status int, code ErrorCode, message string) {
	JSON(w, status, ErrorResponse{Error: ErrorBody{Code: code, Message: message}})
}
