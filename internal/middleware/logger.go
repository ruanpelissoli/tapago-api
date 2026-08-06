// Package middleware holds the HTTP middleware shared by every route.
//
// Cross-cutting concerns that are not specific to one endpoint (logging,
// authentication, rate limiting) live here rather than being repeated in
// each handler.
package middleware

import (
	"log/slog"
	"net/http"
	"time"

	chimw "github.com/go-chi/chi/v5/middleware"
)

// RequestLogger emits one structured log line per request once it completes.
//
// It logs after the handler returns so the status code and duration are
// known. 5xx responses log at error level so they surface in alerting
// without needing to parse the status field.
func RequestLogger(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		ww := chimw.NewWrapResponseWriter(w, r.ProtoMajor)

		defer func() {
			attrs := []any{
				"method", r.Method,
				// Path only — the query string can carry tokens or PII.
				"path", r.URL.Path,
				"status", ww.Status(),
				"bytes", ww.BytesWritten(),
				"duration_ms", time.Since(start).Milliseconds(),
			}
			if id := chimw.GetReqID(r.Context()); id != "" {
				attrs = append(attrs, "request_id", id)
			}

			if ww.Status() >= http.StatusInternalServerError {
				slog.Error("http request", attrs...)
				return
			}
			slog.Info("http request", attrs...)
		}()

		next.ServeHTTP(ww, r)
	})
}
