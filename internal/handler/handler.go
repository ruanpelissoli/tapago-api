// Package handler holds shared HTTP response helpers used by the concrete
// handler packages beneath it (handler/health, handler/auth, ...).
//
// Handlers never write response bodies by hand; they go through JSON or
// Error so every endpoint emits the same content type and error envelope.
package handler

import (
	"encoding/json"
	"log/slog"
	"net/http"
)

// ErrorResponse is the envelope returned for every non-2xx response.
type ErrorResponse struct {
	Error string `json:"error"`
}

// JSON writes v as a JSON body with the given status code.
//
// The payload is marshalled before any header is written so that an
// encoding failure can still produce a 500 instead of a truncated body.
func JSON(w http.ResponseWriter, status int, v any) {
	body, err := json.Marshal(v)
	if err != nil {
		slog.Error("encode response body", "error", err)
		http.Error(w, `{"error":"internal server error"}`, http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if _, err := w.Write(body); err != nil {
		// The client went away mid-write; nothing left to do but record it.
		slog.Debug("write response body", "error", err)
	}
}

// Error writes a JSON error envelope with the given status code.
//
// The message is shown to the client, so it must never contain internal
// details such as SQL text, stack traces, or connection strings.
func Error(w http.ResponseWriter, status int, message string) {
	JSON(w, status, ErrorResponse{Error: message})
}
