package mercadopago

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const (
	// DefaultBaseURL serves both the sandbox and production; the access token
	// decides which one a request lands in. There is no second host to
	// configure, and no way to accidentally point a production token at a
	// test environment.
	DefaultBaseURL = "https://api.mercadopago.com"

	// defaultTimeout bounds a whole call. Ten seconds matches internal/social
	// and is chosen for the same reason: a person is waiting on the other end
	// of the request that triggered it.
	defaultTimeout = 10 * time.Second

	// maxResponseBytes caps how much of a response we read. Real payment and
	// customer documents are a few kilobytes; the cap only stops a broken or
	// hostile endpoint from streaming us out of memory.
	maxResponseBytes = 1 << 20
)

// Payment statuses Mercado Pago can report on POST /v1/payments.
const (
	statusAuthorized = "authorized"
	statusRejected   = "rejected"
	statusCancelled  = "cancelled"
)

// Client talks to the Mercado Pago REST API.
//
// It is safe for concurrent use: every field is set at construction and never
// written afterwards, and http.Client is itself concurrency-safe. Build one
// per process and share it.
type Client struct {
	accessToken string
	baseURL     string
	httpClient  *http.Client
}

// Option customises a Client. As in internal/social, options exist mainly so
// tests can substitute a local server and a controlled HTTP client.
type Option func(*Client)

// WithHTTPClient overrides the HTTP client, and with it the timeout and any
// transport-level instrumentation.
func WithHTTPClient(c *http.Client) Option {
	return func(cl *Client) {
		if c != nil {
			cl.httpClient = c
		}
	}
}

// WithBaseURL points the client at a different host.
//
// This exists for tests. Production code must use DefaultBaseURL: the access
// token is sent as a bearer credential on every request, so redirecting the
// client hands a live payment credential to whoever runs the other host.
func WithBaseURL(rawURL string) Option {
	return func(cl *Client) {
		if u := strings.TrimRight(strings.TrimSpace(rawURL), "/"); u != "" {
			cl.baseURL = u
		}
	}
}

// New builds a Client authenticated with accessToken.
//
// A blank token is refused with ErrNoAccessToken rather than accepted. A
// client that cannot possibly succeed should say so at startup, where the
// operator is looking, instead of turning every payment into a 401 later.
func New(accessToken string, opts ...Option) (*Client, error) {
	token := strings.TrimSpace(accessToken)
	if token == "" {
		return nil, ErrNoAccessToken
	}

	c := &Client{
		accessToken: token,
		baseURL:     DefaultBaseURL,
		httpClient:  &http.Client{Timeout: defaultTimeout},
	}
	for _, opt := range opts {
		opt(c)
	}
	return c, nil
}

// CreateCustomer registers email as a Mercado Pago customer and returns the
// customer id.
//
// The address is trimmed and lower-cased first, matching how auth normalises
// it, so the same user cannot end up with two customers differing only in
// case. An address Mercado Pago already knows produces ErrCustomerAlreadyExists.
func (c *Client) CreateCustomer(ctx context.Context, email string) (string, error) {
	const op = "CreateCustomer"

	normalised := strings.ToLower(strings.TrimSpace(email))
	if normalised == "" {
		return "", fmt.Errorf("%w: %s: email is empty", ErrInvalidRequest, op)
	}

	body := struct {
		Email string `json:"email"`
	}{Email: normalised}

	// Keyed on the address, so a retry of "register this user" resolves to
	// the same customer instead of a second one.
	resp, err := c.post(ctx, op, "/v1/customers", body, idempotencyKey("customer", normalised))
	if err != nil {
		return "", err
	}

	if resp.status < 200 || resp.status > 299 {
		return "", c.customerFailure(op, resp)
	}

	var decoded struct {
		ID flexibleID `json:"id"`
	}
	if err := json.Unmarshal(resp.body, &decoded); err != nil {
		// Mercado Pago accepted the request but we cannot read the answer, so
		// the customer may well exist. That is an unknown outcome, not a
		// rejection.
		return "", &APIError{Op: op, HTTPStatus: resp.status, Message: "response body is not valid JSON", sentinel: ErrUnavailable}
	}
	if decoded.ID == "" {
		return "", &APIError{Op: op, HTTPStatus: resp.status, Message: "response carried no customer id", sentinel: ErrUnavailable}
	}
	return string(decoded.ID), nil
}

