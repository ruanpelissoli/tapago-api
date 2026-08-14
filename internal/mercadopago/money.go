package mercadopago

import (
	"fmt"
	"strconv"
	"strings"
)

// Centavos is an exact BRL amount in hundredths of a real: 1999 is R$ 19,99.
//
// Money is never a float64 here. That is the standing rule in
// internal/model/CLAUDE.md, and it matters more on this path than most:
// bets.stake_amount_brl is numeric(12,2) and the amount we hold on a card has
// to be the number we stored, exactly. R$ 19.99 has no finite binary
// representation, so a float64 round-trip can serialise as 19.989999999999998
// and — depending on how Mercado Pago rounds it — place a hold for a
// different amount than the bet records, leaving a discrepancy nobody can
// reconcile after the fact.
//
// The conversion to the JSON number Mercado Pago's transaction_amount expects
// happens in MarshalJSON and nowhere else, always with exactly two decimals.
type Centavos int64

// BRL builds a Centavos from reais and centavos, e.g. BRL(19, 99) is R$ 19,99.
func BRL(reais, centavos int64) Centavos {
	return Centavos(reais*100 + centavos)
}

// ParseBRL reads a decimal string like "19.99", "19,99", "20" or "0.05" into
// centavos. It exists for values arriving as Postgres numeric text, which is
// how pgx hands back a numeric(12,2) when it is not scanned into a float.
//
// More than two decimal places is an error rather than a silent rounding: a
// caller passing "19.999" has a bug, and quietly charging them 19.99 or 20.00
// hides it.
func ParseBRL(s string) (Centavos, error) {
	raw := strings.TrimSpace(s)
	if raw == "" {
		return 0, fmt.Errorf("mercadopago: %q is not a BRL amount", s)
	}

	neg := false
	switch raw[0] {
	case '-':
		neg, raw = true, raw[1:]
	case '+':
		raw = raw[1:]
	}
	// Mercado Pago quotes amounts with a dot; Brazilian text uses a comma.
	// Accept either rather than making the caller normalise first.
	raw = strings.ReplaceAll(raw, ",", ".")

	whole, frac, hasFrac := strings.Cut(raw, ".")
	if strings.Contains(frac, ".") {
		return 0, fmt.Errorf("mercadopago: %q is not a BRL amount", s)
	}
	if whole == "" {
		whole = "0"
	}
	if hasFrac {
		switch len(frac) {
		case 0:
			frac = "00"
		case 1:
			frac += "0"
		case 2:
		default:
			return 0, fmt.Errorf("mercadopago: %q has more than two decimal places", s)
		}
	} else {
		frac = "00"
	}

	reais, err := strconv.ParseInt(whole, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("mercadopago: %q is not a BRL amount", s)
	}
	cents, err := strconv.ParseInt(frac, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("mercadopago: %q is not a BRL amount", s)
	}

	total := reais*100 + cents
	if neg {
		total = -total
	}
	return Centavos(total), nil
}

// String renders the amount as a plain decimal with exactly two places and a
// dot separator — "19.99", "0.05", "-3.00". This is the wire form, not a
// display form: there is no "R$" and no thousands separator, because it is
// what MarshalJSON emits and what a numeric(12,2) column accepts.
func (c Centavos) String() string {
	sign := ""
	n := int64(c)
	if n < 0 {
		sign, n = "-", -n
	}
	return fmt.Sprintf("%s%d.%02d", sign, n/100, n%100)
}

// MarshalJSON writes the amount as an unquoted JSON number with two decimal
// places, which is what Mercado Pago's transaction_amount field expects.
//
// The digits are produced from the integer, never routed through a float, so
// 1999 always emits exactly 19.99. Emitting the number unquoted is safe: JSON
// has no numeric precision beyond what the text says, and Mercado Pago parses
// transaction_amount as a decimal.
func (c Centavos) MarshalJSON() ([]byte, error) {
	return []byte(c.String()), nil
}

// UnmarshalJSON reads a JSON number or string back into exact centavos.
//
// The number is decoded from its raw text rather than through float64 for the
// same reason MarshalJSON avoids one: 19.99 parsed as a float and multiplied
// by 100 is 1998.9999999999998, which truncates to R$ 19,98.
func (c *Centavos) UnmarshalJSON(data []byte) error {
	text := strings.Trim(strings.TrimSpace(string(data)), `"`)
	if text == "" || text == "null" {
		*c = 0
		return nil
	}
	parsed, err := ParseBRL(text)
	if err != nil {
		return err
	}
	*c = parsed
	return nil
}
