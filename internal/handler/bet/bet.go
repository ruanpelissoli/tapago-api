// Package bet implements the bet endpoints: today only creating one, which
// is the operation that puts a user's money at stake.
//
// # Why there is no transaction around the Mercado Pago call
//
// The obvious shape — BEGIN, insert the bet, call Mercado Pago, update the
// row, COMMIT — is wrong here for two independent reasons:
//
//   - PreAuthRequest.ExternalReference must be the bet id, and it seeds the
//     idempotency key. An uncommitted id is not yet a fact anything can be
//     reconciled against.
//   - A rollback after Mercado Pago succeeded would erase our record of a
//     hold that exists on a real person's card. The database would be tidy
//     and the money would still be gone.
//
// So the shape is: insert 'pending' -> call Mercado Pago -> settle the row.
// A row can therefore be left 'pending' when the provider's answer never
// arrives, which is deliberate — see Create.
//
// Enforcing authentication is the job of internal/middleware.RequireAuth;
// these handlers read the user id off the request context and never inspect
// an Authorization header themselves.
package bet

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
	"github.com/tapago/tapago-api/internal/model"
)

// Client-facing messages. Constants because clients end up matching on the
// exact string, so a typo in one is a breaking change.
const (
	msgInvalidBody           = "request body must be valid JSON"
	msgInvalidGoalType       = "goal_type must be one of: exercise, no_smoking"
	msgInvalidTargetDays     = "target_days must be between 1 and 365"
	msgInvalidStake          = "stake_amount_brl must be a positive amount with at most two decimal places, up to 10000.00"
	msgInvalidPaymentMethod  = "payment_method_id must be a valid uuid"
	msgPaymentMethodNotFound = "payment method not found"
	msgBetInFlight           = "you already have a bet in progress"
	msgCardDeclined          = "card declined"
	msgUnauthorized          = "unauthorized"
	msgInternal              = "internal server error"
	// msgProviderUnavailable is the same string handler/payment uses, so a
	// client sees one wording for "we could not get an answer from Mercado
	// Pago". It deliberately says nothing about the card: an outage reported
	// as a decline sends users to their bank over our problem (the
	// internal/mercadopago ErrUnavailable rule).
	msgProviderUnavailable = "payment provider unavailable"
)

const (
	// The body carries four short values; anything larger is not a request we
	// intend to serve.
	maxBodyBytes = 8 << 10
	// maxTargetDays is an assumed cap — nothing in the schema or the brief
	// fixes an upper bound (003_create_bets.sql only CHECKs target_days > 0).
	// A year is the longest habit window that makes product sense today; it
	// is a named constant precisely so raising it is a one-line edit.
	maxTargetDays = 365
	// betDescription is what the cardholder sees on their statement and in
	// the Mercado Pago dashboard.
	betDescription = "TaPago bet stake"
)

// maxStakeBRL is the other assumed cap: R$ 10.000,00. The column is
// numeric(12,2) and only CHECKs > 0, so this is a product limit rather than a
// storage one — a guard against a fat-fingered stake becoming a real hold.
var maxStakeBRL = mercadopago.BRL(10_000, 0)

// uniqueViolation is the SQLSTATE Postgres raises for a duplicate key.
//
// On this path it means the partial unique index bets_user_id_in_flight_key
// rejected a second in-flight bet. That index — not the pre-check below — is
// the one-active-bet rule: two concurrent requests both see no row and both
// try to insert, and only the index makes the second one fail.
const uniqueViolation = "23505"

// DB is the slice of the pgx pool this handler uses. Depending on an
// interface rather than *pgxpool.Pool is what lets the tests run without a
// live database. QueryRow covers the lookups and the RETURNING insert; Exec
// covers the settling update, which has nothing to read back.
type DB interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// Handler holds the dependencies of the bet endpoints.
type Handler struct {
	db DB
	mp mercadopago.MercadoPagoClient
}

// NewHandler wires a bet handler.
//
// A nil client is valid: the deployment has no MERCADOPAGO_ACCESS_TOKEN, and
// the route stays mounted and answers 503 rather than disappearing from the
// URL surface — the same call as handler/payment. A route that appears and
// vanishes with the environment is far harder to debug than one that returns
// a clear server error.
func NewHandler(db DB, mp mercadopago.MercadoPagoClient) *Handler {
	return &Handler{db: db, mp: mp}
}