// customerFailure turns a non-2xx customer response into the right sentinel,
// singling out "this email is already registered" from real failures.
func (c *Client) customerFailure(op string, resp *response) error {
	mpErr := parseErrorBody(resp.body)

	apiErr := &APIError{
		Op:         op,
		HTTPStatus: resp.status,
		Message:    mpErr.Message,
		Code:       mpErr.Code,
		sentinel:   sentinelForStatus(resp.status),
	}

	// Mercado Pago reports a duplicate as a plain 400, which would otherwise
	// be indistinguishable from a request we built wrong. Re-registering a
	// returning user is normal, so the caller gets a distinct sentinel and,
	// when Mercado Pago disclosed it, the existing id to reuse.
	if resp.status == http.StatusBadRequest && mpErr.isDuplicateCustomer() {
		apiErr.sentinel = ErrCustomerAlreadyExists
		apiErr.CustomerID = mpErr.existingCustomerID()
	}
	return apiErr
}

// CreatePreAuth places an authorization hold on the card behind req.CardToken.
//
// The payment is created with capture=false, so the funds are reserved on the
// cardholder's account and not moved. It is validated locally first: a
// request that cannot succeed never reaches Mercado Pago, which keeps a bug
// on our side from consuming a single-use card token.
func (c *Client) CreatePreAuth(ctx context.Context, req PreAuthRequest) (string, error) {
	const op = "CreatePreAuth"

	if err := req.validate(); err != nil {
		return "", err
	}

	body := preAuthBody(req)

	// Derived from the bet id, never random. bets_mp_preauth_id_key allows
	// one pre-auth per bet, and more importantly a second authorization is a
	// second hold against a real person's available balance. A caller that
	// retries after a timeout must reach the *same* payment.
	resp, err := c.post(ctx, op, "/v1/payments", body, idempotencyKey("preauth", req.ExternalReference))
	if err != nil {
		return "", err
	}

	var decoded struct {
		ID           flexibleID `json:"id"`
		Status       string     `json:"status"`
		StatusDetail string     `json:"status_detail"`
	}
	// A body we cannot parse is only fatal on the success path; on an error
	// status the HTTP code alone already tells us what to return.
	unmarshalErr := json.Unmarshal(resp.body, &decoded)

	if resp.status < 200 || resp.status > 299 {
		mpErr := parseErrorBody(resp.body)
		return "", &APIError{
			Op:         op,
			HTTPStatus: resp.status,
			Status:     decoded.Status,
			// Only ever a payment's own status_detail. Mercado Pago's error
			// codes go in Code: folding them in here would put
			// "unauthorized" where a caller expects a cc_rejected_* reason
			// and derives a user-facing message from it.
			StatusDetail: decoded.StatusDetail,
			Message:      mpErr.Message,
			Code:         mpErr.Code,
			PaymentID:    string(decoded.ID),
			sentinel:     sentinelForStatus(resp.status),
		}
	}

	if unmarshalErr != nil {
		// 2xx with an unreadable body: a hold may exist and we have no id for
		// it. Unknown, not declined.
		return "", &APIError{Op: op, HTTPStatus: resp.status, Message: "response body is not valid JSON", sentinel: ErrUnavailable}
	}

	// The decisive check. Mercado Pago answers 201 Created for a *declined*
	// card too — the issuer's refusal is in the body, not the status line —
	// so trusting the HTTP code here would record a nonexistent hold as a
	// successful one and let a bet be placed against money that was never
	// reserved.
	switch decoded.Status {
	case statusAuthorized:
		if decoded.ID == "" {
			return "", &APIError{Op: op, HTTPStatus: resp.status, Status: decoded.Status, Message: "authorized payment carried no id", sentinel: ErrUnavailable}
		}
		return string(decoded.ID), nil

	case statusRejected, statusCancelled:
		return "", &APIError{
			Op:           op,
			HTTPStatus:   resp.status,
			Status:       decoded.Status,
			StatusDetail: decoded.StatusDetail,
			PaymentID:    string(decoded.ID),
			sentinel:     ErrCardDeclined,
		}

	default:
		// binary_mode=true asks Mercado Pago to settle on authorized or
		// rejected and never leave a payment "in_process"/"pending", so
		// anything else is an anomaly rather than a routine outcome. We do
		// not accept it as success — the hold is not confirmed — and we do
		// not call it a decline either, because the issuer has not refused.
		// The payment id is preserved so the transaction can be found and
		// reconciled by hand.
		return "", &APIError{
			Op:           op,
			HTTPStatus:   resp.status,
			Status:       decoded.Status,
			StatusDetail: decoded.StatusDetail,
			PaymentID:    string(decoded.ID),
			Message:      "payment is neither authorized nor rejected",
			sentinel:     ErrUnavailable,
		}
	}
}

