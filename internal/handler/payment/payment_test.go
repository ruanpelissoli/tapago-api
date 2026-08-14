package payment_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	paymenthandler "github.com/tapago/tapago-api/internal/handler/payment"
	"github.com/tapago/tapago-api/internal/mercadopago"
	"github.com/tapago/tapago-api/internal/middleware"
	"github.com/tapago/tapago-api/internal/token"
)

// These tests exercise the payment-method handlers entirely in process: a
// scripted stub stands in for Postgres and a fake client for Mercado Pago.
// What matters here is the mapping from what the database and the provider
// say to what the client sees — and, above all, that a card token or a
// customer id never appears in a response.

const (
	testUserID     = "11111111-1111-4111-8111-111111111111"
	testCardToken  = "card-token-super-secret"
	testCustomerID = "1234567890-AbCdEfGh"
)

// --- fake database -------------------------------------------------------

// stubRow answers one QueryRow. values are handed to Scan positionally.
type stubRow struct {
	values []any
	err    error
}

func (r stubRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	if len(dest) != len(r.values) {
		return fmt.Errorf("scan: %d destinations, want %d", len(dest), len(r.values))
	}
	for i := range dest {
		switch d := dest[i].(type) {
		case *string:
			*d = r.values[i].(string)
		case *bool:
			*d = r.values[i].(bool)
		case *time.Time:
			*d = r.values[i].(time.Time)
		default:
			return fmt.Errorf("scan: unsupported destination type %T", d)
		}
	}
	return nil
}

// stubRows replays a fixed result set for Query.
type stubRows struct {
	rows   []stubRow
	err    error
	cursor int
	closed bool
}

func (r *stubRows) Close()                                       { r.closed = true }
func (r *stubRows) Err() error                                   { return r.err }
func (r *stubRows) CommandTag() pgconn.CommandTag                { return pgconn.CommandTag{} }
func (r *stubRows) FieldDescriptions() []pgconn.FieldDescription { return nil }
func (r *stubRows) Values() ([]any, error)                       { return nil, errors.New("not implemented") }
func (r *stubRows) RawValues() [][]byte                          { return nil }
func (r *stubRows) Conn() *pgx.Conn                              { return nil }

func (r *stubRows) Next() bool {
	if r.cursor >= len(r.rows) {
		return false
	}
	r.cursor++
	return true
}

func (r *stubRows) Scan(dest ...any) error { return r.rows[r.cursor-1].Scan(dest...) }

// call records one statement the handler sent, so a test can assert on the
// SQL and on the arguments that actually reached the database.
type call struct {
	sql  string
	args []any
}

// stubDB replays queued answers in order. An unexpected extra query is an
// error rather than a silent nil, so a handler taking a path the test did
// not script fails loudly.
type stubDB struct {
	rowAnswers []stubRow
	rowsAnswer *stubRows
	rowsErr    error
	calls      []call
}

func (s *stubDB) QueryRow(_ context.Context, sql string, args ...any) pgx.Row {
	s.calls = append(s.calls, call{sql: sql, args: args})
	if len(s.calls) > len(s.rowAnswers) {
		return stubRow{err: fmt.Errorf("unexpected query %d: %s", len(s.calls), sql)}
	}
	return s.rowAnswers[len(s.calls)-1]
}

func (s *stubDB) Query(_ context.Context, sql string, args ...any) (pgx.Rows, error) {
	s.calls = append(s.calls, call{sql: sql, args: args})
	if s.rowsErr != nil {
		return nil, s.rowsErr
	}
	if s.rowsAnswer == nil {
		return &stubRows{}, nil
	}
	return s.rowsAnswer, nil
}

// --- fake Mercado Pago client -------------------------------------------

type stubMP struct {
	customerID string
	err        error
	calls      int
	lastEmail  string
}

func (s *stubMP) CreateCustomer(_ context.Context, email string) (string, error) {
	s.calls++
	s.lastEmail = email
	if s.err != nil {
		return "", s.err
	}
	return s.customerID, nil
}

func (s *stubMP) CreatePreAuth(context.Context, mercadopago.PreAuthRequest) (string, error) {
	return "", errors.New("not used by these handlers")
}

