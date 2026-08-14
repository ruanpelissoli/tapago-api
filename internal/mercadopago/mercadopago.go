// Package mercadopago wraps the two Mercado Pago REST operations this API
// performs server-side: creating a customer, and placing a pre-authorization
// hold on a card.
//
// # What this package deliberately does not do
//
// It never sees a card number. Mercado Pago's POST /v1/card_tokens takes a
// raw PAN and CVV, and calling it from here would pull this service into PCI
// scope — the exact outcome migrations/004_create_payment_methods.sql was
// written to avoid. The mobile SDK tokenises the card on the device and
// forwards the resulting short-lived token; the only card-shaped value that
// ever reaches this package is CardToken.
//
// It also does not use Mercado Pago's Go SDK. As in internal/social, two
// JSON POSTs over net/http do not justify a dependency tree, and hand-rolling
// them keeps the error mapping — which is the hard and important part here —
// under our control.
//
// # Environments
//
// There is one base URL, https://api.mercadopago.com, for both sandbox and
// production; the access token selects which. WithBaseURL exists so tests can
// point at an httptest server and must not be used to reach anything else.
//
// # Errors are the point
//
// A payment integration has three failure modes that look alike over the wire
// and must never be confused:
//
//   - The card was declined. Mercado Pago answers 201 Created with
//     status:"rejected" in the body, so an integration that only checks the
//     HTTP code reads a decline as a success. This package inspects status
//     and returns ErrCardDeclined.
//   - We are broken — a wrong token, a malformed request. ErrAuthentication
//     and ErrInvalidRequest.
//   - Mercado Pago is unreachable. ErrUnavailable. This is the
//     internal/social ErrKeysUnavailable lesson restated: reporting an outage
//     to a user as "your card was declined" sends them to their bank over a
//     problem on our side.
//
// Every error is matchable with errors.Is so a handler can choose a status
// code without parsing strings, and carries an *APIError with Mercado Pago's
// own status/status_detail for the log line.
package mercadopago

import (
	"context"
	"errors"
	"fmt"
	"net/http"
)

// MercadoPagoClient is the behaviour handlers depend on, so they can be
// tested against a fake instead of an httptest server.
type MercadoPagoClient interface {
	// CreateCustomer registers email with Mercado Pago and returns the
	// customer id. A returning user whose address is already registered
	// yields ErrCustomerAlreadyExists, not a generic failure.
	CreateCustomer(ctx context.Context, email string) (customerID string, err error)

	// CreatePreAuth places an authorization hold on the card behind
	// req.CardToken and returns the Mercado Pago payment id. The funds are
	// held, not captured; capturing or releasing them is a later operation
	// and is not implemented here.
	CreatePreAuth(ctx context.Context, req PreAuthRequest) (paymentID string, err error)
}

// PreAuthRequest is one authorization hold.
//
// AmountBRL is Centavos rather than a float: see that type's documentation,
// and the standing rule in internal/model/CLAUDE.md that money is never
// float64.
type PreAuthRequest struct {
	// CardToken is the single-use token the mobile SDK produced from the
	// card. Mercado Pago expires these in minutes and invalidates them on
	// use, so one token cannot be replayed into a second hold.
	CardToken string
	// CustomerID is the Mercado Pago customer the card belongs to. Optional:
	// a one-off card that was never saved has none, and Mercado Pago accepts
	// the payment without it.
	CustomerID string
	// AmountBRL is the amount to hold, in centavos. Must be positive.
	AmountBRL Centavos
	// Description appears on the cardholder's statement and in the Mercado
	// Pago dashboard.
	Description string
	// ExternalReference is our own id for the thing being paid for — the bet
	// id. It is what lets a Mercado Pago record be traced back to a row here,
	// and it seeds the idempotency key, so it must be stable across retries
	// of the same logical operation and distinct between different ones.
	ExternalReference string
	// PayerEmail is the cardholder's address. Mercado Pago requires a payer
	// email on a card payment; it is trimmed and lower-cased before sending,
	// matching how auth normalises addresses.
	PayerEmail string
	// PaymentMethodID is Mercado Pago's identifier for the card brand
	// ("visa", "master", "elo", ...). The mobile SDK returns it alongside the
	// token. Optional — Mercado Pago infers it from the token when omitted —
	// but sending it avoids relying on that inference.
	PaymentMethodID string
}

