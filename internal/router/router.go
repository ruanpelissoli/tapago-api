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
	authhandler "github.com/tapago/tapago-api/internal/handler/auth"
	bethandler "github.com/tapago/tapago-api/internal/handler/bet"
	"github.com/tapago/tapago-api/internal/handler/health"
	paymenthandler "github.com/tapago/tapago-api/internal/handler/payment"
	"github.com/tapago/tapago-api/internal/mercadopago"
	"github.com/tapago/tapago-api/internal/middleware"
	"github.com/tapago/tapago-api/internal/token"
)

// Deps carries the dependencies handlers need. Passing them explicitly keeps
// the router testable and avoids package-level singletons.
type Deps struct {
	// DB is the shared connection pool. It may be nil in tests that only
	// exercise routes which do not touch the database.
	DB *pgxpool.Pool
	// Tokens signs and verifies access tokens. A nil Issuer fails closed —
	// verification rejects every token — so the route surface stays the same
	// whether or not auth is configured. cmd/api always supplies one.
	Tokens *token.Issuer
	// Social holds the Google and Apple ID-token verifiers. Either may be
	// nil when that provider has no client id configured; the route is still
	// mounted and answers 503, keeping the URL surface independent of the
	// environment.
	Social authhandler.SocialVerifiers
	// MercadoPago talks to the payment provider. It may be nil when no
	// access token is configured; the payment routes are still mounted and
	// answer 503, for the same reason as Social — the URL surface must not
	// depend on the environment.
	MercadoPago mercadopago.MercadoPagoClient
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

	auth := authhandler.NewHandler(deps.DB, deps.Tokens, deps.Social)
	payments := paymenthandler.NewHandler(deps.DB, deps.MercadoPago)
	bets := bethandler.NewHandler(deps.DB, deps.MercadoPago)

	// Public: these are how a client obtains a token in the first place, so
	// they must sit outside RequireAuth.
	r.Post("/auth/register", auth.Register)
	r.Post("/auth/login", auth.Login)
	// Social sign-in. The mobile SDK has already completed the OAuth flow;
	// these take the resulting ID token and return the same {token, user}
	// envelope as /auth/login.
	r.Post("/auth/google", auth.Google)
	r.Post("/auth/apple", auth.Apple)

	// Protected: everything inside this group requires a valid bearer token.
	// Using a chi.Group rather than a global middleware keeps /health and the
	// two routes above reachable without one — an allow-list of public paths
	// inside a global middleware is the kind of thing that grows a hole.
	r.Group(func(r chi.Router) {
		r.Use(middleware.RequireAuth(deps.Tokens))

		r.Get("/me", auth.Me)

		// Saved cards. These are the only versioned paths in the API so far;
		// the prefix is pinned by the acceptance criteria, and whether the
		// older routes should move under /v1 is a separate decision.
		r.Post("/v1/payment-methods", payments.Create)
		r.Get("/v1/payment-methods", payments.List)

		// Placing a bet. It reads a saved card and puts a hold on it, so it
		// belongs behind the same auth as the routes above.
		r.Post("/v1/bets", bets.Create)
		// Reading the caller's in-flight bet. A single resource rather than a
		// list: the partial unique index bets_user_id_in_flight_key allows at
		// most one pending-or-active bet per user. chi matches /v1/bets and
		// /v1/bets/active as independent static nodes, so neither shadows the
		// other.
		r.Get("/v1/bets/active", bets.Active)
	})

	return r
}
