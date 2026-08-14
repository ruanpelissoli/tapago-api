package model

import "time"

// PaymentMethod is a saved Mercado Pago card a user can stake a bet against.
//
// No PAN, CVV or expiry date may ever be added to this struct, nor to the
// payment_methods table behind it. Only provider identifiers and the display
// metadata needed to render "Visa ···· 4242" belong here; the card data
// itself stays with Mercado Pago, and that is what keeps this database out of
// PCI scope.
//
// Fields mirror the columns of 004_create_payment_methods.sql in order. Every
// column is NOT NULL, so no field is a pointer. As with User and Bet there
// are deliberately no json tags.
type PaymentMethod struct {
	// ID is the primary key, a Postgres uuid rendered as its canonical
	// 36-character text form. Query sites select id::text.
	ID string
	// UserID is the owning account. The foreign key is ON DELETE CASCADE —
	// the opposite call from bets — because a saved card is meaningless
	// without its user and carries no financial record of its own.
	UserID string
	// MPCardToken is the Mercado Pago card token. Those tokens are
	// short-lived and single-use, so this field is likely to be revisited
	// when the payment integration lands; it keeps the migration's name for
	// now because downstream work is written against it.
	MPCardToken string
	// MPCustomerID is the Mercado Pago customer this card belongs to. Paired
	// with the card, it is the durable handle for charging.
	MPCustomerID string
	// LastFour is the last four digits, for display only.
	//
	// A string, not an int: "0042" is a valid last-four and would lose its
	// leading zeros as a number. The database additionally enforces exactly
	// four digits, which also guards against anyone writing a full PAN into
	// this column.
	LastFour string
	// CardBrand is the network name shown next to LastFour, e.g. "visa".
	CardBrand string
	// IsDefault marks the card used when the user does not pick one. The
	// partial unique index payment_methods_user_id_default_key forbids a
	// second default per user; having none is valid.
	IsDefault bool

	CreatedAt time.Time
}