// --- helpers -------------------------------------------------------------

// savedRow is what the insert's RETURNING clause produces.
func savedRow(isDefault bool) stubRow {
	return stubRow{values: []any{
		"22222222-2222-4222-8222-222222222222", "4242", "visa", isDefault,
		time.Date(2026, 8, 14, 12, 0, 0, 0, time.UTC),
	}}
}

func pgErr(code string) stubRow {
	return stubRow{err: &pgconn.PgError{Code: code}}
}

// authed builds a request carrying an authenticated user id, the way
// RequireAuth would. It goes through the middleware rather than writing the
// context key directly because that key is deliberately unexported.
func authed(t *testing.T, method, body, userID string) *http.Request {
	t.Helper()

	var reader *strings.Reader
	if body == "" {
		reader = strings.NewReader("")
	} else {
		reader = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, "/v1/payment-methods", reader)
	if userID == "" {
		return req
	}

	issuer, err := token.New("payment-handler-test-secret")
	if err != nil {
		t.Fatalf("token.New: %v", err)
	}
	raw, err := issuer.Issue(userID)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+raw)

	var authenticated *http.Request
	middleware.RequireAuth(issuer)(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		authenticated = r
	})).ServeHTTP(httptest.NewRecorder(), req)
	if authenticated == nil {
		t.Fatalf("RequireAuth rejected the test token for subject %q", userID)
	}
	return authenticated
}

func serve(t *testing.T, h http.HandlerFunc, req *http.Request) *httptest.ResponseRecorder {
	t.Helper()

	rec := httptest.NewRecorder()
	h(rec, req)
	return rec
}

func decode(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()

	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body %q: %v", rec.Body.String(), err)
	}
	return body
}

// assertNoSecrets is the guard that outlives any particular DTO: whatever the
// response shape becomes, these two values must never be in it.
func assertNoSecrets(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()

	body := rec.Body.String()
	for _, secret := range []string{testCardToken, testCustomerID, "mp_card_token", "mp_customer_id"} {
		if strings.Contains(body, secret) {
			t.Errorf("response body leaked %q: %s", secret, body)
		}
	}
}

const validBody = `{"card_token":"` + testCardToken + `","last_four":"4242","card_brand":"visa"}`

// --- create --------------------------------------------------------------

// The first card a user saves creates the Mercado Pago customer and becomes
// their default.
func TestCreateSavesFirstCard(t *testing.T) {
	db := &stubDB{rowAnswers: []stubRow{
		{err: pgx.ErrNoRows},                  // no stored customer id yet
		{values: []any{"person@example.com"}}, // the user's email
		savedRow(true),                        // the insert
	}}
	mp := &stubMP{customerID: testCustomerID}
	h := paymenthandler.NewHandler(db, mp)

	rec := serve(t, h.Create, authed(t, http.MethodPost, validBody, testUserID))

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201: %s", rec.Code, rec.Body)
	}
	if mp.calls != 1 {
		t.Errorf("mercado pago calls = %d, want 1", mp.calls)
	}
	if mp.lastEmail != "person@example.com" {
		t.Errorf("customer email = %q", mp.lastEmail)
	}

	body := decode(t, rec)
	if body["id"] != "22222222-2222-4222-8222-222222222222" {
		t.Errorf("id = %v", body["id"])
	}
	if body["last_four"] != "4242" || body["card_brand"] != "visa" {
		t.Errorf("body = %v", body)
	}
	if body["is_default"] != true {
		t.Errorf("is_default = %v, want true: the first card is the default", body["is_default"])
	}
	if _, ok := body["created_at"]; !ok {
		t.Error("body has no created_at")
	}
	if len(body) != 5 {
		t.Errorf("body has %d fields, want exactly 5: %v", len(body), body)
	}
	assertNoSecrets(t, rec)

	// The insert must carry the resolved customer id and the normalised
	// values.
	insert := db.calls[2]
	if !strings.Contains(insert.sql, "INSERT INTO payment_methods") {
		t.Fatalf("third statement is not the insert: %s", insert.sql)
	}
	want := []any{testUserID, testCardToken, testCustomerID, "4242", "visa"}
	for i := range want {
		if insert.args[i] != want[i] {
			t.Errorf("insert arg %d = %v, want %v", i, insert.args[i], want[i])
		}
	}
}

