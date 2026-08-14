package mercadopago_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/tapago/tapago-api/internal/mercadopago"
)

const testToken = "TEST-access-token"

// captured is what a test server saw, so a case can assert on the request we
// built rather than only on the reply we handled.
type captured struct {
	mu      sync.Mutex
	path    string
	idemKey string
	auth    string
	body    map[string]any
	raw     string
	calls   int
}

func (c *captured) snapshot() captured {
	c.mu.Lock()
	defer c.mu.Unlock()
	return captured{path: c.path, idemKey: c.idemKey, auth: c.auth, body: c.body, raw: c.raw, calls: c.calls}
}

// newServer starts an httptest server that records the request and replies
// with the given status and body. No test in this package touches the
// network: the README promises the suite has no external dependencies, and a
// payments client is the last thing that should be calling out to a live API
// from a unit test.
func newServer(t *testing.T, status int, reply string) (*mercadopago.Client, *captured) {
	t.Helper()

	rec := &captured{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)

		rec.mu.Lock()
		rec.calls++
		rec.path = r.URL.Path
		rec.idemKey = r.Header.Get("X-Idempotency-Key")
		rec.auth = r.Header.Get("Authorization")
		rec.raw = string(raw)
		rec.body = nil
		_ = json.Unmarshal(raw, &rec.body)
		rec.mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, reply)
	}))
	t.Cleanup(srv.Close)

	client, err := mercadopago.New(testToken, mercadopago.WithBaseURL(srv.URL))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return client, rec
}

func validPreAuth() mercadopago.PreAuthRequest {
	return mercadopago.PreAuthRequest{
		CardToken:         "card-token-abc",
		CustomerID:        "1234567890-CuStOmEr",
		AmountBRL:         mercadopago.BRL(19, 99),
		Description:       "Aposta tapago",
		ExternalReference: "bet-0001",
		PayerEmail:        "Person@Example.com",
		PaymentMethodID:   "visa",
	}
}

func TestNew(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		token   string
		wantErr error
	}{
		{name: "token accepted", token: "APP_USR-123"},
		{name: "blank token refused", token: "", wantErr: mercadopago.ErrNoAccessToken},
		{name: "whitespace token refused", token: "   \t ", wantErr: mercadopago.ErrNoAccessToken},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			client, err := mercadopago.New(tc.token)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("New(%q) error = %v, want %v", tc.token, err, tc.wantErr)
				}
				if client != nil {
					t.Fatal("New returned a client alongside an error")
				}
				return
			}
			if err != nil {
				t.Fatalf("New(%q): %v", tc.token, err)
			}
			if client == nil {
				t.Fatal("New returned no client and no error")
			}
		})
	}
}

