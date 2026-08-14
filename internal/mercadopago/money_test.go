package mercadopago_test

import (
	"encoding/json"
	"testing"

	"github.com/tapago/tapago-api/internal/mercadopago"
)

// The reason this type exists: R$ 19,99 has no exact binary representation,
// so a float64 amount serialises as 19.989999999999998 (or rounds to 19.99
// only by luck of the formatter). The hold placed on a card must be the
// number stored in bets.stake_amount_brl, to the centavo.
func TestCentavosMarshalsWithExactlyTwoDecimals(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		amount mercadopago.Centavos
		want   string
	}{
		{name: "the canonical float trap", amount: 1999, want: "19.99"},
		{name: "whole reais keep two places", amount: 2000, want: "20.00"},
		{name: "one centavo", amount: 1, want: "0.01"},
		{name: "five centavos", amount: 5, want: "0.05"},
		{name: "ten centavos is not one", amount: 10, want: "0.10"},
		{name: "zero", amount: 0, want: "0.00"},
		{name: "another repeating binary fraction", amount: 1010, want: "10.10"},
		{name: "0.29 rounds badly as a float", amount: 29, want: "0.29"},
		{name: "large stake", amount: 123456789, want: "1234567.89"},
		{name: "negative", amount: -1999, want: "-19.99"},
		{name: "negative under one real", amount: -5, want: "-0.05"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if got := tc.amount.String(); got != tc.want {
				t.Errorf("String() = %q, want %q", got, tc.want)
			}

			encoded, err := json.Marshal(tc.amount)
			if err != nil {
				t.Fatalf("Marshal: %v", err)
			}
			// Unquoted: transaction_amount is a JSON number, not a string.
			if string(encoded) != tc.want {
				t.Errorf("Marshal = %s, want %s", encoded, tc.want)
			}
		})
	}
}

// Embedded in a struct, the amount must still land in the document as a bare
// number with two places — this is the exact shape Mercado Pago receives.
func TestCentavosInAPayload(t *testing.T) {
	t.Parallel()

	payload := struct {
		TransactionAmount mercadopago.Centavos `json:"transaction_amount"`
	}{TransactionAmount: mercadopago.BRL(19, 99)}

	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if want := `{"transaction_amount":19.99}`; string(encoded) != want {
		t.Errorf("Marshal = %s, want %s", encoded, want)
	}

	// And it survives a decode without drifting, which a float round-trip
	// would not guarantee.
	var back struct {
		TransactionAmount mercadopago.Centavos `json:"transaction_amount"`
	}
	if err := json.Unmarshal(encoded, &back); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if back.TransactionAmount != 1999 {
		t.Errorf("round-trip = %d centavos, want 1999", back.TransactionAmount)
	}
}

func TestBRL(t *testing.T) {
	t.Parallel()

	if got := mercadopago.BRL(19, 99); got != 1999 {
		t.Errorf("BRL(19, 99) = %d, want 1999", got)
	}
	if got := mercadopago.BRL(0, 5); got != 5 {
		t.Errorf("BRL(0, 5) = %d, want 5", got)
	}
	if got := mercadopago.BRL(1000, 0); got != 100000 {
		t.Errorf("BRL(1000, 0) = %d, want 100000", got)
	}
}

func TestParseBRL(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		in      string
		want    mercadopago.Centavos
		wantErr bool
	}{
		{name: "two decimals", in: "19.99", want: 1999},
		{name: "comma separator", in: "19,99", want: 1999},
		{name: "one decimal is tenths", in: "19.9", want: 1990},
		{name: "no decimals", in: "20", want: 2000},
		{name: "trailing dot", in: "20.", want: 2000},
		{name: "leading dot", in: ".05", want: 5},
		{name: "padded", in: "  19.99  ", want: 1999},
		{name: "explicit plus", in: "+19.99", want: 1999},
		{name: "negative", in: "-19.99", want: -1999},
		{name: "postgres numeric text", in: "1234567.89", want: 123456789},
		{name: "zero", in: "0.00", want: 0},

		// Three decimals is a caller bug. Silently rounding it would charge a
		// different amount than the caller asked for and hide the mistake.
		{name: "three decimals rejected", in: "19.999", wantErr: true},
		{name: "empty", in: "", wantErr: true},
		{name: "whitespace", in: "   ", wantErr: true},
		{name: "not a number", in: "abc", wantErr: true},
		{name: "two separators", in: "1.9.9", wantErr: true},
		{name: "currency symbol", in: "R$ 19,99", wantErr: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, err := mercadopago.ParseBRL(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("ParseBRL(%q) = %d, want an error", tc.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseBRL(%q): %v", tc.in, err)
			}
			if got != tc.want {
				t.Errorf("ParseBRL(%q) = %d, want %d", tc.in, got, tc.want)
			}
		})
	}
}

// Mercado Pago quotes amounts as JSON numbers; reading one back through a
// float64 would turn 19.99 into 1998 centavos.
func TestCentavosUnmarshal(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   string
		want mercadopago.Centavos
	}{
		{name: "json number", in: `19.99`, want: 1999},
		{name: "json string", in: `"19.99"`, want: 1999},
		{name: "integer", in: `20`, want: 2000},
		{name: "null is zero", in: `null`, want: 0},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var got mercadopago.Centavos
			if err := json.Unmarshal([]byte(tc.in), &got); err != nil {
				t.Fatalf("Unmarshal(%s): %v", tc.in, err)
			}
			if got != tc.want {
				t.Errorf("Unmarshal(%s) = %d, want %d", tc.in, got, tc.want)
			}
		})
	}
}