// A user who already has a card already has a customer. Calling Mercado Pago
// again would only earn a duplicate error we cannot recover from.
func TestCreateReusesStoredCustomerID(t *testing.T) {
	db := &stubDB{rowAnswers: []stubRow{
		{values: []any{testCustomerID}},
		savedRow(false),
	}}
	mp := &stubMP{customerID: "a-second-customer-that-must-not-be-created"}
	h := paymenthandler.NewHandler(db, mp)

	rec := serve(t, h.Create, authed(t, http.MethodPost, validBody, testUserID))

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201: %s", rec.Code, rec.Body)
	}
	if mp.calls != 0 {
		t.Errorf("mercado pago calls = %d, want 0: the stored customer id must be reused", mp.calls)
	}
	if len(db.calls) != 2 {
		t.Errorf("database queries = %d, want 2: the users table must not be read", len(db.calls))
	}
	if decode(t, rec)["is_default"] != false {
		t.Error("is_default = true, want false for a second card")
	}
	if got := db.calls[1].args[2]; got != testCustomerID {
		t.Errorf("insert customer id = %v, want the stored one", got)
	}
	assertNoSecrets(t, rec)
}

// Two concurrent first cards both see NOT EXISTS as true; the partial unique
// index rejects the loser, which is then saved as a non-default card.
func TestCreateRetriesAfterDefaultRace(t *testing.T) {
	db := &stubDB{rowAnswers: []stubRow{
		{values: []any{testCustomerID}},
		pgErr("23505"),
		savedRow(false),
	}}
	h := paymenthandler.NewHandler(db, &stubMP{})

	rec := serve(t, h.Create, authed(t, http.MethodPost, validBody, testUserID))

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201: %s", rec.Code, rec.Body)
	}
	if !strings.Contains(db.calls[2].sql, "false") {
		t.Errorf("retry statement does not force is_default false: %s", db.calls[2].sql)
	}
	if decode(t, rec)["is_default"] != false {
		t.Error("is_default = true, want false after losing the race")
	}
}

// The retry happens once. A second unique violation is a real failure.
func TestCreateRetriesOnlyOnce(t *testing.T) {
	db := &stubDB{rowAnswers: []stubRow{
		{values: []any{testCustomerID}},
		pgErr("23505"),
		pgErr("23505"),
	}}
	h := paymenthandler.NewHandler(db, &stubMP{})

	rec := serve(t, h.Create, authed(t, http.MethodPost, validBody, testUserID))

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	if len(db.calls) != 3 {
		t.Errorf("database queries = %d, want 3", len(db.calls))
	}
}

func TestCreateRejectsMalformedRequests(t *testing.T) {
	cases := map[string]string{
		"not json":            "not json",
		"empty body":          "",
		"no fields":           `{}`,
		"missing card_token":  `{"last_four":"4242","card_brand":"visa"}`,
		"blank card_token":    `{"card_token":"   ","last_four":"4242","card_brand":"visa"}`,
		"oversized token":     `{"card_token":"` + strings.Repeat("a", 513) + `","last_four":"4242","card_brand":"visa"}`,
		"missing last_four":   `{"card_token":"tok","card_brand":"visa"}`,
		"three digits":        `{"card_token":"tok","last_four":"424","card_brand":"visa"}`,
		"five digits":         `{"card_token":"tok","last_four":"42424","card_brand":"visa"}`,
		"non digits":          `{"card_token":"tok","last_four":"42a4","card_brand":"visa"}`,
		"full pan":            `{"card_token":"tok","last_four":"4242424242424242","card_brand":"visa"}`,
		"last_four as number": `{"card_token":"tok","last_four":4242,"card_brand":"visa"}`,
		"missing card_brand":  `{"card_token":"tok","last_four":"4242"}`,
		"blank card_brand":    `{"card_token":"tok","last_four":"4242","card_brand":"  "}`,
		"oversized brand":     `{"card_token":"tok","last_four":"4242","card_brand":"` + strings.Repeat("v", 41) + `"}`,
	}

	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			db := &stubDB{}
			mp := &stubMP{}
			h := paymenthandler.NewHandler(db, mp)

			rec := serve(t, h.Create, authed(t, http.MethodPost, body, testUserID))

			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body)
			}
			if len(db.calls) != 0 {
				t.Errorf("database queries = %d, want 0: validation runs before any dependency", len(db.calls))
			}
			if mp.calls != 0 {
				t.Errorf("mercado pago calls = %d, want 0", mp.calls)
			}
		})
	}
}