func TestCreateCustomer(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		status     int
		reply      string
		email      string
		wantID     string
		wantErr    error
		wantExists string // expected APIError.CustomerID
	}{
		{
			name:   "created",
			status: http.StatusCreated,
			reply:  `{"id":"1234567890-AbCdEfGh","email":"person@example.com"}`,
			email:  "  Person@Example.COM ",
			wantID: "1234567890-AbCdEfGh",
		},
		{
			name:   "already exists with id disclosed",
			status: http.StatusBadRequest,
			reply: `{"message":"Customer already exists.","error":"bad_request","status":400,
			         "cause":[{"code":101,"description":"Customer already existent: 1234567890-AbCdEfGh"}]}`,
			email:      "person@example.com",
			wantErr:    mercadopago.ErrCustomerAlreadyExists,
			wantExists: "1234567890-AbCdEfGh",
		},
		{
			name:   "already exists without id",
			status: http.StatusBadRequest,
			reply:  `{"message":"Customer already exists.","error":"bad_request","status":400}`,
			email:  "person@example.com",
			// Recognised from the message even though the cause array is
			// absent, because a returning user must not hit a hard failure.
			wantErr: mercadopago.ErrCustomerAlreadyExists,
		},
		{
			name:    "other 400 is our bug",
			status:  http.StatusBadRequest,
			reply:   `{"message":"email must be a valid email","error":"bad_request","status":400,"cause":[{"code":2067,"description":"invalid email"}]}`,
			email:   "person@example.com",
			wantErr: mercadopago.ErrInvalidRequest,
		},
		{
			name:    "unauthorized",
			status:  http.StatusUnauthorized,
			reply:   `{"message":"invalid access token","error":"unauthorized","status":401}`,
			email:   "person@example.com",
			wantErr: mercadopago.ErrAuthentication,
		},
		{
			name:    "2xx with no id",
			status:  http.StatusCreated,
			reply:   `{"email":"person@example.com"}`,
			email:   "person@example.com",
			wantErr: mercadopago.ErrUnavailable,
		},
		{
			name:    "malformed json",
			status:  http.StatusCreated,
			reply:   `{"id":`,
			email:   "person@example.com",
			wantErr: mercadopago.ErrUnavailable,
		},
		{
			name:    "blank email never reaches the network",
			status:  http.StatusCreated,
			reply:   `{"id":"never-used"}`,
			email:   "   ",
			wantErr: mercadopago.ErrInvalidRequest,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			client, rec := newServer(t, tc.status, tc.reply)
			id, err := client.CreateCustomer(context.Background(), tc.email)

			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("CreateCustomer error = %v, want %v", err, tc.wantErr)
				}
				if id != "" {
					t.Errorf("CreateCustomer returned id %q alongside an error", id)
				}
				if tc.wantExists != "" {
					var apiErr *mercadopago.APIError
					if !errors.As(err, &apiErr) {
						t.Fatalf("error %v is not an *APIError", err)
					}
					if apiErr.CustomerID != tc.wantExists {
						t.Errorf("APIError.CustomerID = %q, want %q", apiErr.CustomerID, tc.wantExists)
					}
				}
				return
			}

			if err != nil {
				t.Fatalf("CreateCustomer: %v", err)
			}
			if id != tc.wantID {
				t.Errorf("customer id = %q, want %q", id, tc.wantID)
			}

			got := rec.snapshot()
			if got.path != "/v1/customers" {
				t.Errorf("path = %q, want /v1/customers", got.path)
			}
			// Normalised the same way auth normalises addresses, so one user
			// cannot end up with two customers differing only in case.
			if email, _ := got.body["email"].(string); email != "person@example.com" {
				t.Errorf("email sent = %q, want person@example.com", email)
			}
			if got.auth != "Bearer "+testToken {
				t.Errorf("Authorization = %q", got.auth)
			}
			if got.idemKey == "" {
				t.Error("X-Idempotency-Key was not sent")
			}
		})
	}
}

// A blank email must be caught before the client dials, not by Mercado Pago.
func TestCreateCustomerValidatesBeforeCalling(t *testing.T) {
	t.Parallel()

	client, rec := newServer(t, http.StatusCreated, `{"id":"never-used"}`)
	if _, err := client.CreateCustomer(context.Background(), " "); !errors.Is(err, mercadopago.ErrInvalidRequest) {
		t.Fatalf("error = %v, want ErrInvalidRequest", err)
	}
	if calls := rec.snapshot().calls; calls != 0 {
		t.Errorf("server was called %d times, want 0", calls)
	}
}

// The same email always yields the same key, and a different one does not —
// otherwise a retry would create a second customer.
func TestCreateCustomerIdempotencyKeyIsDeterministic(t *testing.T) {
	t.Parallel()

	client, rec := newServer(t, http.StatusCreated, `{"id":"1234567890-AbCdEfGh"}`)
	ctx := context.Background()

	if _, err := client.CreateCustomer(ctx, "person@example.com"); err != nil {
		t.Fatalf("first call: %v", err)
	}
	first := rec.snapshot().idemKey

	// Case and whitespace differences normalise to the same address, so they
	// must not produce a different key.
	if _, err := client.CreateCustomer(ctx, "  PERSON@Example.com "); err != nil {
		t.Fatalf("second call: %v", err)
	}
	if second := rec.snapshot().idemKey; second != first {
		t.Errorf("idempotency key changed between equivalent calls: %q vs %q", first, second)
	}

	if _, err := client.CreateCustomer(ctx, "other@example.com"); err != nil {
		t.Fatalf("third call: %v", err)
	}
	if third := rec.snapshot().idemKey; third == first {
		t.Error("two different emails shared an idempotency key")
	}
}