type createRequest struct {
	GoalType   string `json:"goal_type"`
	TargetDays int    `json:"target_days"`
	// StakeAmountBRL is json.Number, never float64. The standing rule in
	// internal/model/CLAUDE.md is that money never touches a binary float,
	// and this is the path it was written for: R$ 19,99 through a float64 can
	// come back out as 19.989999999999998 and place a hold for an amount the
	// bet row does not record. json.Number accepts both "50.00" and 50.00 and
	// keeps the raw text either way; mercadopago.ParseBRL turns it into exact
	// centavos and rejects more than two decimal places.
	//
	// A comma decimal ("50,00") is rejected at decode time even though
	// ParseBRL would accept one: json.Number requires valid JSON number
	// syntax, quoted or not. The wire format is a dot, so that is the right
	// answer here — the comma support exists for Postgres numeric text.
	StakeAmountBRL json.Number `json:"stake_amount_brl"`
	// PaymentMethodID is *our* payment_methods.id — not Mercado Pago's
	// payment_method_id, which is the card brand ("visa", "master", ...).
	// The name collision is real and unavoidable: the client sends us a row
	// id, and the brand we send to Mercado Pago is read out of that row's
	// card_brand column.
	PaymentMethodID string `json:"payment_method_id"`
}

// betResponse is the only shape a bet is ever serialised in.
//
// It has no mp_preauth_id field at all, which is stronger than `json:"-"`:
// there is nothing to accidentally un-hide. The pre-authorisation id is
// provider internal and no client's business — the same call as
// mp_customer_id being absent from paymentMethodResponse.
//
// StakeAmountBRL is a JSON *string* ("50.00") rather than a number, for the
// same reason it is never a float64 here: a client parsing 50.00 into a
// double re-introduces exactly the imprecision this path avoids.
type betResponse struct {
	ID             string    `json:"id"`
	GoalType       string    `json:"goal_type"`
	TargetDays     int       `json:"target_days"`
	StakeAmountBRL string    `json:"stake_amount_brl"`
	Status         string    `json:"status"`
	CreatedAt      time.Time `json:"created_at"`
}

// selectInFlightBetSQL is a fast-fail optimisation, not the guarantee. It
// spares a user who already has a bet the card lookup and the provider call;
// the unique index is what actually enforces the rule.
const selectInFlightBetSQL = `
SELECT 1
FROM bets
WHERE user_id = $1::uuid AND status IN ('pending', 'active')
LIMIT 1`

// selectCardSQL loads everything the pre-auth needs in one round trip. The
// join is for users.email: Mercado Pago requires a payer email on a card
// payment.
//
// The pm.user_id = $2 predicate is the authorisation check. A row belonging
// to someone else and a row that does not exist both come back as
// pgx.ErrNoRows and both answer 404 — distinguishing them would tell a caller
// which card ids exist.
const selectCardSQL = `
SELECT pm.mp_card_token, pm.mp_customer_id, pm.card_brand, u.email
FROM payment_methods pm
JOIN users u ON u.id = pm.user_id
WHERE pm.id = $1::uuid AND pm.user_id = $2::uuid`

// insertBetSQL creates the row before Mercado Pago is called, so the bet id
// exists to be used as the external reference.
const insertBetSQL = `
INSERT INTO bets (user_id, goal_type, target_days, stake_amount_brl, status)
VALUES ($1::uuid, $2, $3, $4::numeric, 'pending')
RETURNING id::text, created_at`

const activateBetSQL = `
UPDATE bets
SET status = 'active', mp_preauth_id = $2, updated_at = now()
WHERE id = $1::uuid`

const cancelBetSQL = `
UPDATE bets
SET status = 'cancelled', updated_at = now()
WHERE id = $1::uuid`

// card is the row selectCardSQL returns.
type card struct {
	token      string
	customerID string
	brand      string
	payerEmail string
}