// validate rejects a request that cannot succeed, before it costs a network
// call or burns the single-use card token.
func (r PreAuthRequest) validate() error {
	switch {
	case strings.TrimSpace(r.CardToken) == "":
		return fmt.Errorf("%w: CardToken is empty", ErrInvalidRequest)
	case strings.TrimSpace(r.ExternalReference) == "":
		// Without it there is no stable idempotency key, so a retry would
		// place a second hold. Refusing here is what makes that impossible.
		return fmt.Errorf("%w: ExternalReference is empty", ErrInvalidRequest)
	case r.AmountBRL <= 0:
		return fmt.Errorf("%w: AmountBRL must be greater than zero, got %s", ErrInvalidRequest, r.AmountBRL)
	case strings.TrimSpace(r.PayerEmail) == "":
		// Mercado Pago rejects a card payment without a payer email; failing
		// here gives a field name instead of an opaque 400.
		return fmt.Errorf("%w: PayerEmail is empty", ErrInvalidRequest)
	}
	return nil
}

// payment is the POST /v1/payments body for a pre-authorization.
type payment struct {
	TransactionAmount Centavos `json:"transaction_amount"`
	Token             string   `json:"token"`
	Description       string   `json:"description,omitempty"`
	Installments      int      `json:"installments"`
	PaymentMethodID   string   `json:"payment_method_id,omitempty"`
	// Capture=false is what makes this an authorization hold rather than a
	// charge. It is a non-omitempty pointer-free bool on purpose: with
	// `omitempty` the false value would be dropped from the JSON and Mercado
	// Pago would default to capturing the money immediately.
	Capture bool `json:"capture"`
	// BinaryMode=true forbids a "pending" outcome, so the response is either
	// an authorization or a decline and the caller never has to poll.
	BinaryMode        bool          `json:"binary_mode"`
	ExternalReference string        `json:"external_reference"`
	Payer             *paymentPayer `json:"payer,omitempty"`
}

type paymentPayer struct {
	// Type must be "customer" whenever ID is set — Mercado Pago rejects a
	// payer id without one (error 4013, "payer.type can't be null"). Omitted
	// entirely for the guest flow, which identifies the payer by email alone.
	Type string `json:"type,omitempty"`
	// ID links the payment to a stored Mercado Pago customer. Omitted for a
	// one-off card that was never saved.
	ID    string `json:"id,omitempty"`
	Email string `json:"email"`
}

// payerTypeCustomer is the only payer type this package sends.
const payerTypeCustomer = "customer"

func preAuthBody(r PreAuthRequest) payment {
	payer := &paymentPayer{Email: strings.ToLower(strings.TrimSpace(r.PayerEmail))}
	if customer := strings.TrimSpace(r.CustomerID); customer != "" {
		payer.Type = payerTypeCustomer
		payer.ID = customer
	}

	return payment{
		TransactionAmount: r.AmountBRL,
		Token:             strings.TrimSpace(r.CardToken),
		Description:       strings.TrimSpace(r.Description),
		// Always 1. A pre-authorization is a hold on the full amount; there
		// is nothing to instalment, and Mercado Pago requires the field.
		Installments:      1,
		PaymentMethodID:   strings.TrimSpace(r.PaymentMethodID),
		Capture:           false,
		BinaryMode:        true,
		ExternalReference: strings.TrimSpace(r.ExternalReference),
		Payer:             payer,
	}
}