func TestCreatePreAuthSuccess(t *testing.T) {
	t.Parallel()

	client, rec := newServer(t, http.StatusCreated,
		`{"id":123456789,"status":"authorized","status_detail":"pending_capture","captured":false}`)

	id, err := client.CreatePreAuth(context.Background(), validPreAuth())
	if err != nil {
		t.Fatalf("CreatePreAuth: %v", err)
	}
	// Mercado Pago sends the payment id as a JSON number; it must survive as
	// a string without being mangled by a float round-trip.
	if id != "123456789" {
		t.Errorf("payment id = %q, want 123456789", id)
	}

	got := rec.snapshot()
	if got.path != "/v1/payments" {
		t.Errorf("path = %q, want /v1/payments", got.path)
	}

	// capture=false is the whole point: with it dropped or true, Mercado Pago
	// takes the money instead of holding it.
	if capture, ok := got.body["capture"].(bool); !ok || capture {
		t.Errorf("capture = %v (present=%v), want false", got.body["capture"], ok)
	}
	if _, present := got.body["capture"]; !present {
		t.Error("capture was omitted from the request body")
	}
	if binary, _ := got.body["binary_mode"].(bool); !binary {
		t.Error("binary_mode was not true")
	}
	if inst, _ := got.body["installments"].(float64); inst != 1 {
		t.Errorf("installments = %v, want 1", got.body["installments"])
	}
	if ref, _ := got.body["external_reference"].(string); ref != "bet-0001" {
		t.Errorf("external_reference = %q, want bet-0001", ref)
	}
	if token, _ := got.body["token"].(string); token != "card-token-abc" {
		t.Errorf("token = %q, want card-token-abc", token)
	}
	if method, _ := got.body["payment_method_id"].(string); method != "visa" {
		t.Errorf("payment_method_id = %q, want visa", method)
	}

	payer, ok := got.body["payer"].(map[string]any)
	if !ok {
		t.Fatalf("payer missing from body: %s", got.raw)
	}
	if email, _ := payer["email"].(string); email != "person@example.com" {
		t.Errorf("payer.email = %q, want person@example.com (lower-cased)", email)
	}
	if customer, _ := payer["id"].(string); customer != "1234567890-CuStOmEr" {
		t.Errorf("payer.id = %q", customer)
	}
	// Mercado Pago rejects a payer id without a type (error 4013), so the two
	// must always travel together.
	if kind, _ := payer["type"].(string); kind != "customer" {
		t.Errorf("payer.type = %q, want customer", kind)
	}

	// The exact decimal, on the wire, with no float64 anywhere in between.
	if !strings.Contains(got.raw, `"transaction_amount":19.99`) {
		t.Errorf("transaction_amount not serialised as 19.99: %s", got.raw)
	}

	if got.idemKey == "" {
		t.Error("X-Idempotency-Key was not sent")
	}
	if got.auth != "Bearer "+testToken {
		t.Errorf("Authorization = %q", got.auth)
	}
}

// The guest flow: no saved customer, so the payer is identified by email
// alone. payer.type must then be absent — sending "customer" without an id
// is not a shape Mercado Pago accepts.
func TestCreatePreAuthWithoutCustomerOmitsPayerType(t *testing.T) {
	t.Parallel()

	client, rec := newServer(t, http.StatusCreated, `{"id":1,"status":"authorized"}`)

	req := validPreAuth()
	req.CustomerID = ""
	if _, err := client.CreatePreAuth(context.Background(), req); err != nil {
		t.Fatalf("CreatePreAuth: %v", err)
	}

	payer, ok := rec.snapshot().body["payer"].(map[string]any)
	if !ok {
		t.Fatal("payer missing from body")
	}
	if _, present := payer["type"]; present {
		t.Errorf("payer.type = %v, want it omitted with no customer id", payer["type"])
	}
	if _, present := payer["id"]; present {
		t.Errorf("payer.id = %v, want it omitted with no customer id", payer["id"])
	}
	if email, _ := payer["email"].(string); email != "person@example.com" {
		t.Errorf("payer.email = %q", email)
	}
}

