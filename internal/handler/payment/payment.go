// Package payment implements the saved-card endpoints: storing a Mercado
// Pago card token as a payment method, and listing the methods a user has
// saved.
//
// It never sees a card number. The mobile SDK tokenises the card on the
// device and this package only ever handles the resulting token plus the
// display metadata needed to render "Visa ···· 4242" — the same boundary
// migrations/004_create_payment_methods.sql and internal/mercadopago draw,
// and what keeps this service out of PCI scope.
//
// Enforcing authentication is the job of internal/middleware.RequireAuth;
// these handlers read the user id off the request context and never inspect
// an Authorization header themselves.
package payment

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/tapago/tapago-api/internal/handler"
	"github.com/tapago/tapago-api/internal/mercadopago"
	"github.com/tapago/tapago-api/internal/middleware"
)

// Client-facing messages. Constants because clients end up matching on the
// exact string, so a typo in one is a breaking change.
const (
	msgInvalidBody      = "request body must be valid JSON"
	msgInvalidCardToken = "card_token is required"
	msgInvalidLastFour  = "last_four must be exactly four digits"
	msgInvalidCardBrand = "card_brand is required"
	msgUnauthorized     = "unauthorized"
	msgInternal         = "internal server error"
	// msgProviderUnavailable covers every "we could not get an answer from
	// Mercado Pago" case. It deliberately says nothing about the card: an
	// outage reported as a decline sends users to their bank over our
	// problem (the internal/mercadopago ErrUnavailable rule).
	msgProviderUnavailable = "payment provider unavailable"
)

const (
	// The body carries three short strings; anything larger is not a request
	// we intend to serve.
	maxBodyBytes = 8 << 10
	// maxCardTokenLen bounds the token so junk never reaches Mercado Pago.
	// Real tokens are 32 hex characters; this is generous on purpose.
	maxCardTokenLen = 512
	// maxCardBrandLen bounds the brand slug ("visa", "master", "elo", ...).
	maxCardBrandLen = 40
	// lastFourLen mirrors the last_four ~ '^[0-9]{4}$' CHECK in migration
	// 004. Validating it here turns what the database would raise as a
	// constraint violation (a 500) into the 400 it actually is.
	lastFourLen = 4
)

// uniqueViolation is the SQLSTATE Postgres raises for a duplicate key.
//
// Two concurrent first cards both evaluate the insert's NOT EXISTS as true,
// so the partial unique index payment_methods_user_id_default_key is the
// source of truth for "one default per user" — exactly as the unique email
// index is in internal/handler/auth. There is no pre-check that could close
// that race.
const uniqueViolation = "23505"

// DB is the slice of the pgx pool these handlers use. Depending on an
// interface rather than *pgxpool.Pool is what lets the tests run without a
// live database. Both methods are needed: creating a card returns one row,
// listing them returns many.
type DB interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// Handler holds the dependencies of the payment-method endpoints.
type Handler struct {
	db DB
	mp mercadopago.MercadoPagoClient
}

// NewHandler wires a payment handler.
//
// A nil client is valid: the deployment has no MERCADOPAGO_ACCESS_TOKEN, and
// the routes stay mounted and answer 503 rather than disappearing from the
// URL surface — the same call as a nil social verifier. A route that appears
// and vanishes with the environment is far harder to debug than one that
// returns a clear server error.
func NewHandler(db DB, mp mercadopago.MercadoPagoClient) *Handler {
	return &Handler{db: db, mp: mp}
}

type createRequest struct {
	CardToken string `json:"card_token"`
	LastFour  string `json:"last_four"`
	CardBrand string `json:"card_brand"`
}

// paymentMethodResponse is the only shape a saved card is ever serialised
// in. It has no mp_card_token and no mp_customer_id field at all, which is a
// stronger guarantee than `json:"-"`: there is nothing to accidentally
// un-hide. The token is a live credential and the customer id is provider
// internal; neither is any of a client's business. Same argument as
// userResponse having no password field.
type paymentMethodResponse struct {
	ID        string    `json:"id"`
	LastFour  string    `json:"last_four"`
	CardBrand string    `json:"card_brand"`
	IsDefault bool      `json:"is_default"`
	CreatedAt time.Time `json:"created_at"`
}

