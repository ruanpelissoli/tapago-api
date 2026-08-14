package bet_test

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

	bethandler "github.com/tapago/tapago-api/internal/handler/bet"
	"github.com/tapago/tapago-api/internal/mercadopago"
	"github.com/tapago/tapago-api/internal/middleware"
	"github.com/tapago/tapago-api/internal/token"
)

// These tests exercise the bet handler entirely in process: a scripted stub
// stands in for Postgres and a fake client for Mercado Pago. What matters is
// the mapping from what the database and the provider say to what the client
// sees and what the bet row ends up as — above all that a provider outage
// leaves the row pending, and that no card token or provider id ever appears
// in a response.

const (
	testUserID     = "11111111-1111-4111-8111-111111111111"
	testMethodID   = "22222222-2222-4222-8222-222222222222"
	testBetID      = "33333333-3333-4333-8333-333333333333"
	testCardToken  = "card-token-super-secret"
	testCustomerID = "1234567890-AbCdEfGh"
	testPreauthID  = "9876543210"
	testEmail      = "person@example.com"
)

var testCreatedAt = time.Date(2026, 8, 14, 12, 0, 0, 0, time.UTC)

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
		case *int:
			*d = r.values[i].(int)
		case *time.Time:
			*d = r.values[i].(time.Time)
		default:
			return fmt.Errorf("scan: unsupported destination type %T", d)
		}
	}
	return nil
}

// call records one statement the handler sent, so a test can assert on the
// SQL and on the arguments that actually reached the database.
type call struct {
	sql  string
	args []any
}

// stubDB replays queued QueryRow answers in order and records every
// statement, Exec included. An unexpected extra query is an error rather than
// a silent nil, so a handler taking a path the test did not script fails
// loudly.
type stubDB struct {
	rowAnswers []stubRow
	rowCalls   int
	execErr    error
	calls      []call
	execs      []call
}

func (s *stubDB) QueryRow(_ context.Context, sql string, args ...any) pgx.Row {
	s.calls = append(s.calls, call{sql: sql, args: args})
	s.rowCalls++
	if s.rowCalls > len(s.rowAnswers) {
		return stubRow{err: fmt.Errorf("unexpected query %d: %s", s.rowCalls, sql)}
	}
	return s.rowAnswers[s.rowCalls-1]
}

func (s *stubDB) Exec(_ context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	s.calls = append(s.calls, call{sql: sql, args: args})
	s.execs = append(s.execs, call{sql: sql, args: args})
	return pgconn.CommandTag{}, s.execErr
}

// --- fake Mercado Pago client -------------------------------------------

type stubMP struct {
	preauthID string
	err       error
	calls     int
	last      mercadopago.PreAuthRequest
}

func (s *stubMP) CreateCustomer(context.Context, string) (string, error) {
	return "", errors.New("not used by this handler")
}

func (s *stubMP) CreatePreAuth(_ context.Context, req mercadopago.PreAuthRequest) (string, error) {
	s.calls++
	s.last = req
	if s.err != nil {
		return "", s.err
	}
	return s.preauthID, nil
}

// --- helpers -------------------------------------------------------------

// noBetInFlight is what the pre-check sees for a user with a free slot.
var noBetInFlight = stubRow{err: pgx.ErrNoRows}

// cardRow is what selectCardSQL returns.
var cardRow = stubRow{values: []any{testCardToken, testCustomerID, "visa", testEmail}}

// insertedRow is what the insert's RETURNING clause produces.
var insertedRow = stubRow{values: []any{testBetID, testCreatedAt}}

// happyPath scripts the three reads a successful create performs.
func happyPath() *stubDB {
	return &stubDB{rowAnswers: []stubRow{noBetInFlight, cardRow, insertedRow}}
}

func pgErr(code string) stubRow {
	return stubRow{err: &pgconn.PgError{Code: code}}
}

