package postgres

import (
	"context"
	"errors"
	"net"
	"os"
	"strings"
	"syscall"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

type ConnectionError struct {
	Operation string
	Err       error
}

func (e *ConnectionError) Error() string {
	return "postgres " + e.Operation + " failed: " + e.SafeCause()
}

func (e *ConnectionError) Unwrap() error {
	return e.Err
}

func (e *ConnectionError) SafeCause() string {
	return classifyConnectionError(e.Err)
}

type Store struct {
	pool    *pgxpool.Pool
	queries *Queries
}

func Open(ctx context.Context, databaseURL string) (*Store, error) {
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		return nil, &ConnectionError{Operation: "configuration", Err: err}
	}

	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, &ConnectionError{Operation: "connection", Err: err}
	}

	return &Store{pool: pool, queries: New(pool)}, nil
}

func (s *Store) Ping(ctx context.Context) error {
	return s.pool.Ping(ctx)
}

func (s *Store) Queries() *Queries {
	return s.queries
}

func (s *Store) Close() {
	s.pool.Close()
}

func classifyConnectionError(err error) string {
	if err == nil {
		return "unknown"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "timeout"
	}
	if errors.Is(err, context.Canceled) {
		return "canceled"
	}

	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return "timeout"
	}

	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return "dns"
	}

	if errors.Is(err, syscall.ECONNREFUSED) {
		return "connection_refused"
	}

	var pathErr *os.PathError
	if errors.As(err, &pathErr) && errors.Is(pathErr.Err, syscall.ECONNREFUSED) {
		return "connection_refused"
	}

	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		if strings.HasPrefix(pgErr.Code, "28") {
			return "authentication_or_configuration"
		}
		return "server_error"
	}

	return "unclassified"
}
