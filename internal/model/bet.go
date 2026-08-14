package model

import (
	"time"

	"github.com/jackc/pgx/v5/pgtype"
)

// BetStatus is the lifecycle state of a bet.
//
// The four values below must match the CHECK constraint on bets.status in
// 003_create_bets.sql character-for-character. Go cannot catch a divergence:
// a status the CHECK rejects compiles fine and fails at query time, on the
// user's request, so treat this block and the migration as one edit.
type BetStatus string

const (
	// BetStatusPending is a bet that exists but has no Mercado Pago
	// pre-authorisation behind it yet.
	BetStatusPending BetStatus = "pending"
	// BetStatusActive is a bet whose stake is pre-authorised and whose goal
	// window is running.
	BetStatusActive BetStatus = "active"
	// BetStatusCompleted is a bet that reached its end, won or lost.
	BetStatusCompleted BetStatus = "completed"
	// BetStatusCancelled is a bet abandoned before it ran its course.
	BetStatusCancelled BetStatus = "cancelled"
)

// GoalType is the habit a bet is staked against.
//
// Unlike BetStatus there is no CHECK on bets.goal_type: 003_create_bets.sql
// leaves the column unconstrained because the goal taxonomy is not settled.
// These constants are therefore advisory — the database will accept any text
// — and the set is expected to grow. When a CHECK is finally added it must be
// written from this list, and the two kept in sync from then on.
type GoalType string

const (
	GoalTypeExercise  GoalType = "exercise"
	GoalTypeNoSmoking GoalType = "no_smoking"
)

// Bet is a user's staked commitment to a habit goal.
//
// Fields mirror the columns of bets (003_create_bets.sql) in order, so the
// struct reads against the migration and future Scan calls line up.
//
// This is a pure data structure. In particular there is no IsActive helper
// and no validation of target_days or the stake: the one-active-bet rule is
// enforced by the partial unique index bets_user_id_in_flight_key, and the
// range rules by CHECK constraints. Re-stating either in Go would give two
// places to disagree. As with User there are deliberately no json tags —
// response shapes are built by handlers from explicit DTOs.
type Bet struct {
	// ID is the primary key, a Postgres uuid rendered as its canonical
	// 36-character text form. Query sites select id::text.
	ID string
	// UserID is the owning account. The foreign key is ON DELETE RESTRICT,
	// not CASCADE: a bet is a financial record and must not disappear as a
	// side effect of deleting a user.
	UserID string
	// GoalType is advisory at the database level — see GoalType.
	GoalType GoalType
	// TargetDays is the length of the goal window; the database requires it
	// to be positive.
	TargetDays int
	// StakeAmountBRL is the money at risk, in Brazilian reais.
	//
	// pgtype.Numeric, never float64: BRL settles in centavos and binary
	// floating point cannot represent them exactly, so a float would lose or
	// invent centavos on the way to a real charge. pgtype ships inside
	// github.com/jackc/pgx/v5 — already this project's only driver
	// dependency, chosen (see internal/db/CLAUDE.md) precisely so numeric is
	// handled properly without driver-level conversion hacks — so it scans
	// numeric(12,2) exactly and adds no new module.
	//
	// The cost is ergonomic: this is a struct, so arithmetic goes through
	// Int/Exp or Value/Scan rather than an operator. That is accepted
	// deliberately over losing exactness.
	StakeAmountBRL pgtype.Numeric
	// Status must be one of the BetStatus constants; see the type's comment.
	Status BetStatus
	// MPPreauthID is the Mercado Pago pre-authorisation holding the stake.
	//
	// nil is the normal early state, not an error: the row exists before the
	// stake is pre-authorised, and a bet cancelled first never gets one. The
	// unique index covers only non-null values, so many bets may sit here at
	// once. nil must never be spelled as an empty string.
	MPPreauthID *string

	CreatedAt time.Time
	UpdatedAt time.Time
}