// Mercado Pago runs two error envelopes: the service layer's
// {message,error,status,cause[]} and the edge gateway's bare {code,message},
// which is what a bad bearer token actually hits. Both must map to the same
// sentinel, and neither may panic on the other's missing fields.
func TestErrorEnvelopeVariants(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		status  int
		reply   string
		wantErr error
		wantMsg string
	}{
		{
			name:    "edge gateway envelope, no status or cause",
			status:  http.StatusUnauthorized,
			reply:   `{"code":"unauthorized","message":"invalid access token"}`,
			wantErr: mercadopago.ErrAuthentication,
			wantMsg: "invalid access token",
		},
		{
			name:    "service envelope with a numeric cause code",
			status:  http.StatusUnauthorized,
			reply:   `{"message":"Must provide your access_token to proceed","error":"unauthorized","status":401,"cause":[{"code":5,"description":"Must provide your access_token to proceed","data":"14-08-2026T13:42:56UTC;abc"}]}`,
			wantErr: mercadopago.ErrAuthentication,
			wantMsg: "Must provide your access_token to proceed",
		},
		{
			name:    "service envelope with a string cause code",
			status:  http.StatusBadRequest,
			reply:   `{"message":"bad request","error":"bad_request","status":400,"cause":[{"code":"2062","description":"invalid token"}]}`,
			wantErr: mercadopago.ErrInvalidRequest,
			wantMsg: "bad request",
		},
		{
			name:   "empty cause array is not indexed into",
			status: http.StatusBadRequest,
			reply:  `{"message":"bad request","error":"bad_request","status":400,"cause":[]}`,
			// The point of this case is that it returns at all rather than
			// panicking on cause[0].
			wantErr: mercadopago.ErrInvalidRequest,
			wantMsg: "bad request",
		},
		{
			name:    "non-json body from the edge",
			status:  http.StatusBadGateway,
			reply:   `<html><head><title>502</title></head></html>`,
			wantErr: mercadopago.ErrUnavailable,
		},
		{
			name:    "json array instead of an object",
			status:  http.StatusInternalServerError,
			reply:   `["nope"]`,
			wantErr: mercadopago.ErrUnavailable,
		},
		{
			name:    "message falls back to the error slug",
			status:  http.StatusBadRequest,
			reply:   `{"error":"bad_request","status":400}`,
			wantErr: mercadopago.ErrInvalidRequest,
			wantMsg: "bad_request",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			client, _ := newServer(t, tc.status, tc.reply)
			_, err := client.CreatePreAuth(context.Background(), validPreAuth())

			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("error = %v, want %v", err, tc.wantErr)
			}
			if tc.wantMsg != "" {
				var apiErr *mercadopago.APIError
				if !errors.As(err, &apiErr) {
					t.Fatalf("error %v is not an *APIError", err)
				}
				if apiErr.Message != tc.wantMsg {
					t.Errorf("APIError.Message = %q, want %q", apiErr.Message, tc.wantMsg)
				}
			}
		})
	}
}