// authed builds a request carrying an authenticated user id, the way
// RequireAuth would. It goes through the middleware rather than writing the
// context key directly because that key is deliberately unexported.
func authed(t *testing.T, body, userID string) *http.Request {
	t.Helper()

	req := httptest.NewRequest(http.MethodPost, "/v1/bets", strings.NewReader(body))
	if userID == "" {
		return req
	}

	issuer, err := token.New("bet-handler-test-secret")
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

// authedGet is authed's counterpart for the read endpoint: same RequireAuth
// round trip, no body.
func authedGet(t *testing.T, userID string) *http.Request {
	t.Helper()

	req := httptest.NewRequest(http.MethodGet, "/v1/bets/active", nil)
	if userID == "" {
		return req
	}

	issuer, err := token.New("bet-handler-test-secret")
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
// response shape becomes, none of these may be in it.
func assertNoSecrets(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()

	body := rec.Body.String()
	for _, secret := range []string{
		testCardToken, testCustomerID, testPreauthID,
		"mp_card_token", "mp_customer_id", "mp_preauth_id",
	} {
		if strings.Contains(body, secret) {
			t.Errorf("response body leaked %q: %s", secret, body)
		}
	}
}

// assertNoProviderDetail checks the error envelope says nothing about Mercado
// Pago or the card beyond the sentinel-derived message.
func assertNoProviderDetail(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()

	got := strings.ToLower(fmt.Sprint(decode(t, rec)["error"]))
	for _, leak := range []string{"mercadopago", "cc_rejected", "status_detail", "dial timeout", "insufficient"} {
		if strings.Contains(got, leak) {
			t.Errorf("error = %q leaks provider detail (%q)", got, leak)
		}
	}
}

const validBody = `{"goal_type":"exercise","target_days":30,` +
	`"stake_amount_brl":"50.00","payment_method_id":"` + testMethodID + `"}`

// --- success -------------------------------------------------------------

func TestCreatePlacesBetAndHold(t *testing.T) {
	db := happyPath()
	mp := &stubMP{preauthID: testPreauthID}
	h := bethandler.NewHandler(db, mp)

	rec := serve(t, h.Create, authed(t, validBody, testUserID))

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201: %s", rec.Code, rec.Body)
	}

	// The pre-auth must carry the committed bet id — it seeds the idempotency
	// key — and the stake as exact centavos.
	if mp.calls != 1 {
		t.Fatalf("mercado pago calls = %d, want 1", mp.calls)
	}
	if mp.last.ExternalReference != testBetID {
		t.Errorf("external reference = %q, want the bet id %q", mp.last.ExternalReference, testBetID)
	}
	if mp.last.AmountBRL != mercadopago.BRL(50, 0) {
		t.Errorf("amount = %d centavos, want %d", mp.last.AmountBRL, mercadopago.BRL(50, 0))
	}
	if mp.last.CardToken != testCardToken || mp.last.CustomerID != testCustomerID {
		t.Errorf("card = %+v, want the stored token and customer", mp.last)
	}
	if mp.last.PayerEmail != testEmail {
		t.Errorf("payer email = %q, want %q", mp.last.PayerEmail, testEmail)
	}
	// Mercado Pago's payment_method_id is the *brand*, read out of the saved
	// card — not our payment_methods.id, which the client sent.
	if mp.last.PaymentMethodID != "visa" {
		t.Errorf("payment method id = %q, want the card brand %q", mp.last.PaymentMethodID, "visa")
	}
	if mp.last.Description == "" {
		t.Error("description is empty; it appears on the cardholder's statement")
	}

	body := decode(t, rec)
	if body["id"] != testBetID {
		t.Errorf("id = %v, want %q", body["id"], testBetID)
	}
	if body["status"] != "active" {
		t.Errorf("status = %v, want active", body["status"])
	}
	if body["goal_type"] != "exercise" {
		t.Errorf("goal_type = %v", body["goal_type"])
	}
	if body["target_days"] != float64(30) {
		t.Errorf("target_days = %v", body["target_days"])
	}
	// A string, not a JSON number: a client parsing 50.00 into a double
	// re-introduces the imprecision this whole path avoids.
	if got, ok := body["stake_amount_brl"].(string); !ok || got != "50.00" {
		t.Errorf("stake_amount_brl = %#v, want the string %q", body["stake_amount_brl"], "50.00")
	}
	if _, ok := body["created_at"]; !ok {
		t.Error("body has no created_at")
	}
	if len(body) != 6 {
		t.Errorf("body has %d fields, want exactly 6: %v", len(body), body)
	}
	assertNoSecrets(t, rec)

	// The insert stores the stake as exact numeric text and the row starts
	// pending; the settling update is what makes it active.
	insert := db.calls[2]
	if !strings.Contains(insert.sql, "INSERT INTO bets") || !strings.Contains(insert.sql, "'pending'") {
		t.Fatalf("third statement is not the pending insert: %s", insert.sql)
	}
	if got := insert.args[3]; got != "50.00" {
		t.Errorf("stored stake = %#v, want the string %q", got, "50.00")
	}
	if len(db.execs) != 1 {
		t.Fatalf("exec statements = %d, want 1", len(db.execs))
	}
	activate := db.execs[0]
	if !strings.Contains(activate.sql, "'active'") || !strings.Contains(activate.sql, "updated_at = now()") {
		t.Errorf("settling update does not activate the row: %s", activate.sql)
	}
	if activate.args[0] != testBetID || activate.args[1] != testPreauthID {
		t.Errorf("settling update args = %v, want the bet and pre-auth ids", activate.args)
	}
}

// The card lookup must be scoped to the caller: it is the authorisation check
// as well as the read.
func TestCreateScopesCardLookupToCaller(t *testing.T) {
	db := happyPath()
	h := bethandler.NewHandler(db, &stubMP{preauthID: testPreauthID})

	serve(t, h.Create, authed(t, validBody, testUserID))

	lookup := db.calls[1]
	if !strings.Contains(lookup.sql, "pm.user_id = $2::uuid") {
		t.Errorf("card lookup is not scoped to the caller: %s", lookup.sql)
	}
	if len(lookup.args) != 2 || lookup.args[0] != testMethodID || lookup.args[1] != testUserID {
		t.Errorf("card lookup args = %v, want the method id then the caller", lookup.args)
	}
}

// Amounts with real centavos are the ones a float64 would mangle. Both the
// quoted and the bare form are accepted; a comma decimal ("19,99") is not,
// because json.Number requires valid JSON number syntax even inside a string.
func TestCreateKeepsCentavosExact(t *testing.T) {
	for _, sent := range []string{`"19.99"`, `19.99`} {
		t.Run(sent, func(t *testing.T) {
			db := happyPath()
			mp := &stubMP{preauthID: testPreauthID}
			h := bethandler.NewHandler(db, mp)

			body := `{"goal_type":"no_smoking","target_days":1,` +
				`"stake_amount_brl":` + sent + `,"payment_method_id":"` + testMethodID + `"}`
			rec := serve(t, h.Create, authed(t, body, testUserID))

			if rec.Code != http.StatusCreated {
				t.Fatalf("status = %d, want 201: %s", rec.Code, rec.Body)
			}
			if mp.last.AmountBRL != mercadopago.BRL(19, 99) {
				t.Errorf("amount = %d centavos, want 1999", mp.last.AmountBRL)
			}
			if got := db.calls[2].args[3]; got != "19.99" {
				t.Errorf("stored stake = %#v, want %q", got, "19.99")
			}
			if got := decode(t, rec)["stake_amount_brl"]; got != "19.99" {
				t.Errorf("stake_amount_brl = %#v, want %q", got, "19.99")
			}
		})
	}
}

// --- outcome table -------------------------------------------------------

// A declined card cancels the bet and frees the slot: the user can retry with
// another card straight away.
func TestCreateWhenCardDeclined(t *testing.T) {
	db := happyPath()
	err := errors.Join(
		&mercadopago.APIError{Op: "CreatePreAuth", StatusDetail: "cc_rejected_insufficient_amount"},
		mercadopago.ErrCardDeclined,
	)
	h := bethandler.NewHandler(db, &stubMP{err: err})

	rec := serve(t, h.Create, authed(t, validBody, testUserID))

	if rec.Code != http.StatusPaymentRequired {
		t.Fatalf("status = %d, want 402: %s", rec.Code, rec.Body)
	}
	if len(db.execs) != 1 || !strings.Contains(db.execs[0].sql, "'cancelled'") {
		t.Fatalf("bet was not cancelled: %v", db.execs)
	}
	if db.execs[0].args[0] != testBetID {
		t.Errorf("cancel targets %v, want the bet id", db.execs[0].args[0])
	}
	assertNoProviderDetail(t, rec)
	assertNoSecrets(t, rec)
}

// The whole point of the pending branch: after an outage the hold may exist,
// so the row must NOT be cancelled — that would free the slot and let the
// user place a second real hold on the same card.
func TestCreateLeavesBetPendingWhenProviderUnavailable(t *testing.T) {
	cases := map[string]error{
		"unavailable":  fmt.Errorf("%w: dial timeout", mercadopago.ErrUnavailable),
		"rate limited": fmt.Errorf("%w", mercadopago.ErrRateLimited),
		"with a payment id": errors.Join(
			&mercadopago.APIError{Op: "CreatePreAuth", PaymentID: testPreauthID},
			mercadopago.ErrUnavailable,
		),
	}

	for name, err := range cases {
		t.Run(name, func(t *testing.T) {
			db := happyPath()
			h := bethandler.NewHandler(db, &stubMP{err: err})

			rec := serve(t, h.Create, authed(t, validBody, testUserID))

			if rec.Code != http.StatusServiceUnavailable {
				t.Fatalf("status = %d, want 503: %s", rec.Code, rec.Body)
			}
			if got := decode(t, rec)["error"]; got != "payment provider unavailable" {
				t.Errorf("error = %q", got)
			}
			if len(db.execs) != 0 {
				t.Errorf("the row was updated (%v); it must stay pending", db.execs)
			}
			assertNoProviderDetail(t, rec)
			assertNoSecrets(t, rec)
		})
	}
}

// Our bug, not the card's: cancel the bet so the user is not left blocked,
// and say nothing that looks like a decline.
func TestCreateWhenProviderRejectsOurRequest(t *testing.T) {
	cases := map[string]error{
		"bad credentials": fmt.Errorf("%w", mercadopago.ErrAuthentication),
		"invalid request": fmt.Errorf("%w", mercadopago.ErrInvalidRequest),
		"unknown failure": errors.New("something else entirely"),
	}

	for name, err := range cases {
		t.Run(name, func(t *testing.T) {
			db := happyPath()
			h := bethandler.NewHandler(db, &stubMP{err: err})

			rec := serve(t, h.Create, authed(t, validBody, testUserID))

			if rec.Code != http.StatusInternalServerError {
				t.Fatalf("status = %d, want 500: %s", rec.Code, rec.Body)
			}
			if len(db.execs) != 1 || !strings.Contains(db.execs[0].sql, "'cancelled'") {
				t.Fatalf("bet was not cancelled: %v", db.execs)
			}
			if got := decode(t, rec)["error"]; got != "internal server error" {
				t.Errorf("error = %q, want the generic message", got)
			}
			assertNoProviderDetail(t, rec)
		})
	}
}

// The hold exists and we could not record it. 500, but the ids are in the log
// so the row is recoverable.
func TestCreateReportsSettlingUpdateFailure(t *testing.T) {
	db := happyPath()
	db.execErr = errors.New("connection reset")
	h := bethandler.NewHandler(db, &stubMP{preauthID: testPreauthID})

	rec := serve(t, h.Create, authed(t, validBody, testUserID))

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500: %s", rec.Code, rec.Body)
	}
	if got := fmt.Sprint(decode(t, rec)["error"]); strings.Contains(got, "connection reset") {
		t.Errorf("error = %q leaks the underlying failure", got)
	}
	assertNoSecrets(t, rec)
}