// listResponse wraps the collection in an object rather than returning a
// top-level JSON array: it leaves room for pagination metadata without a
// breaking change.
type listResponse struct {
	PaymentMethods []paymentMethodResponse `json:"payment_methods"`
}

const selectCustomerIDSQL = `
SELECT mp_customer_id
FROM payment_methods
WHERE user_id = $1::uuid
LIMIT 1`

const selectUserEmailSQL = `
SELECT email
FROM users
WHERE id = $1::uuid`

// insertPaymentMethodSQL computes is_default in the same statement that
// inserts the row: the first card a user saves becomes the default, later
// ones do not. Doing it in SQL removes the read-then-write window a
// SELECT-then-INSERT would leave open.
const insertPaymentMethodSQL = `
INSERT INTO payment_methods (user_id, mp_card_token, mp_customer_id, last_four, card_brand, is_default)
VALUES ($1::uuid, $2, $3, $4, $5, NOT EXISTS (SELECT 1 FROM payment_methods WHERE user_id = $1::uuid))
RETURNING id::text, last_four, card_brand, is_default, created_at`

// insertNonDefaultPaymentMethodSQL is the retry after the partial unique
// index rejected a concurrent second default. The card is still saved; it
// just is not the default one.
const insertNonDefaultPaymentMethodSQL = `
INSERT INTO payment_methods (user_id, mp_card_token, mp_customer_id, last_four, card_brand, is_default)
VALUES ($1::uuid, $2, $3, $4, $5, false)
RETURNING id::text, last_four, card_brand, is_default, created_at`

// selectPaymentMethodsSQL orders the default card first, then newest first,
// which is the order a client renders them in.
const selectPaymentMethodsSQL = `
SELECT id::text, last_four, card_brand, is_default, created_at
FROM payment_methods
WHERE user_id = $1::uuid
ORDER BY is_default DESC, created_at DESC`

// Create saves a Mercado Pago card token as a payment method.
//
// POST /v1/payment-methods  {"card_token": ..., "last_four": ..., "card_brand": ...}
func (h *Handler) Create(w http.ResponseWriter, r *http.Request) {
	userID, ok := authenticatedUserID(w, r)
	if !ok {
		return
	}

	var req createRequest
	if !decodeJSON(w, r, &req) {
		return
	}

	// Validate everything before touching a dependency: a bad request must
	// cost neither a query nor a call to Mercado Pago.
	cardToken := strings.TrimSpace(req.CardToken)
	if cardToken == "" || len(cardToken) > maxCardTokenLen {
		handler.Error(w, http.StatusBadRequest, msgInvalidCardToken)
		return
	}
	lastFour := strings.TrimSpace(req.LastFour)
	if !isFourDigits(lastFour) {
		handler.Error(w, http.StatusBadRequest, msgInvalidLastFour)
		return
	}
	cardBrand := strings.ToLower(strings.TrimSpace(req.CardBrand))
	if cardBrand == "" || len(cardBrand) > maxCardBrandLen {
		handler.Error(w, http.StatusBadRequest, msgInvalidCardBrand)
		return
	}

	customerID, ok := h.resolveCustomerID(w, r, userID)
	if !ok {
		return
	}

	saved, err := h.insert(r.Context(), insertPaymentMethodSQL, userID, cardToken, customerID, lastFour, cardBrand)
	if err != nil && isUniqueViolation(err) {
		// Another request for the same user won the race to be the default.
		// The index, not a pre-flight SELECT, is what settles it; this card
		// is saved as a non-default one instead.
		slog.Info("create payment method: default already taken, saving as non-default", "user_id", userID)
		saved, err = h.insert(r.Context(), insertNonDefaultPaymentMethodSQL, userID, cardToken, customerID, lastFour, cardBrand)
	}
	if err != nil {
		slog.Error("create payment method: insert", "user_id", userID, "error", err)
		handler.Error(w, http.StatusInternalServerError, msgInternal)
		return
	}

	handler.JSON(w, http.StatusCreated, saved)
}

