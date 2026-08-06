package db_test

import (
	"context"
	"errors"
	"testing"

	"github.com/tapago/tapago-api/internal/db"
)

func TestConnectRejectsEmptyURL(t *testing.T) {
	_, err := db.Connect(context.Background(), "")
	if !errors.Is(err, db.ErrEmptyDatabaseURL) {
		t.Fatalf("err = %v, want ErrEmptyDatabaseURL", err)
	}
}

func TestConnectRejectsMalformedURL(t *testing.T) {
	// Parsing fails before any network access, so this needs no database.
	_, err := db.Connect(context.Background(), "://not a dsn")
	if err == nil {
		t.Fatal("expected an error for a malformed database URL")
	}
	if errors.Is(err, db.ErrEmptyDatabaseURL) {
		t.Fatalf("err = %v, want a parse error, not ErrEmptyDatabaseURL", err)
	}
}