// No access token configured: the route is still mounted and says so plainly,
// and nothing is written for a hold that was never attempted.
func TestCreateWithoutClientIsUnavailable(t *testing.T) {
	db := happyPath()
	h := bethandler.NewHandler(db, nil)

	rec := serve(t, h.Create, authed(t, validBody, testUserID))

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503: %s", rec.Code, rec.Body)
	}
	if got := decode(t, rec)["error"]; got != "payment provider unavailable" {
		t.Errorf("error = %q", got)
	}
	if len(db.calls) != 0 {
		t.Errorf("database statements = %d, want 0: no bet may be written", len(db.calls))
	}
}

// --- one bet at a time ---------------------------------------------------

// The fast-fail pre-check: cheap, and it spares the card lookup entirely.
func TestCreateRejectsSecondBetViaPreCheck(t *testing.T) {
	db := &stubDB{rowAnswers: []stubRow{{values: []any{1}}}}
	mp := &stubMP{preauthID: testPreauthID}
	h := bethandler.NewHandler(db, mp)

	rec := serve(t, h.Create, authed(t, validBody, testUserID))

	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409: %s", rec.Code, rec.Body)
	}
	if len(db.calls) != 1 {
		t.Errorf("database statements = %d, want 1: nothing else may run", len(db.calls))
	}
	if mp.calls != 0 {
		t.Errorf("mercado pago calls = %d, want 0", mp.calls)
	}
}