// List returns the authenticated user's saved payment methods.
//
// GET /v1/payment-methods
func (h *Handler) List(w http.ResponseWriter, r *http.Request) {
	userID, ok := authenticatedUserID(w, r)
	if !ok {
		return
	}

	rows, err := h.db.Query(r.Context(), selectPaymentMethodsSQL, userID)
	if err != nil {
		slog.Error("list payment methods: query", "user_id", userID, "error", err)
		handler.Error(w, http.StatusInternalServerError, msgInternal)
		return
	}
	defer rows.Close()

	// Allocated, not nil: a nil slice marshals to `null`, and a client
	// iterating the field would have to special-case it.
	methods := make([]paymentMethodResponse, 0)
	for rows.Next() {
		var m paymentMethodResponse
		if err := rows.Scan(&m.ID, &m.LastFour, &m.CardBrand, &m.IsDefault, &m.CreatedAt); err != nil {
			slog.Error("list payment methods: scan", "user_id", userID, "error", err)
			handler.Error(w, http.StatusInternalServerError, msgInternal)
			return
		}
		methods = append(methods, m)
	}
	// A failure part-way through streaming only shows up here; without this
	// check a truncated list would be served as a complete one.
	if err := rows.Err(); err != nil {
		slog.Error("list payment methods: read rows", "user_id", userID, "error", err)
		handler.Error(w, http.StatusInternalServerError, msgInternal)
		return
	}

	handler.JSON(w, http.StatusOK, listResponse{PaymentMethods: methods})
}

// insert writes one row and scans the saved record back out.
func (h *Handler) insert(ctx context.Context, sql, userID, cardToken, customerID, lastFour, cardBrand string) (paymentMethodResponse, error) {
	var saved paymentMethodResponse
	err := h.db.QueryRow(ctx, sql, userID, cardToken, customerID, lastFour, cardBrand).
		Scan(&saved.ID, &saved.LastFour, &saved.CardBrand, &saved.IsDefault, &saved.CreatedAt)
	return saved, err
}

// resolveCustomerID returns the Mercado Pago customer this user's cards
// belong to, creating one if this is their first card. It writes the
// response and returns false when it could not.
func (h *Handler) resolveCustomerID(w http.ResponseWriter, r *http.Request, userID string) (string, bool) {
	ctx := r.Context()

	var customerID string
	err := h.db.QueryRow(ctx, selectCustomerIDSQL, userID).Scan(&customerID)
	switch {
	case err == nil:
		// The user already has a card, so Mercado Pago already knows them.
		// Reusing the stored id is not just an optimisation: creating a
		// second customer for the same address is what MP rejects with the
		// duplicate error we cannot recover from.
		return customerID, true
	case !errors.Is(err, pgx.ErrNoRows):
		slog.Error("create payment method: select customer id", "user_id", userID, "error", err)
		handler.Error(w, http.StatusInternalServerError, msgInternal)
		return "", false
	}

	var email string
	err = h.db.QueryRow(ctx, selectUserEmailSQL, userID).Scan(&email)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		// A valid token for a user that no longer exists: the credential is
		// unusable, so 401 rather than 404 — the auth.Me precedent.
		handler.Error(w, http.StatusUnauthorized, msgUnauthorized)
		return "", false
	case err != nil:
		slog.Error("create payment method: select user email", "user_id", userID, "error", err)
		handler.Error(w, http.StatusInternalServerError, msgInternal)
		return "", false
	}

	// No access token configured. That is our misconfiguration, not a card
	// problem, and the route still exists so the gap is visible rather than
	// looking like a wrong path.
	if h.mp == nil {
		slog.Error("create payment method: mercado pago is not configured", "user_id", userID)
		handler.Error(w, http.StatusServiceUnavailable, msgProviderUnavailable)
		return "", false
	}

	customerID, err = h.mp.CreateCustomer(ctx, email)
	if err == nil {
		return customerID, true
	}
	return reportCustomerError(w, userID, err)
}