// Create places a bet and holds its stake on the caller's saved card.
//
// POST /v1/bets  {"goal_type":…,"target_days":…,"stake_amount_brl":"50.00","payment_method_id":…}
//
// The outcome of the Mercado Pago call decides both the stored status and the
// response:
//
//	success                        -> active,   201
//	ErrCardDeclined                -> cancelled, 402
//	ErrUnavailable/ErrRateLimited  -> pending,   503
//	anything else                  -> cancelled, 500
//
// The pending branch is the subtle one and is deliberate: after an outage the
// hold's state is genuinely unknown and it may exist. Cancelling the bet
// would free the user's in-flight slot and let them place a *second* real
// hold on the same card. Leaving it pending blocks new bets until someone
// reconciles it, which is the safe side of that trade.
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
	goalType := model.GoalType(strings.TrimSpace(req.GoalType))
	if goalType != model.GoalTypeExercise && goalType != model.GoalTypeNoSmoking {
		// bets.goal_type has no CHECK yet (the taxonomy is unsettled), so
		// this handler is the only thing standing between a typo and a goal
		// nothing can ever resolve.
		handler.Error(w, http.StatusBadRequest, msgInvalidGoalType)
		return
	}
	if req.TargetDays < 1 || req.TargetDays > maxTargetDays {
		handler.Error(w, http.StatusBadRequest, msgInvalidTargetDays)
		return
	}
	stake, err := mercadopago.ParseBRL(string(req.StakeAmountBRL))
	if err != nil || stake <= 0 || stake > maxStakeBRL {
		// ParseBRL's message quotes the amount; the client already knows what
		// it sent, and the generic message keeps this in line with the other
		// validation errors.
		handler.Error(w, http.StatusBadRequest, msgInvalidStake)
		return
	}
	// Checked here so a malformed value never reaches Postgres as an invalid
	// uuid cast (SQLSTATE 22P02 -> a 500 for what is a 400).
	if !isUUID(req.PaymentMethodID) {
		handler.Error(w, http.StatusBadRequest, msgInvalidPaymentMethod)
		return
	}

	// No access token configured. That is our misconfiguration, not a card
	// problem — and catching it here means no bet row is written for a hold
	// that was never even attempted.
	if h.mp == nil {
		slog.Error("create bet: mercado pago is not configured", "user_id", userID)
		handler.Error(w, http.StatusServiceUnavailable, msgProviderUnavailable)
		return
	}

	ctx := r.Context()

	if !h.checkNoBetInFlight(ctx, w, userID) {
		return
	}

	saved, ok := h.loadCard(ctx, w, req.PaymentMethodID, userID)
	if !ok {
		return
	}

	betID, createdAt, ok := h.insertPending(ctx, w, userID, goalType, req.TargetDays, stake)
	if !ok {
		return
	}

	preauthID, err := h.mp.CreatePreAuth(ctx, mercadopago.PreAuthRequest{
		CardToken:  saved.token,
		CustomerID: saved.customerID,
		AmountBRL:  stake,
		// The bet id, not a random value: it seeds the idempotency key, so a
		// retry of this same bet returns the original hold instead of placing
		// a second one.
		ExternalReference: betID,
		Description:       betDescription,
		PayerEmail:        saved.payerEmail,
		PaymentMethodID:   saved.brand,
	})
	if err != nil {
		h.settleFailure(ctx, w, userID, betID, err)
		return
	}

	if _, err := h.db.Exec(ctx, activateBetSQL, betID, preauthID); err != nil {
		// The hold exists and we could not record it. Both ids go to the log
		// because they are the only handle on it; the row is recoverable from
		// this line and nothing else.
		slog.Error("create bet: activate after successful pre-auth",
			"user_id", userID, "bet_id", betID, "mp_preauth_id", preauthID, "error", err)
		handler.Error(w, http.StatusInternalServerError, msgInternal)
		return
	}

	handler.JSON(w, http.StatusCreated, betResponse{
		ID:             betID,
		GoalType:       string(goalType),
		TargetDays:     req.TargetDays,
		StakeAmountBRL: stake.String(),
		Status:         string(model.BetStatusActive),
		CreatedAt:      createdAt,
	})
}

// checkNoBetInFlight is the fast-fail pre-check. It writes the response and
// returns false when the user already has a bet, or when the query failed.
func (h *Handler) checkNoBetInFlight(ctx context.Context, w http.ResponseWriter, userID string) bool {
	var one int
	err := h.db.QueryRow(ctx, selectInFlightBetSQL, userID).Scan(&one)
	switch {
	case err == nil:
		handler.Error(w, http.StatusConflict, msgBetInFlight)
		return false
	case errors.Is(err, pgx.ErrNoRows):
		return true
	default:
		slog.Error("create bet: select in-flight bet", "user_id", userID, "error", err)
		handler.Error(w, http.StatusInternalServerError, msgInternal)
		return false
	}
}

// loadCard reads the caller's card and email. It writes the response and
// returns false when the card is not theirs, does not exist, or cannot be
// read.
func (h *Handler) loadCard(ctx context.Context, w http.ResponseWriter, paymentMethodID, userID string) (card, bool) {
	var c card
	err := h.db.QueryRow(ctx, selectCardSQL, paymentMethodID, userID).
		Scan(&c.token, &c.customerID, &c.brand, &c.payerEmail)
	switch {
	case err == nil:
		return c, true
	case errors.Is(err, pgx.ErrNoRows):
		// "not yours" and "does not exist" answer the same, on purpose:
		// telling them apart turns this endpoint into an oracle for other
		// users' card ids.
		handler.Error(w, http.StatusNotFound, msgPaymentMethodNotFound)
	default:
		slog.Error("create bet: select payment method", "user_id", userID, "error", err)
		handler.Error(w, http.StatusInternalServerError, msgInternal)
	}
	return card{}, false
}