// The guarantee: two concurrent requests both pass the pre-check, and the
// partial unique index rejects the loser.
func TestCreateRejectsSecondBetViaUniqueIndex(t *testing.T) {
	db := &stubDB{rowAnswers: []stubRow{noBetInFlight, cardRow, pgErr("23505")}}
	mp := &stubMP{preauthID: testPreauthID}
	h := bethandler.NewHandler(db, mp)

	rec := serve(t, h.Create, authed(t, validBody, testUserID))

	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409: %s", rec.Code, rec.Body)
	}
	if mp.calls != 0 {
		t.Errorf("mercado pago calls = %d, want 0: no row, no hold", mp.calls)
	}
	if len(db.execs) != 0 {
		t.Errorf("exec statements = %d, want 0", len(db.execs))
	}
}

// --- lookups -------------------------------------------------------------

// Someone else's card and a card that never existed answer identically:
// telling them apart would make this an oracle for other users' card ids.
func TestCreateWithForeignPaymentMethod(t *testing.T) {
	db := &stubDB{rowAnswers: []stubRow{noBetInFlight, {err: pgx.ErrNoRows}}}
	mp := &stubMP{}
	h := bethandler.NewHandler(db, mp)

	rec := serve(t, h.Create, authed(t, validBody, testUserID))

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404: %s", rec.Code, rec.Body)
	}
	if mp.calls != 0 {
		t.Errorf("mercado pago calls = %d, want 0", mp.calls)
	}
	if len(db.calls) != 2 {
		t.Errorf("database statements = %d, want 2: nothing may be inserted", len(db.calls))
	}
}