// response is a completed HTTP exchange with the body already drained and
// closed.
type response struct {
	status int
	body   []byte
}

// post sends one JSON POST and reads the response.
//
// There is no retry here, deliberately: see CLAUDE.md. One attempt, the
// caller's context honoured throughout, and every transport failure folded
// into ErrUnavailable so a caller can never mistake "we could not ask" for
// "the answer was no".
func (c *Client) post(ctx context.Context, op, path string, body any, idemKey string) (*response, error) {
	encoded, err := json.Marshal(body)
	if err != nil {
		return nil, &APIError{Op: op, Message: "encode request body", sentinel: ErrInvalidRequest}
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, bytes.NewReader(encoded))
	if err != nil {
		// A bad base URL lands here. It is a misconfiguration, but from a
		// caller's seat it is still "we could not reach Mercado Pago".
		return nil, &APIError{Op: op, Message: "build request", sentinel: ErrUnavailable}
	}
	req.Header.Set("Authorization", "Bearer "+c.accessToken)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Idempotency-Key", idemKey)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		// Timeouts, DNS failures, resets and a cancelled context all arrive
		// here. None of them says anything about the card. The error string
		// is dropped rather than wrapped because it contains the request URL,
		// and keeping the access token out of logs is easier when no error in
		// this package ever carries request detail.
		msg := "request failed"
		if ctxErr := ctx.Err(); ctxErr != nil {
			msg = "request cancelled: " + ctxErr.Error()
		}
		return nil, &APIError{Op: op, Message: msg, sentinel: ErrUnavailable}
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return nil, &APIError{Op: op, HTTPStatus: resp.StatusCode, Message: "read response body", sentinel: ErrUnavailable}
	}

	return &response{status: resp.StatusCode, body: raw}, nil
}

// idempotencyKey derives a stable X-Idempotency-Key from the operation and
// the caller's own identifier for it.
//
// It is a hash rather than the raw value for two reasons: the header must be
// a bounded, header-safe ASCII string, and a bet id is our internal
// identifier — hashing it keeps it out of a third party's logs. The namespace
// prefix stops a customer call and a payment call from colliding on the same
// input.
//
// Nothing random goes in. A random key would defeat the entire purpose: two
// retries of the same pre-auth would look like two distinct requests and
// place two holds on the cardholder's balance.
func idempotencyKey(namespace, value string) string {
	sum := sha256.Sum256([]byte(namespace + ":" + strings.TrimSpace(value)))
	return hex.EncodeToString(sum[:])
}

// mpError is the error envelope Mercado Pago returns on a 4xx or 5xx.
//
// There are two of them in production and this struct is the union. The
// service layer sends
//
//	{"message":"...","error":"bad_request","status":400,
//	 "cause":[{"code":101,"description":"..."}]}
//
// while the edge gateway — which is what answers a missing or malformed
// bearer token — sends only
//
//	{"code":"unauthorized","message":"invalid access token"}
//
// with no status and no cause at all. Every field here is therefore optional,
// nothing indexes into Cause without checking, and a body that is not JSON
// (the edge also serves HTML) degrades to the zero value. The HTTP status
// alone always determines the sentinel, so none of this parsing is load
// bearing — it only enriches the log line.
type mpError struct {
	Message string  `json:"message"`
	Error   string  `json:"error"`
	Status  int     `json:"status"`
	Cause   []cause `json:"cause"`
	// Code is the edge gateway's top-level slug ("unauthorized"). The service
	// layer puts its numeric codes inside Cause instead, so at most one of
	// these is ever populated.
	Code flexibleID `json:"code"`
}

// cause is one entry of Mercado Pago's cause array. The code arrives as a
// number on some endpoints and a string on others, so it is decoded loosely.
type cause struct {
	Code        flexibleID `json:"code"`
	Description string     `json:"description"`
}