// Retryable draws the line callers use to decide whether to try again: an
// outage or a rate limit can differ next time, a decline or a bad request
// cannot.
func TestAPIErrorRetryable(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		status int
		reply  string
		want   bool
	}{
		{name: "declined", status: http.StatusCreated, reply: `{"id":1,"status":"rejected","status_detail":"cc_rejected_card_error"}`, want: false},
		{name: "bad request", status: http.StatusBadRequest, reply: `{"message":"bad"}`, want: false},
		{name: "unauthorized", status: http.StatusUnauthorized, reply: `{"message":"bad token"}`, want: false},
		{name: "rate limited", status: http.StatusTooManyRequests, reply: `{"message":"slow down"}`, want: true},
		{name: "server error", status: http.StatusInternalServerError, reply: `{"message":"boom"}`, want: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			client, _ := newServer(t, tc.status, tc.reply)
			_, err := client.CreatePreAuth(context.Background(), validPreAuth())

			var apiErr *mercadopago.APIError
			if !errors.As(err, &apiErr) {
				t.Fatalf("error %v is not an *APIError", err)
			}
			if got := apiErr.Retryable(); got != tc.want {
				t.Errorf("Retryable() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestCreatePreAuthOutcomes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		status     int
		reply      string
		wantErr    error
		wantDetail string
		wantPayID  string
	}{
		{
			name:   "declined card is a 201, not an HTTP error",
			status: http.StatusCreated,
			reply:  `{"id":987654321,"status":"rejected","status_detail":"cc_rejected_insufficient_amount"}`,
			// The single most important case in this package: an integration
			// that trusted the HTTP code would record this as a live hold.
			wantErr:    mercadopago.ErrCardDeclined,
			wantDetail: "cc_rejected_insufficient_amount",
			wantPayID:  "987654321",
		},
		{
			name:       "bad security code",
			status:     http.StatusCreated,
			reply:      `{"id":11,"status":"rejected","status_detail":"cc_rejected_bad_filled_security_code"}`,
			wantErr:    mercadopago.ErrCardDeclined,
			wantDetail: "cc_rejected_bad_filled_security_code",
			wantPayID:  "11",
		},
		{
			name:      "cancelled counts as declined",
			status:    http.StatusCreated,
			reply:     `{"id":12,"status":"cancelled","status_detail":"by_collector"}`,
			wantErr:   mercadopago.ErrCardDeclined,
			wantPayID: "12",
		},
		{
			name:   "in_process is not accepted as success",
			status: http.StatusCreated,
			reply:  `{"id":13,"status":"in_process","status_detail":"pending_review_manual"}`,
			// binary_mode should prevent this. If it happens anyway the hold
			// is unconfirmed — but the issuer has not refused, so calling it a
			// decline would be a lie to the user.
			wantErr:   mercadopago.ErrUnavailable,
			wantPayID: "13",
		},
		{
			name:      "approved means it was captured, which we did not ask for",
			status:    http.StatusCreated,
			reply:     `{"id":14,"status":"approved","status_detail":"accredited"}`,
			wantErr:   mercadopago.ErrUnavailable,
			wantPayID: "14",
		},
		{
			name:    "authorized without an id",
			status:  http.StatusCreated,
			reply:   `{"status":"authorized"}`,
			wantErr: mercadopago.ErrUnavailable,
		},
		{
			name:    "bad request",
			status:  http.StatusBadRequest,
			reply:   `{"message":"invalid card token","error":"bad_request","status":400,"cause":[{"code":2062,"description":"invalid token"}]}`,
			wantErr: mercadopago.ErrInvalidRequest,
		},
		{
			name:    "unauthorized",
			status:  http.StatusUnauthorized,
			reply:   `{"message":"invalid access token","error":"unauthorized","status":401}`,
			wantErr: mercadopago.ErrAuthentication,
		},
		{
			name:    "forbidden",
			status:  http.StatusForbidden,
			reply:   `{"message":"forbidden","error":"forbidden","status":403}`,
			wantErr: mercadopago.ErrAuthentication,
		},
		{
			name:    "rate limited",
			status:  http.StatusTooManyRequests,
			reply:   `{"message":"too many requests","error":"too_many_requests","status":429}`,
			wantErr: mercadopago.ErrRateLimited,
		},
		{
			name:    "server error",
			status:  http.StatusInternalServerError,
			reply:   `{"message":"internal error","error":"internal_error","status":500}`,
			wantErr: mercadopago.ErrUnavailable,
		},
		{
			name:    "bad gateway with an empty body",
			status:  http.StatusBadGateway,
			reply:   ``,
			wantErr: mercadopago.ErrUnavailable,
		},
		{
			name:    "malformed json on a 2xx",
			status:  http.StatusCreated,
			reply:   `{"id":123,"status":`,
			wantErr: mercadopago.ErrUnavailable,
		},
		{
			name:    "html error page instead of json",
			status:  http.StatusServiceUnavailable,
			reply:   `<html><body>503 Service Unavailable</body></html>`,
			wantErr: mercadopago.ErrUnavailable,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			client, _ := newServer(t, tc.status, tc.reply)
			id, err := client.CreatePreAuth(context.Background(), validPreAuth())

			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("CreatePreAuth error = %v, want %v", err, tc.wantErr)
			}
			if id != "" {
				t.Errorf("returned payment id %q alongside an error", id)
			}

			// A failure must never be mistaken for the other kind: a decline
			// is not an outage and an outage is not a decline.
			if tc.wantErr != mercadopago.ErrCardDeclined && errors.Is(err, mercadopago.ErrCardDeclined) {
				t.Error("non-decline reported as ErrCardDeclined")
			}
			if tc.wantErr == mercadopago.ErrCardDeclined && errors.Is(err, mercadopago.ErrUnavailable) {
				t.Error("decline reported as ErrUnavailable")
			}

			var apiErr *mercadopago.APIError
			if !errors.As(err, &apiErr) {
				t.Fatalf("error %v is not an *APIError", err)
			}
			if tc.wantDetail != "" && apiErr.StatusDetail != tc.wantDetail {
				t.Errorf("APIError.StatusDetail = %q, want %q", apiErr.StatusDetail, tc.wantDetail)
			}
			if tc.wantPayID != "" && apiErr.PaymentID != tc.wantPayID {
				t.Errorf("APIError.PaymentID = %q, want %q", apiErr.PaymentID, tc.wantPayID)
			}
			// Whatever else it says, it must never leak the credential.
			if strings.Contains(apiErr.Error(), testToken) {
				t.Errorf("error message leaks the access token: %s", apiErr.Error())
			}
		})
	}
}