func TestCreateReportsDatabaseFailureAsInternal(t *testing.T) {
	cases := map[string][]stubRow{
		"pre-check": {{err: errors.New("connection reset")}},
		"card lookup": {
			noBetInFlight,
			{err: errors.New("connection reset")},
		},
		"insert": {noBetInFlight, cardRow, {err: errors.New("connection reset")}},
	}

	for name, answers := range cases {
		t.Run(name, func(t *testing.T) {
			db := &stubDB{rowAnswers: answers}
			mp := &stubMP{}
			h := bethandler.NewHandler(db, mp)

			rec := serve(t, h.Create, authed(t, validBody, testUserID))

			if rec.Code != http.StatusInternalServerError {
				t.Fatalf("status = %d, want 500: %s", rec.Code, rec.Body)
			}
			if got := fmt.Sprint(decode(t, rec)["error"]); strings.Contains(got, "connection reset") {
				t.Errorf("error = %q leaks the underlying failure", got)
			}
			if mp.calls != 0 {
				t.Errorf("mercado pago calls = %d, want 0", mp.calls)
			}
		})
	}
}

// --- validation ----------------------------------------------------------

func TestCreateRejectsMalformedRequests(t *testing.T) {
	cases := map[string]string{
		"not json":   "not json",
		"empty body": "",
		"no fields":  `{}`,

		"unknown goal type": `{"goal_type":"meditation","target_days":30,"stake_amount_brl":"50.00","payment_method_id":"` + testMethodID + `"}`,
		"blank goal type":   `{"goal_type":"  ","target_days":30,"stake_amount_brl":"50.00","payment_method_id":"` + testMethodID + `"}`,
		"missing goal type": `{"target_days":30,"stake_amount_brl":"50.00","payment_method_id":"` + testMethodID + `"}`,

		"zero target days":    `{"goal_type":"exercise","target_days":0,"stake_amount_brl":"50.00","payment_method_id":"` + testMethodID + `"}`,
		"negative days":       `{"goal_type":"exercise","target_days":-5,"stake_amount_brl":"50.00","payment_method_id":"` + testMethodID + `"}`,
		"too many days":       `{"goal_type":"exercise","target_days":366,"stake_amount_brl":"50.00","payment_method_id":"` + testMethodID + `"}`,
		"missing target days": `{"goal_type":"exercise","stake_amount_brl":"50.00","payment_method_id":"` + testMethodID + `"}`,

		"zero stake":         `{"goal_type":"exercise","target_days":30,"stake_amount_brl":"0","payment_method_id":"` + testMethodID + `"}`,
		"negative stake":     `{"goal_type":"exercise","target_days":30,"stake_amount_brl":"-1.00","payment_method_id":"` + testMethodID + `"}`,
		"three decimals":     `{"goal_type":"exercise","target_days":30,"stake_amount_brl":"19.999","payment_method_id":"` + testMethodID + `"}`,
		"stake over the cap": `{"goal_type":"exercise","target_days":30,"stake_amount_brl":"10000.01","payment_method_id":"` + testMethodID + `"}`,
		"stake not a number": `{"goal_type":"exercise","target_days":30,"stake_amount_brl":"fifty","payment_method_id":"` + testMethodID + `"}`,
		"comma decimal":      `{"goal_type":"exercise","target_days":30,"stake_amount_brl":"19,99","payment_method_id":"` + testMethodID + `"}`,
		"missing stake":      `{"goal_type":"exercise","target_days":30,"payment_method_id":"` + testMethodID + `"}`,

		"payment method not a uuid":   `{"goal_type":"exercise","target_days":30,"stake_amount_brl":"50.00","payment_method_id":"not-a-uuid"}`,
		"payment method missing":      `{"goal_type":"exercise","target_days":30,"stake_amount_brl":"50.00"}`,
		"payment method as a number":  `{"goal_type":"exercise","target_days":30,"stake_amount_brl":"50.00","payment_method_id":42}`,
		"target days as a string":     `{"goal_type":"exercise","target_days":"30","stake_amount_brl":"50.00","payment_method_id":"` + testMethodID + `"}`,
		"stake as a bool":             `{"goal_type":"exercise","target_days":30,"stake_amount_brl":true,"payment_method_id":"` + testMethodID + `"}`,
		"payment method with a space": `{"goal_type":"exercise","target_days":30,"stake_amount_brl":"50.00","payment_method_id":" ` + testMethodID[1:] + `"}`,
	}

	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			db := &stubDB{}
			mp := &stubMP{}
			h := bethandler.NewHandler(db, mp)

			rec := serve(t, h.Create, authed(t, body, testUserID))

			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body)
			}
			if len(db.calls) != 0 {
				t.Errorf("database statements = %d, want 0: validation runs before any dependency", len(db.calls))
			}
			if mp.calls != 0 {
				t.Errorf("mercado pago calls = %d, want 0", mp.calls)
			}
		})
	}
}

