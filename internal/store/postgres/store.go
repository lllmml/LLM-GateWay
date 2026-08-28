package postgres

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"
)

type ConnectionError struct {
	Operation string
	Err       error
}

func (e *ConnectionError) Error() string {
	return "postgres " + e.Operation + " failed"
}

func (e *ConnectionError) Unwrap() error {
	return e.Err
}

type Store struct {
	pool *pgxpool.Pool
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

	return &Store{pool: pool}, nil
}

func (s *Store) Ping(ctx context.Context) error {
	return s.pool.Ping(ctx)
}

func (s *Store) Close() {
	s.pool.Close()
}
