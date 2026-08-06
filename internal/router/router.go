// Package router wires HTTP routes to their handlers.
//
// It is the single place that knows the full URL surface of the API. Handler
// packages stay unaware of their own paths, which keeps routes greppable from
// one file.
package router

import (
	"net/http"

	"github.com/go-chi/chi/v5"
	chimw "github.com/go-chi/chi/v5/middleware"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tapago/tapago-api/internal/handler"
	"github.com/tapago/tapago-api/internal/handler/health"
	"github.com/tapago/tapago-api/internal/middleware"
)

// Deps carries the dependencies handlers need. Passing them explicitly keeps
// the router testable and avoids package-level singletons.
type Deps struct {
	// DB is the shared connection pool. It may be nil in tests that only
	// exercise routes which do not touch the database.
	DB *pgxpool.Pool
}

// New builds the application router with all routes and middleware attached.
func New(deps Deps) http.Handler {
	r := chi.NewRouter()

	// Order matters: RequestID must run before RequestLogger so the logger
	// can attach the id, and Recoverer must wrap the handlers so a panic is
	// still logged as a 500 rather than killing the connection.
	r.Use(chimw.RequestID)
	r.Use(chimw.RealIP)
	r.Use(middleware.RequestLogger)
	r.Use(chimw.Recoverer)

	r.NotFound(func(w http.ResponseWriter, _ *http.Request) {
		handler.Error(w, http.StatusNotFound, "resource not found")
	})
	r.MethodNotAllowed(func(w http.ResponseWriter, _ *http.Request) {
		handler.Error(w, http.StatusMethodNotAllowed, "method not allowed")
	})

	r.Get("/health", health.Check)

	// Future feature routes mount here, e.g.:
	//   r.Route("/v1/auth", func(r chi.Router) { ... })

	return r
}