// The caps are inclusive at their edges, and the smallest possible stake is
// one centavo.
func TestCreateAcceptsBoundaryValues(t *testing.T) {
	cases := map[string]struct{ days, stake string }{
		"one day":        {"1", `"0.01"`},
		"a full year":    {"365", `"10000.00"`},
		"integer amount": {"30", `"50"`},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			db := happyPath()
			h := bethandler.NewHandler(db, &stubMP{preauthID: testPreauthID})

			body := `{"goal_type":"exercise","target_days":` + tc.days +
				`,"stake_amount_brl":` + tc.stake + `,"payment_method_id":"` + testMethodID + `"}`
			rec := serve(t, h.Create, authed(t, body, testUserID))

			if rec.Code != http.StatusCreated {
				t.Fatalf("status = %d, want 201: %s", rec.Code, rec.Body)
			}
		})
	}
}

// --- reading the in-flight bet -------------------------------------------

// activeRow is what selectActiveBetSQL returns for a user with a bet running.
func activeRow(status string) stubRow {
	return stubRow{values: []any{testBetID, "exercise", 30, "50.00", status, testCreatedAt}}
}

func TestActiveReturnsInFlightBet(t *testing.T) {
	db := &stubDB{rowAnswers: []stubRow{activeRow("active")}}
	h := bethandler.NewHandler(db, &stubMP{})

	rec := serve(t, h.Active, authedGet(t, testUserID))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body)
	}

	body := decode(t, rec)
	if body["id"] != testBetID {
		t.Errorf("id = %v, want %q", body["id"], testBetID)
	}
	if body["goal_type"] != "exercise" {
		t.Errorf("goal_type = %v", body["goal_type"])
	}
	if body["target_days"] != float64(30) {
		t.Errorf("target_days = %v", body["target_days"])
	}
	// A string, not a JSON number, for the same reason as on the create path.
	if got, ok := body["stake_amount_brl"].(string); !ok || got != "50.00" {
		t.Errorf("stake_amount_brl = %#v, want the string %q", body["stake_amount_brl"], "50.00")
	}
	if body["status"] != "active" {
		t.Errorf("status = %v, want active", body["status"])
	}
	if _, ok := body["created_at"]; !ok {
		t.Error("body has no created_at")
	}
	// A bare object, not wrapped, with exactly the six fields of betResponse.
	if len(body) != 6 {
		t.Errorf("body has %d fields, want exactly 6: %v", len(body), body)
	}
	assertNoSecrets(t, rec)

	// One read, scoped to the caller and to the in-flight statuses.
	if len(db.calls) != 1 {
		t.Fatalf("database statements = %d, want 1: this endpoint is one SELECT", len(db.calls))
	}
	lookup := db.calls[0]
	if !strings.Contains(lookup.sql, "user_id = $1::uuid") {
		t.Errorf("query is not scoped to the caller: %s", lookup.sql)
	}
	if !strings.Contains(lookup.sql, "'pending', 'active'") {
		t.Errorf("query does not restrict to in-flight statuses: %s", lookup.sql)
	}
	if !strings.Contains(lookup.sql, "stake_amount_brl::text") {
		t.Errorf("stake is not selected as text; money must never scan into a float: %s", lookup.sql)
	}
	if len(lookup.args) != 1 || lookup.args[0] != testUserID {
		t.Errorf("query args = %v, want just the caller's id", lookup.args)
	}
	if len(db.execs) != 0 {
		t.Errorf("exec statements = %d, want 0: this endpoint is read-only", len(db.execs))
	}
}