// insertPending writes the bet before Mercado Pago is called. It writes the
// response and returns false on failure.
func (h *Handler) insertPending(
	ctx context.Context, w http.ResponseWriter,
	userID string, goalType model.GoalType, targetDays int, stake mercadopago.Centavos,
) (string, time.Time, bool) {
	var (
		betID     string
		createdAt time.Time
	)
	// stake.String() renders exact centavos as "50.00" — the text form
	// numeric(12,2) accepts — so the stored stake and the hold are the same
	// number by construction.
	err := h.db.QueryRow(ctx, insertBetSQL, userID, string(goalType), targetDays, stake.String()).
		Scan(&betID, &createdAt)
	switch {
	case err == nil:
		return betID, createdAt, true
	case isUniqueViolation(err):
		// bets_user_id_in_flight_key. The pre-check above missed it because a
		// concurrent request inserted between the two statements — this is
		// the branch that actually enforces one bet at a time.
		slog.Info("create bet: lost the in-flight race", "user_id", userID)
		handler.Error(w, http.StatusConflict, msgBetInFlight)
	default:
		slog.Error("create bet: insert", "user_id", userID, "error", err)
		handler.Error(w, http.StatusInternalServerError, msgInternal)
	}
	return "", time.Time{}, false
}

// settleFailure maps a Mercado Pago failure onto a bet status and an HTTP
// status, and writes both, so the mapping lives in one readable place.
//
// Nothing Mercado Pago said reaches the client: the sentinel picks the
// status, the detail goes to the log.
func (h *Handler) settleFailure(ctx context.Context, w http.ResponseWriter, userID, betID string, err error) {
	switch {
	case errors.Is(err, mercadopago.ErrCardDeclined):
		// The issuer refused. The bet never starts, and the slot is freed
		// immediately so the user can retry with another card.
		slog.Info("create bet: card declined", "user_id", userID, "bet_id", betID, "error", err)
		h.cancel(ctx, userID, betID)
		handler.Error(w, http.StatusPaymentRequired, msgCardDeclined)

	case errors.Is(err, mercadopago.ErrUnavailable), errors.Is(err, mercadopago.ErrRateLimited):
		// We could not get an answer, so the hold may well exist. The row
		// stays 'pending' on purpose: cancelling it would free the in-flight
		// slot and let the user place a second real hold on the same card.
		// The cost is that they are blocked from new bets until this is
		// reconciled — a follow-up, see CLAUDE.md.
		attrs := []any{"user_id", userID, "bet_id", betID, "error", err}
		var apiErr *mercadopago.APIError
		if errors.As(err, &apiErr) && apiErr.PaymentID != "" {
			// The only handle on a hold that may have been placed anyway.
			attrs = append(attrs, "mp_payment_id", apiErr.PaymentID)
		}
		slog.Error("create bet: mercado pago unavailable, bet left pending", attrs...)
		handler.Error(w, http.StatusServiceUnavailable, msgProviderUnavailable)

	default:
		// ErrAuthentication (a wrong or revoked access token) and
		// ErrInvalidRequest (a request we built wrong) are both our fault,
		// and so is anything unrecognised. No hold was placed, so the bet is
		// cancelled and the user is not left blocked by our bug.
		slog.Error("create bet: pre-authorisation failed", "user_id", userID, "bet_id", betID, "error", err)
		h.cancel(ctx, userID, betID)
		handler.Error(w, http.StatusInternalServerError, msgInternal)
	}
}

// cancel settles a bet that will never run. A failure here is logged and
// swallowed: the caller is already writing an error response, and the row is
// recoverable from the log line.
func (h *Handler) cancel(ctx context.Context, userID, betID string) {
	if _, err := h.db.Exec(ctx, cancelBetSQL, betID); err != nil {
		slog.Error("create bet: cancel", "user_id", userID, "bet_id", betID, "error", err)
	}
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
		// A decode error can quote the body. Only the fact that decoding
		// failed is safe to record — the handler/payment rule, kept here so
		// the two packages do not drift.
		slog.Debug("bet: decode request body failed")
		handler.Error(w, http.StatusBadRequest, msgInvalidBody)
		return false
	}
	return true
}

// isUUID reports whether s is a canonical 8-4-4-4-12 hex UUID.
//
// Duplicated from handler/payment rather than exported from it, for the same
// reason it was duplicated there: a feature package should not grow a public
// API for twenty lines of syntax gating. This is a shape check, not a parser
// — Postgres does the actual parsing.
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