func TestCreatePreAuthValidation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		mutate   func(*mercadopago.PreAuthRequest)
		wantWord string
	}{
		{name: "empty card token", mutate: func(r *mercadopago.PreAuthRequest) { r.CardToken = "" }, wantWord: "CardToken"},
		{name: "blank card token", mutate: func(r *mercadopago.PreAuthRequest) { r.CardToken = "  " }, wantWord: "CardToken"},
		{name: "empty external reference", mutate: func(r *mercadopago.PreAuthRequest) { r.ExternalReference = "" }, wantWord: "ExternalReference"},
		{name: "zero amount", mutate: func(r *mercadopago.PreAuthRequest) { r.AmountBRL = 0 }, wantWord: "AmountBRL"},
		{name: "negative amount", mutate: func(r *mercadopago.PreAuthRequest) { r.AmountBRL = -100 }, wantWord: "AmountBRL"},
		{name: "empty payer email", mutate: func(r *mercadopago.PreAuthRequest) { r.PayerEmail = "" }, wantWord: "PayerEmail"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			client, rec := newServer(t, http.StatusCreated, `{"id":1,"status":"authorized"}`)
			req := validPreAuth()
			tc.mutate(&req)

			_, err := client.CreatePreAuth(context.Background(), req)
			if !errors.Is(err, mercadopago.ErrInvalidRequest) {
				t.Fatalf("error = %v, want ErrInvalidRequest", err)
			}
			// The message must name the field, or a caller debugging a 400
			// has nothing to go on.
			if !strings.Contains(err.Error(), tc.wantWord) {
				t.Errorf("error %q does not mention %q", err, tc.wantWord)
			}
			// And none of it may reach Mercado Pago: a request we know is bad
			// must not consume the single-use card token.
			if calls := rec.snapshot().calls; calls != 0 {
				t.Errorf("server was called %d times, want 0", calls)
			}
		})
	}
}

// A cancelled context must surface as "we could not reach them", never as a
// decline — the hold may or may not exist.
func TestCreatePreAuthContextCancelled(t *testing.T) {
	t.Parallel()

	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(func() {
		close(release)
		srv.Close()
	})

	client, err := mercadopago.New(testToken, mercadopago.WithBaseURL(srv.URL))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	id, err := client.CreatePreAuth(ctx, validPreAuth())
	if !errors.Is(err, mercadopago.ErrUnavailable) {
		t.Fatalf("error = %v, want ErrUnavailable", err)
	}
	if errors.Is(err, mercadopago.ErrCardDeclined) {
		t.Error("a cancelled context was reported as a declined card")
	}
	if id != "" {
		t.Errorf("returned payment id %q on a cancelled context", id)
	}
}

// The idempotency key is a pure function of the bet id: two attempts at the
// same pre-auth must reach the same key, or the second one places a second
// hold on the cardholder's balance.
func TestCreatePreAuthIdempotencyKeyDerivesFromExternalReference(t *testing.T) {
	t.Parallel()

	client, rec := newServer(t, http.StatusCreated, `{"id":1,"status":"authorized"}`)
	ctx := context.Background()

	if _, err := client.CreatePreAuth(ctx, validPreAuth()); err != nil {
		t.Fatalf("first call: %v", err)
	}
	first := rec.snapshot().idemKey

	// Same bet, different card token and amount — a retry that changed
	// nothing that identifies the bet must still be recognised as a retry.
	retry := validPreAuth()
	retry.CardToken = "card-token-xyz"
	retry.AmountBRL = mercadopago.BRL(50, 00)
	if _, err := client.CreatePreAuth(ctx, retry); err != nil {
		t.Fatalf("retry: %v", err)
	}
	if second := rec.snapshot().idemKey; second != first {
		t.Errorf("idempotency key changed for the same bet: %q vs %q", first, second)
	}

	other := validPreAuth()
	other.ExternalReference = "bet-0002"
	if _, err := client.CreatePreAuth(ctx, other); err != nil {
		t.Fatalf("other bet: %v", err)
	}
	if third := rec.snapshot().idemKey; third == first {
		t.Error("two different bets shared an idempotency key")
	}

	// The bet id itself is not handed to a third party's logs verbatim.
	if strings.Contains(first, "bet-0001") {
		t.Error("idempotency key contains the raw external reference")
	}
}