// The outage case the app needs: a bet stranded 'pending' still occupies the
// user's slot, so it must come back rather than reading as "no bet".
func TestActiveReturnsPendingBet(t *testing.T) {
	db := &stubDB{rowAnswers: []stubRow{activeRow("pending")}}
	h := bethandler.NewHandler(db, &stubMP{})

	rec := serve(t, h.Active, authedGet(t, testUserID))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body)
	}
	if got := decode(t, rec)["status"]; got != "pending" {
		t.Errorf("status = %v, want pending", got)
	}
	assertNoSecrets(t, rec)
}

// No row: the user never placed a bet, or every bet they placed is completed
// or cancelled. The status predicate makes both cases identical here.
func TestActiveWhenNoBet(t *testing.T) {
	db := &stubDB{rowAnswers: []stubRow{{err: pgx.ErrNoRows}}}
	h := bethandler.NewHandler(db, &stubMP{})

	rec := serve(t, h.Active, authedGet(t, testUserID))

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404: %s", rec.Code, rec.Body)
	}
	if got := decode(t, rec)["error"]; got != "no active bet" {
		t.Errorf("error = %q, want %q", got, "no active bet")
	}
}

func TestActiveReportsDatabaseFailureAsInternal(t *testing.T) {
	db := &stubDB{rowAnswers: []stubRow{{err: errors.New("connection reset")}}}
	h := bethandler.NewHandler(db, &stubMP{})

	rec := serve(t, h.Active, authedGet(t, testUserID))

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500: %s", rec.Code, rec.Body)
	}
	if got := fmt.Sprint(decode(t, rec)["error"]); got != "internal server error" {
		t.Errorf("error = %q, want the generic message", got)
	}
	if strings.Contains(rec.Body.String(), "connection reset") {
		t.Errorf("body leaks the underlying failure: %s", rec.Body)
	}
}