// "0042" is a real last-four and must survive as a string.
func TestCreateAcceptsLeadingZeroLastFour(t *testing.T) {
	db := &stubDB{rowAnswers: []stubRow{
		{values: []any{testCustomerID}},
		{values: []any{"id", "0042", "elo", false, time.Now().UTC()}},
	}}
	h := paymenthandler.NewHandler(db, &stubMP{})

	body := `{"card_token":"` + testCardToken + `","last_four":"0042","card_brand":"ELO"}`
	rec := serve(t, h.Create, authed(t, http.MethodPost, body, testUserID))

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201: %s", rec.Code, rec.Body)
	}
	if got := db.calls[1].args[3]; got != "0042" {
		t.Errorf("last_four = %v, want %q", got, "0042")
	}
	// Mercado Pago's own brand slugs are lower case; storing "ELO" would
	// make a later lookup by brand miss.
	if got := db.calls[1].args[4]; got != "elo" {
		t.Errorf("card_brand = %v, want %q", got, "elo")
	}
}

func TestCreateRequiresAuthentication(t *testing.T) {
	cases := map[string]string{
		"no user id on the context": "",
		"non-uuid subject":          "not-a-uuid",
	}

	for name, userID := range cases {
		t.Run(name, func(t *testing.T) {
			db := &stubDB{}
			h := paymenthandler.NewHandler(db, &stubMP{})

			rec := serve(t, h.Create, authed(t, http.MethodPost, validBody, userID))

			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401: %s", rec.Code, rec.Body)
			}
			if got := rec.Header().Get("WWW-Authenticate"); got != "Bearer" {
				t.Errorf("WWW-Authenticate = %q, want %q", got, "Bearer")
			}
			if len(db.calls) != 0 {
				t.Errorf("database queries = %d, want 0", len(db.calls))
			}
		})
	}
}

// A valid token for a deleted user: the credential is unusable, so 401 —
// not 404, which would confirm the account once existed.
func TestCreateRejectsDeletedUser(t *testing.T) {
	db := &stubDB{rowAnswers: []stubRow{
		{err: pgx.ErrNoRows},
		{err: pgx.ErrNoRows},
	}}
	mp := &stubMP{}
	h := paymenthandler.NewHandler(db, mp)

	rec := serve(t, h.Create, authed(t, http.MethodPost, validBody, testUserID))

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401: %s", rec.Code, rec.Body)
	}
	if mp.calls != 0 {
		t.Errorf("mercado pago calls = %d, want 0", mp.calls)
	}
}

// No access token configured: the route is still mounted and says so
// plainly, rather than 404-ing or pretending the card failed.
func TestCreateWithoutClientIsUnavailable(t *testing.T) {
	db := &stubDB{rowAnswers: []stubRow{
		{err: pgx.ErrNoRows},
		{values: []any{"person@example.com"}},
	}}
	h := paymenthandler.NewHandler(db, nil)

	rec := serve(t, h.Create, authed(t, http.MethodPost, validBody, testUserID))

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503: %s", rec.Code, rec.Body)
	}
	if got := decode(t, rec)["error"]; got != "payment provider unavailable" {
		t.Errorf("error = %q", got)
	}
}

