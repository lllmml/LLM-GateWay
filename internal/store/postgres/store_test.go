package postgres

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
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

func TestConnectionErrorProvidesSafeActionableCause(t *testing.T) {
	err := &ConnectionError{
		Operation: "connection",
		Err:       context.DeadlineExceeded,
	}

	if got := err.Error(); got != "postgres connection failed: timeout" {
		t.Fatalf("Error() = %q, want timeout classification", got)
	}
}

func TestConnectionErrorPreservesErrorChain(t *testing.T) {
	wrapped := errors.Join(errors.New("outer"), context.DeadlineExceeded)
	err := &ConnectionError{
		Operation: "connection",
		Err:       wrapped,
	}

	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("ConnectionError does not preserve errors.Is chain")
	}
}

func TestOpenTimeoutErrorIsSafeAndActionable(t *testing.T) {
	const databaseURL = "postgres://user:super-secret@10.255.255.1:5432/database"
	ctx, cancel := context.WithTimeout(context.Background(), time.Nanosecond)
	defer cancel()

	_, err := Open(ctx, databaseURL)
	if err == nil {
		t.Fatal("Open returned nil error")
	}

	var connectionErr *ConnectionError
	if !errors.As(err, &connectionErr) {
		t.Fatalf("Open error type = %T, want *ConnectionError", err)
	}
	if connectionErr.SafeCause() != "timeout" && connectionErr.SafeCause() != "canceled" {
		t.Fatalf("SafeCause() = %q, want timeout or canceled", connectionErr.SafeCause())
	}
	if strings.Contains(err.Error(), "super-secret") || strings.Contains(err.Error(), databaseURL) {
		t.Fatalf("Open error exposed database credentials: %q", err)
	}
}