// reportCustomerError maps a Mercado Pago failure onto a status code and
// writes it, so the mapping lives in one readable place. It returns a
// usable customer id only on the one branch that can recover — a duplicate
// customer whose id Mercado Pago happened to disclose.
//
// Nothing Mercado Pago said reaches the client: the sentinel picks the
// status, the detail goes to the log.
func reportCustomerError(w http.ResponseWriter, userID string, err error) (string, bool) {
	switch {
	case errors.Is(err, mercadopago.ErrCustomerAlreadyExists):
		// A returning user, normally after their last card was deleted. If
		// Mercado Pago disclosed the existing id we can carry on with it,
		// but it usually does not (see internal/mercadopago/CLAUDE.md: there
		// is no such field on a 101) and we have no stored id either, since
		// the lookup above found no row. Recovering it needs
		// GET /v1/customers/search, which the client does not implement, so
		// this is a genuine server-side gap rather than anything the caller
		// can fix.
		var apiErr *mercadopago.APIError
		if errors.As(err, &apiErr) && apiErr.CustomerID != "" {
			return apiErr.CustomerID, true
		}
		slog.Error("create payment method: customer exists but its id is unknown", "user_id", userID, "error", err)
		handler.Error(w, http.StatusInternalServerError, msgInternal)

	case errors.Is(err, mercadopago.ErrUnavailable), errors.Is(err, mercadopago.ErrRateLimited):
		// We could not get an answer. Retrying may work, and it must never
		// be presented to the user as a problem with their card.
		slog.Error("create payment method: mercado pago unavailable", "user_id", userID, "error", err)
		handler.Error(w, http.StatusServiceUnavailable, msgProviderUnavailable)

	default:
		// ErrAuthentication (a wrong or revoked access token) and
		// ErrInvalidRequest (a request we built wrong) are both our fault,
		// and so is anything unrecognised.
		slog.Error("create payment method: create customer", "user_id", userID, "error", err)
		handler.Error(w, http.StatusInternalServerError, msgInternal)
	}
	return "", false
}

// authenticatedUserID reads the user id RequireAuth put on the context and
// checks its shape, writing a 401 and returning false if either fails.
func authenticatedUserID(w http.ResponseWriter, r *http.Request) (string, bool) {
	userID, ok := middleware.UserID(r.Context())
	if !ok {
		// Only reachable if a route is mounted outside RequireAuth.
		unauthorized(w)
		return "", false
	}
	// The subject is signed by us but still arrives as a client-supplied
	// string. Checking the shape here keeps a malformed value from reaching
	// Postgres as an invalid uuid cast (SQLSTATE 22P02 -> a 500).
	if !isUUID(userID) {
		unauthorized(w)
		return "", false
	}
	return userID, true
}

func unauthorized(w http.ResponseWriter) {
	// RFC 7235 requires a challenge on a 401, and RequireAuth sets the same
	// header on its own rejections.
	w.Header().Set("WWW-Authenticate", "Bearer")
	handler.Error(w, http.StatusUnauthorized, msgUnauthorized)
}

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == uniqueViolation
}

// decodeJSON reads the request body into dst, writing a 400 and returning
// false if it cannot. The body is capped so a large upload cannot exhaust
// memory before validation ever runs.
func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)

	if err := json.NewDecoder(r.Body).Decode(dst); err != nil {
		// A decode error can quote the body, which holds a live card token.
		// Only the fact that decoding failed is safe to record.
		slog.Debug("payment: decode request body failed")
		handler.Error(w, http.StatusBadRequest, msgInvalidBody)
		return false
	}
	return true
}

// isFourDigits mirrors the last_four CHECK from migration 004. It stays a
// string rather than an int because "0042" is a valid last-four and would
// lose its leading zeros as a number.
func isFourDigits(s string) bool {
	if len(s) != lastFourLen {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// isUUID reports whether s is a canonical 8-4-4-4-12 hex UUID.
//
// Duplicated from internal/handler/auth rather than exported from it: that
// package documents itself as handling credentials only, and widening its
// API for twenty lines of syntax gating would be the worse trade. This is a
// shape check, not a parser — Postgres does the actual parsing.
func isUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i := 0; i < len(s); i++ {
		switch i {
		case 8, 13, 18, 23:
			if s[i] != '-' {
				return false
			}
		default:
			if !isHexDigit(s[i]) {
				return false
			}
		}
	}
	return true
}

func isHexDigit(c byte) bool {
	return (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
}