func TestCreateMapsMercadoPagoErrors(t *testing.T) {
	cases := map[string]struct {
		err  error
		want int
	}{
		"unavailable":     {err: fmt.Errorf("%w: dial timeout", mercadopago.ErrUnavailable), want: http.StatusServiceUnavailable},
		"rate limited":    {err: fmt.Errorf("%w", mercadopago.ErrRateLimited), want: http.StatusServiceUnavailable},
		"bad credentials": {err: fmt.Errorf("%w", mercadopago.ErrAuthentication), want: http.StatusInternalServerError},
		"invalid request": {err: fmt.Errorf("%w", mercadopago.ErrInvalidRequest), want: http.StatusInternalServerError},
		"unknown failure": {err: errors.New("something else entirely"), want: http.StatusInternalServerError},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			db := &stubDB{rowAnswers: []stubRow{
				{err: pgx.ErrNoRows},
				{values: []any{"person@example.com"}},
			}}
			h := paymenthandler.NewHandler(db, &stubMP{err: tc.err})

			rec := serve(t, h.Create, authed(t, http.MethodPost, validBody, testUserID))

			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d: %s", rec.Code, tc.want, rec.Body)
			}
			// An outage must never read as a card problem, and the
			// provider's own words must never reach the client.
			if got := fmt.Sprint(decode(t, rec)["error"]); strings.Contains(got, "mercadopago") ||
				strings.Contains(got, "declined") || strings.Contains(got, "dial timeout") {
				t.Errorf("error = %q leaks provider detail", got)
			}
			if len(db.calls) != 2 {
				t.Errorf("database queries = %d, want 2: nothing may be inserted", len(db.calls))
			}
		})
	}
}

// A duplicate customer is a normal outcome — but only recoverable when
// Mercado Pago actually disclosed the existing id.
func TestCreateWhenCustomerAlreadyExists(t *testing.T) {
	t.Run("id disclosed", func(t *testing.T) {
		db := &stubDB{rowAnswers: []stubRow{
			{err: pgx.ErrNoRows},
			{values: []any{"person@example.com"}},
			savedRow(true),
		}}
		apiErr := &mercadopago.APIError{Op: "CreateCustomer", CustomerID: testCustomerID}
		// Only the constructor inside the package can set the sentinel, so
		// the wrapper below is what makes errors.Is match here.
		h := paymenthandler.NewHandler(db, &stubMP{err: wrapExisting(apiErr)})

		rec := serve(t, h.Create, authed(t, http.MethodPost, validBody, testUserID))

		if rec.Code != http.StatusCreated {
			t.Fatalf("status = %d, want 201: %s", rec.Code, rec.Body)
		}
		if got := db.calls[2].args[2]; got != testCustomerID {
			t.Errorf("insert customer id = %v, want the disclosed one", got)
		}
		assertNoSecrets(t, rec)
	})

	t.Run("id withheld", func(t *testing.T) {
		db := &stubDB{rowAnswers: []stubRow{
			{err: pgx.ErrNoRows},
			{values: []any{"person@example.com"}},
		}}
		h := paymenthandler.NewHandler(db, &stubMP{err: fmt.Errorf("%w", mercadopago.ErrCustomerAlreadyExists)})

		rec := serve(t, h.Create, authed(t, http.MethodPost, validBody, testUserID))

		// Mercado Pago has a customer we cannot name, and recovering it
		// needs an endpoint the client does not implement. Our gap, so 500.
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d, want 500: %s", rec.Code, rec.Body)
		}
		if len(db.calls) != 2 {
			t.Errorf("database queries = %d, want 2: nothing may be inserted", len(db.calls))
		}
	})
}

// wrapExisting reproduces the shape the real client returns for a duplicate
// customer: errors.Is matches the sentinel and errors.As still reaches the
// APIError for its CustomerID. The sentinel field inside APIError is
// unexported, so a test cannot build that pairing directly.
func wrapExisting(err *mercadopago.APIError) error {
	return errors.Join(err, mercadopago.ErrCustomerAlreadyExists)
}

func TestCreateReportsDatabaseFailureAsInternal(t *testing.T) {
	db := &stubDB{rowAnswers: []stubRow{{err: errors.New("connection reset")}}}
	h := paymenthandler.NewHandler(db, &stubMP{})

	rec := serve(t, h.Create, authed(t, http.MethodPost, validBody, testUserID))

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	if got := fmt.Sprint(decode(t, rec)["error"]); strings.Contains(got, "connection reset") {
		t.Errorf("error = %q leaks the underlying failure", got)
	}
}

