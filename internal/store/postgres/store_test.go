package postgres

import (
	"context"
	"strings"
	"testing"
)

func TestOpenDoesNotExposeDatabaseURL(t *testing.T) {
	const databaseURL = "postgres://user:super-secret@example.invalid/database"

	_, err := Open(context.Background(), databaseURL)
	if err == nil {
		t.Fatal("Open returned nil error")
	}
	if strings.Contains(err.Error(), "super-secret") || strings.Contains(err.Error(), databaseURL) {
		t.Fatalf("Open error exposed database credentials: %q", err)
	}
}
