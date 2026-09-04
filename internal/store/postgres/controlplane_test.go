package postgres

import (
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

func TestIsProjectSlugConflictRequiresOwnerSlugConstraint(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{
			name: "owner slug constraint",
			err:  &pgconn.PgError{Code: "23505", ConstraintName: projectOwnerSlugConstraint},
			want: true,
		},
		{
			name: "wrapped owner slug constraint",
			err:  fmt.Errorf("insert project: %w", &pgconn.PgError{Code: "23505", ConstraintName: projectOwnerSlugConstraint}),
			want: true,
		},
		{
			name: "project primary key",
			err:  &pgconn.PgError{Code: "23505", ConstraintName: "projects_pkey"},
			want: false,
		},
		{
			name: "different PostgreSQL error",
			err:  &pgconn.PgError{Code: "23503", ConstraintName: projectOwnerSlugConstraint},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isProjectSlugConflict(tt.err); got != tt.want {
				t.Fatalf("isProjectSlugConflict() = %t, want %t", got, tt.want)
			}
		})
	}
}