// A customer create and a pre-auth built from the same string must not
// collide on one key.
func TestIdempotencyKeysAreNamespaced(t *testing.T) {
	t.Parallel()

	client, rec := newServer(t, http.StatusCreated, `{"id":"1234567890-AbCd","status":"authorized"}`)
	ctx := context.Background()

	const shared = "shared@example.com"

	if _, err := client.CreateCustomer(ctx, shared); err != nil {
		t.Fatalf("CreateCustomer: %v", err)
	}
	customerKey := rec.snapshot().idemKey

	req := validPreAuth()
	req.ExternalReference = shared
	if _, err := client.CreatePreAuth(ctx, req); err != nil {
		t.Fatalf("CreatePreAuth: %v", err)
	}
	if paymentKey := rec.snapshot().idemKey; paymentKey == customerKey {
		t.Error("customer and payment calls collided on one idempotency key")
	}
}

// The client is documented as safe for concurrent use; the race detector
// makes that claim testable.
func TestClientConcurrentUse(t *testing.T) {
	t.Parallel()

	client, _ := newServer(t, http.StatusCreated, `{"id":123,"status":"authorized"}`)

	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := client.CreatePreAuth(context.Background(), validPreAuth()); err != nil {
				t.Errorf("CreatePreAuth: %v", err)
			}
		}()
	}
	wg.Wait()
}

// Options that receive a zero value must leave the default in place rather
// than installing it. A blank base URL or a nil HTTP client silently applied
// would produce a client that fails every call at request time.
//
// Nothing here dials: a zero PreAuthRequest is refused by local validation,
// which is exactly the point — construction succeeded and the client is
// usable.
func TestOptionsIgnoreZeroValues(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		opt  mercadopago.Option
	}{
		{name: "blank base url", opt: mercadopago.WithBaseURL("   ")},
		{name: "empty base url", opt: mercadopago.WithBaseURL("")},
		{name: "nil http client", opt: mercadopago.WithHTTPClient(nil)},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			client, err := mercadopago.New(testToken, tc.opt)
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			if _, err := client.CreatePreAuth(context.Background(), mercadopago.PreAuthRequest{}); !errors.Is(err, mercadopago.ErrInvalidRequest) {
				t.Fatalf("error = %v, want ErrInvalidRequest", err)
			}
		})
	}
}

// A trailing slash on the base URL must not produce "//v1/payments".
func TestWithBaseURLTrimsTrailingSlash(t *testing.T) {
	t.Parallel()

	rec := &captured{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.mu.Lock()
		rec.path = r.URL.Path
		rec.mu.Unlock()
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"id":1,"status":"authorized"}`)
	}))
	t.Cleanup(srv.Close)

	client, err := mercadopago.New(testToken, mercadopago.WithBaseURL(srv.URL+"/"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := client.CreatePreAuth(context.Background(), validPreAuth()); err != nil {
		t.Fatalf("CreatePreAuth: %v", err)
	}
	if got := rec.snapshot().path; got != "/v1/payments" {
		t.Errorf("path = %q, want /v1/payments", got)
	}
}

// The response reader is capped; a server that streams forever must not take
// the process with it.
func TestResponseBodyIsCapped(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"id":1,"status":"authorized","padding":"`)
		chunk := strings.Repeat("x", 64*1024)
		for i := 0; i < 64; i++ {
			if _, err := io.WriteString(w, chunk); err != nil {
				return
			}
		}
		_, _ = io.WriteString(w, `"}`)
	}))
	t.Cleanup(srv.Close)

	client, err := mercadopago.New(testToken, mercadopago.WithBaseURL(srv.URL))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// Truncated at the cap, so the JSON no longer parses: an unreadable body
	// is ErrUnavailable, and crucially the call returns instead of reading
	// 4 MiB into memory.
	if _, err := client.CreatePreAuth(context.Background(), validPreAuth()); !errors.Is(err, mercadopago.ErrUnavailable) {
		t.Fatalf("error = %v, want ErrUnavailable", err)
	}
}