// Reading a bet touches no provider, so a deployment without a Mercado Pago
// access token must still answer 200 — there is no 503 path on this route.
func TestActiveWorksWithoutMercadoPagoClient(t *testing.T) {
	db := &stubDB{rowAnswers: []stubRow{activeRow("active")}}
	h := bethandler.NewHandler(db, nil)

	rec := serve(t, h.Active, authedGet(t, testUserID))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body)
	}
}

func TestActiveRequiresAuthentication(t *testing.T) {
	cases := map[string]string{
		"no user id on the context": "",
		// Must be rejected here, not by Postgres: a bad uuid cast is SQLSTATE
		// 22P02, which would surface as a 500.
		"non-uuid subject": "not-a-uuid",
	}

	for name, userID := range cases {
		t.Run(name, func(t *testing.T) {
			db := &stubDB{}
			h := bethandler.NewHandler(db, &stubMP{})

			rec := serve(t, h.Active, authedGet(t, userID))

			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401: %s", rec.Code, rec.Body)
			}
			if got := rec.Header().Get("WWW-Authenticate"); got != "Bearer" {
				t.Errorf("WWW-Authenticate = %q, want %q", got, "Bearer")
			}
			if len(db.calls) != 0 {
				t.Errorf("database statements = %d, want 0", len(db.calls))
			}
		})
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
			mp := &stubMP{}
			h := bethandler.NewHandler(db, mp)

			rec := serve(t, h.Create, authed(t, validBody, userID))

			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401: %s", rec.Code, rec.Body)
			}
			if got := rec.Header().Get("WWW-Authenticate"); got != "Bearer" {
				t.Errorf("WWW-Authenticate = %q, want %q", got, "Bearer")
			}
			if len(db.calls) != 0 || mp.calls != 0 {
				t.Errorf("dependencies were touched: %d queries, %d provider calls", len(db.calls), mp.calls)
			}
		})
	}
}