// parsed is the flattened view the error mapping actually uses.
type parsedError struct {
	Message string
	Code    string
	Details []string
}

func parseErrorBody(body []byte) parsedError {
	var raw mpError
	if err := json.Unmarshal(body, &raw); err != nil {
		// A body we cannot parse still leaves the HTTP status usable; the
		// caller's sentinel does not depend on this succeeding.
		return parsedError{}
	}

	out := parsedError{Message: strings.TrimSpace(raw.Message)}
	for _, c := range raw.Cause {
		if out.Code == "" && c.Code != "" {
			out.Code = string(c.Code)
		}
		if d := strings.TrimSpace(c.Description); d != "" {
			out.Details = append(out.Details, d)
		}
	}
	// Fall back to the edge gateway's top-level slug, then to the service
	// layer's "error" field, so the log line names something either way.
	if out.Code == "" {
		out.Code = string(raw.Code)
	}
	if out.Message == "" {
		out.Message = strings.TrimSpace(raw.Error)
	}
	return out
}

// duplicateCustomerCode is the cause code Mercado Pago attaches to "a
// customer already exists with that email".
const duplicateCustomerCode = "101"

// isDuplicateCustomer reports whether a 400 is the benign "this email is
// already registered" case.
//
// Both the code and the message are checked: the code is the reliable signal,
// but it has not always been present on this response, and mistaking a
// duplicate for a hard failure would break every returning user adding a
// second card.
func (p parsedError) isDuplicateCustomer() bool {
	if p.Code == duplicateCustomerCode {
		return true
	}
	for _, text := range append([]string{p.Message}, p.Details...) {
		lower := strings.ToLower(text)
		if strings.Contains(lower, "already exist") || strings.Contains(lower, "already registered") {
			return true
		}
	}
	return false
}

// existingCustomerID digs the pre-existing customer id out of the duplicate
// error, on the chance Mercado Pago mentions it.
//
// It usually will not: there is no documented field carrying the id on a 400,
// and the documented recovery is a follow-up
// GET /v1/customers/search?email=... — which this package does not implement.
// So "" is the *expected* outcome, not a failure. Callers must have a
// fallback, normally their own payment_methods.mp_customer_id row. This is
// best-effort enrichment for the case where the id does turn up in the prose.
func (p parsedError) existingCustomerID() string {
	for _, text := range p.Details {
		if id := customerIDIn(text); id != "" {
			return id
		}
	}
	return customerIDIn(p.Message)
}

// customerIDIn finds a Mercado Pago customer id — a run of digits, a hyphen,
// then an alphanumeric suffix, e.g. "1234567890-AbCdEfGh" — inside prose.
func customerIDIn(text string) string {
	for _, field := range strings.FieldsFunc(text, func(r rune) bool {
		return r == ' ' || r == ',' || r == '.' || r == ':' || r == ';' || r == '"' || r == '\''
	}) {
		digits, suffix, ok := strings.Cut(field, "-")
		if !ok || len(digits) < 6 || suffix == "" {
			continue
		}
		if isDigits(digits) && isAlphanumeric(suffix) {
			return field
		}
	}
	return ""
}

func isDigits(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func isAlphanumeric(s string) bool {
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9', r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z':
		default:
			return false
		}
	}
	return true
}

// flexibleID decodes an identifier Mercado Pago may send as a JSON number or
// a JSON string. Payment ids arrive as numbers, customer ids as strings, and
// a plain string field silently fails on the first form — which would turn
// every successful pre-authorization into "carried no id".
type flexibleID string

func (f *flexibleID) UnmarshalJSON(data []byte) error {
	text := strings.TrimSpace(string(data))
	if text == "null" || text == "" {
		*f = ""
		return nil
	}
	if strings.HasPrefix(text, `"`) {
		var s string
		if err := json.Unmarshal(data, &s); err != nil {
			return err
		}
		*f = flexibleID(strings.TrimSpace(s))
		return nil
	}
	var n json.Number
	if err := json.Unmarshal(data, &n); err != nil {
		return err
	}
	*f = flexibleID(n.String())
	return nil
}