var (
	// ErrNoAccessToken is returned by New when the access token is blank.
	//
	// Failing at construction is deliberate. A client built without a
	// credential would send unauthenticated requests that Mercado Pago
	// rejects with a 401, which this package would surface as
	// ErrAuthentication — a runtime payment failure standing in for a
	// misconfiguration that was knowable at startup.
	ErrNoAccessToken = errors.New("mercadopago: access token is required")

	// ErrInvalidRequest means the request was rejected as malformed — either
	// by our own validation before any network call, or by Mercado Pago with
	// a 4xx that is not an auth or rate-limit failure. Either way it is our
	// bug, not the user's card: retrying it unchanged cannot help.
	ErrInvalidRequest = errors.New("mercadopago: invalid request")

	// ErrCustomerAlreadyExists means a Mercado Pago customer already holds
	// that email address. Mercado Pago reports it as a 400, but for us it is
	// an ordinary outcome — a returning user re-registering a card — and the
	// caller should look the existing customer up rather than fail. When
	// Mercado Pago includes the existing id, APIError.CustomerID carries it.
	ErrCustomerAlreadyExists = errors.New("mercadopago: customer already exists")

	// ErrCardDeclined means Mercado Pago reached the issuer and the issuer
	// said no: insufficient funds, a bad security code, a blocked card. The
	// HTTP response is a perfectly ordinary 201; the decline lives in the
	// body's status/status_detail. APIError.StatusDetail holds the
	// cc_rejected_* reason a user-facing message should be derived from.
	ErrCardDeclined = errors.New("mercadopago: card declined")

	// ErrAuthentication means Mercado Pago refused our credential (401 or
	// 403). The access token is wrong, revoked, or from the other
	// environment. No user action can fix it and no retry will help.
	ErrAuthentication = errors.New("mercadopago: access token rejected")

	// ErrRateLimited means Mercado Pago returned 429. The request may
	// succeed later; this package does not retry (see CLAUDE.md).
	ErrRateLimited = errors.New("mercadopago: rate limited")

	// ErrUnavailable means we could not get an answer: a network failure, a
	// timeout, a cancelled context, a 5xx, or a response we could not parse.
	//
	// The state of the payment is genuinely unknown — a hold may or may not
	// have been placed. It must never be reported to a user as a decline,
	// and a caller retrying it relies on the idempotency key to avoid a
	// second hold on the same card.
	ErrUnavailable = errors.New("mercadopago: mercado pago unavailable")
)

// APIError carries what Mercado Pago said, for the log line and for mapping a
// decline to a user-facing message. It wraps the matching sentinel, so
// errors.Is(err, ErrCardDeclined) works on it and callers only reach for the
// fields when they want the detail.
//
// It never contains the access token.
type APIError struct {
	// Op is the operation that failed, e.g. "CreatePreAuth".
	Op string
	// HTTPStatus is the response code, or 0 if the request never completed.
	HTTPStatus int
	// Status is the payment's status field ("rejected", "authorized", ...),
	// empty for customer calls and for transport failures.
	Status string
	// StatusDetail is the payment's status_detail — the cc_rejected_* code
	// that says *why* an issuer declined. This is the field a user-facing
	// message should switch on; the rest is for logs.
	StatusDetail string
	// Message and Code are Mercado Pago's own error description and machine
	// code from an error response body.
	Message string
	Code    string
	// CustomerID is the pre-existing customer id, set only on
	// ErrCustomerAlreadyExists and only when Mercado Pago disclosed it.
	CustomerID string
	// PaymentID is Mercado Pago's payment id when the call produced a payment
	// record despite failing — a decline, or a status we do not accept. It is
	// what makes the transaction traceable in the Mercado Pago dashboard, and
	// it is the only handle on a hold that may have been placed anyway.
	PaymentID string

	// sentinel is what errors.Is matches. Unexported so callers compare
	// against the exported sentinels rather than reaching in here.
	sentinel error
}

func (e *APIError) Error() string {
	msg := fmt.Sprintf("mercadopago: %s failed", e.Op)
	if e.HTTPStatus != 0 {
		msg += fmt.Sprintf(" (http %d)", e.HTTPStatus)
	}
	if e.Status != "" {
		msg += fmt.Sprintf(" status=%s", e.Status)
	}
	if e.StatusDetail != "" {
		msg += fmt.Sprintf(" status_detail=%s", e.StatusDetail)
	}
	if e.Code != "" {
		msg += fmt.Sprintf(" code=%s", e.Code)
	}
	if e.Message != "" {
		msg += fmt.Sprintf(": %s", e.Message)
	}
	return msg
}

// Unwrap exposes the sentinel to errors.Is.
func (e *APIError) Unwrap() error { return e.sentinel }

// Retryable reports whether repeating the same call could plausibly produce a
// different answer. A declined card and a malformed request cannot; an outage
// or a rate limit can.
//
// Retrying a pre-auth is only safe because the idempotency key is derived
// from the external reference, so a retry that races a request Mercado Pago
// did in fact process returns the original payment instead of a second hold.
func (e *APIError) Retryable() bool {
	return errors.Is(e.sentinel, ErrUnavailable) || errors.Is(e.sentinel, ErrRateLimited)
}

// sentinelForStatus maps an HTTP status code to the sentinel a caller should
// branch on. It is only consulted for non-2xx responses.
func sentinelForStatus(code int) error {
	switch {
	case code == http.StatusUnauthorized, code == http.StatusForbidden:
		return ErrAuthentication
	case code == http.StatusTooManyRequests:
		return ErrRateLimited
	case code >= 500:
		return ErrUnavailable
	default:
		// Everything else in the 4xx range is a request we built wrong.
		return ErrInvalidRequest
	}
}

// compile-time check that the concrete client satisfies the interface.
var _ MercadoPagoClient = (*Client)(nil)