// --- list ----------------------------------------------------------------

func TestListReturnsSavedMethods(t *testing.T) {
	now := time.Date(2026, 8, 14, 12, 0, 0, 0, time.UTC)
	db := &stubDB{rowsAnswer: &stubRows{rows: []stubRow{
		{values: []any{"id-1", "4242", "visa", true, now}},
		{values: []any{"id-2", "0042", "elo", false, now.Add(-time.Hour)}},
	}}}
	h := paymenthandler.NewHandler(db, &stubMP{})

	rec := serve(t, h.List, authed(t, http.MethodGet, "", testUserID))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body)
	}

	var body struct {
		PaymentMethods []map[string]any `json:"payment_methods"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body %q: %v", rec.Body, err)
	}
	if len(body.PaymentMethods) != 2 {
		t.Fatalf("got %d methods, want 2", len(body.PaymentMethods))
	}
	if body.PaymentMethods[0]["id"] != "id-1" || body.PaymentMethods[0]["is_default"] != true {
		t.Errorf("first method = %v, want the default card", body.PaymentMethods[0])
	}
	for _, m := range body.PaymentMethods {
		if len(m) != 5 {
			t.Errorf("method has %d fields, want exactly 5: %v", len(m), m)
		}
	}
	assertNoSecrets(t, rec)

	// Scoped to the caller and ordered in the database, not in Go.
	query := db.calls[0]
	if len(query.args) != 1 || query.args[0] != testUserID {
		t.Errorf("query args = %v, want just the caller's id", query.args)
	}
	if !strings.Contains(query.sql, "ORDER BY is_default DESC, created_at DESC") {
		t.Errorf("query does not order by default then recency: %s", query.sql)
	}
	if !strings.Contains(query.sql, "WHERE user_id = $1::uuid") {
		t.Errorf("query is not scoped to the caller: %s", query.sql)
	}
	if !db.rowsAnswer.closed {
		t.Error("rows were not closed")
	}
}

// An empty result is an empty array. A nil slice would marshal to null and
// every client would have to special-case it.
func TestListWithNoMethodsReturnsEmptyArray(t *testing.T) {
	db := &stubDB{rowsAnswer: &stubRows{}}
	h := paymenthandler.NewHandler(db, &stubMP{})

	rec := serve(t, h.List, authed(t, http.MethodGet, "", testUserID))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := strings.TrimSpace(rec.Body.String()); got != `{"payment_methods":[]}` {
		t.Errorf("body = %s, want an empty array", got)
	}
}

func TestListRequiresAuthentication(t *testing.T) {
	db := &stubDB{}
	h := paymenthandler.NewHandler(db, &stubMP{})

	rec := serve(t, h.List, authed(t, http.MethodGet, "", ""))

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	if len(db.calls) != 0 {
		t.Errorf("database queries = %d, want 0", len(db.calls))
	}
}

func TestListReportsQueryFailure(t *testing.T) {
	h := paymenthandler.NewHandler(&stubDB{rowsErr: errors.New("connection reset")}, &stubMP{})

	rec := serve(t, h.List, authed(t, http.MethodGet, "", testUserID))

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	if got := fmt.Sprint(decode(t, rec)["error"]); strings.Contains(got, "connection reset") {
		t.Errorf("error = %q leaks the underlying failure", got)
	}
}

// A failure part-way through streaming only surfaces from rows.Err(); without
// that check a truncated list would be served as a complete one.
func TestListReportsRowsError(t *testing.T) {
	db := &stubDB{rowsAnswer: &stubRows{
		rows: []stubRow{{values: []any{"id-1", "4242", "visa", true, time.Now().UTC()}}},
		err:  errors.New("connection reset mid-stream"),
	}}
	h := paymenthandler.NewHandler(db, &stubMP{})

	rec := serve(t, h.List, authed(t, http.MethodGet, "", testUserID))

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500: %s", rec.Code, rec.Body)
	}
}
