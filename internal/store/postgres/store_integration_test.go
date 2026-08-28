//go:build integration

package postgres

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/lllmml/production-go-llm-gateway/internal/controlplane"
	projectdomain "github.com/lllmml/production-go-llm-gateway/internal/controlplane/project"
)

func TestOpenAndPing(t *testing.T) {
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Fatal("DATABASE_URL is required for integration tests")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	store, err := Open(ctx, databaseURL)
	if err != nil {
		t.Fatalf("open postgres: %v", err)
	}
	defer store.Close()

	if err := store.Ping(ctx); err != nil {
		t.Fatalf("ping postgres: %v", err)
	}
}

func TestControlPlaneFoundationMigrationUpAndDown(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	pool, _, cleanup := newIsolatedPool(t, ctx)
	defer cleanup()

	applyMigration(t, ctx, pool, "000001_control_plane_foundation.up.sql")

	for _, table := range []string{"users", "web_sessions", "projects"} {
		if !tableExists(t, ctx, pool, table) {
			t.Fatalf("table %s does not exist after up migration", table)
		}
	}

	applyMigration(t, ctx, pool, "000001_control_plane_foundation.down.sql")

	for _, table := range []string{"users", "web_sessions", "projects"} {
		if tableExists(t, ctx, pool, table) {
			t.Fatalf("table %s still exists after down migration", table)
		}
	}
}

func TestControlPlaneFoundationConstraints(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	store, cleanup := newMigratedStore(t, ctx)
	defer cleanup()

	queries := store.queries
	userID := newTestUUID(t)
	_, err := queries.UpsertUserFromGitHub(ctx, UpsertUserFromGitHubParams{
		ID:          userID,
		GithubID:    1001,
		GithubLogin: "owner",
	})
	if err != nil {
		t.Fatalf("insert user: %v", err)
	}

	err = queries.CreateWebSession(ctx, CreateWebSessionParams{
		ID:        newTestUUID(t),
		UserID:    userID,
		TokenHash: []byte("too-short"),
		ExpiresAt: pgtype.Timestamptz{
			Time:  time.Now().Add(time.Hour),
			Valid: true,
		},
		CreatedAt: pgtype.Timestamptz{Time: time.Now(), Valid: true},
	})
	assertPgCode(t, err, "23514")

	projectID := newTestUUID(t)
	_, err = queries.CreateProject(ctx, CreateProjectParams{
		ID:          projectID,
		OwnerUserID: userID,
		Name:        "Gateway",
		Slug:        "gateway",
	})
	if err != nil {
		t.Fatalf("insert project: %v", err)
	}

	_, err = queries.CreateProject(ctx, CreateProjectParams{
		ID:          newTestUUID(t),
		OwnerUserID: userID,
		Name:        "Gateway Duplicate",
		Slug:        "gateway",
	})
	assertPgCode(t, err, "23505")

	_, err = queries.CreateProject(ctx, CreateProjectParams{
		ID:          newTestUUID(t),
		OwnerUserID: userID,
		Name:        "Invalid Slug",
		Slug:        "invalid--slug",
	})
	assertPgCode(t, err, "23514")

	_, err = queries.UpdateProjectForOwner(ctx, UpdateProjectForOwnerParams{
		ID:          projectID,
		OwnerUserID: userID,
		Status:      pgtype.Text{String: "archived", Valid: true},
	})
	assertPgCode(t, err, "23514")
}

func TestProjectOwnershipAndSessionLookupQueries(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	store, cleanup := newMigratedStore(t, ctx)
	defer cleanup()

	queries := store.queries
	ownerID := newTestUUID(t)
	otherID := newTestUUID(t)

	owner := insertUser(t, ctx, queries, ownerID, 2001, "owner")
	insertUser(t, ctx, queries, otherID, 2002, "other")

	project, err := queries.CreateProject(ctx, CreateProjectParams{
		ID:          newTestUUID(t),
		OwnerUserID: owner.ID,
		Name:        "Control Plane",
		Slug:        "control-plane",
	})
	if err != nil {
		t.Fatalf("create project: %v", err)
	}

	got, err := queries.GetProjectForOwner(ctx, GetProjectForOwnerParams{
		ID:          project.ID,
		OwnerUserID: owner.ID,
	})
	if err != nil {
		t.Fatalf("get project for owner: %v", err)
	}
	if got.Slug != "control-plane" {
		t.Fatalf("project slug = %q, want control-plane", got.Slug)
	}

	_, err = queries.GetProjectForOwner(ctx, GetProjectForOwnerParams{
		ID:          project.ID,
		OwnerUserID: otherID,
	})
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("cross-owner project lookup error = %v, want pgx.ErrNoRows", err)
	}

	tokenHash := make([]byte, 32)
	if _, err := rand.Read(tokenHash); err != nil {
		t.Fatalf("generate token hash: %v", err)
	}
	err = queries.CreateWebSession(ctx, CreateWebSessionParams{
		ID:        newTestUUID(t),
		UserID:    owner.ID,
		TokenHash: tokenHash,
		ExpiresAt: pgtype.Timestamptz{
			Time:  time.Now().Add(time.Hour),
			Valid: true,
		},
		CreatedAt: pgtype.Timestamptz{Time: time.Now(), Valid: true},
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	sessionWithUser, err := queries.GetValidWebSessionByTokenHash(ctx, GetValidWebSessionByTokenHashParams{
		TokenHash: tokenHash,
		ExpiresAt: pgtype.Timestamptz{Time: time.Now(), Valid: true},
	})
	if err != nil {
		t.Fatalf("get valid session: %v", err)
	}
	if sessionWithUser.UserID != owner.ID {
		t.Fatalf("session lookup returned wrong session/user")
	}

	missingHash := make([]byte, 32)
	if _, err := rand.Read(missingHash); err != nil {
		t.Fatalf("generate missing hash: %v", err)
	}
	_, err = queries.GetValidWebSessionByTokenHash(ctx, GetValidWebSessionByTokenHashParams{
		TokenHash: missingHash,
		ExpiresAt: pgtype.Timestamptz{Time: time.Now(), Valid: true},
	})
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("missing session error = %v, want pgx.ErrNoRows", err)
	}
}

func TestControlPlaneStoreAdapters(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	store, cleanup := newMigratedStore(t, ctx)
	defer cleanup()

	owner, err := store.UpsertGitHubUser(ctx, controlplane.GitHubUser{
		GitHubID:    3001,
		GitHubLogin: "owner",
	})
	if err != nil {
		t.Fatalf("upsert owner: %v", err)
	}
	other, err := store.UpsertGitHubUser(ctx, controlplane.GitHubUser{
		GitHubID:    3002,
		GitHubLogin: "other",
	})
	if err != nil {
		t.Fatalf("upsert other: %v", err)
	}

	now := time.Now().UTC()
	tokenHash := make([]byte, 32)
	if _, err := rand.Read(tokenHash); err != nil {
		t.Fatalf("generate token hash: %v", err)
	}
	if err := store.CreateSession(ctx, controlplane.NewSession{
		UserID:    owner.ID,
		TokenHash: tokenHash,
		ExpiresAt: now.Add(time.Hour),
		Now:       now,
	}); err != nil {
		t.Fatalf("create session: %v", err)
	}
	session, err := store.GetSession(ctx, tokenHash, now)
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	if session.User.ID != owner.ID || session.User.GitHubLogin != owner.GitHubLogin {
		t.Fatalf("session user = %+v, want owner %+v", session.User, owner)
	}
	if err := store.DeleteSession(ctx, tokenHash); err != nil {
		t.Fatalf("delete session: %v", err)
	}
	if _, err := store.GetSession(ctx, tokenHash, now); !errors.Is(err, controlplane.ErrNotFound) {
		t.Fatalf("deleted session error = %v, want ErrNotFound", err)
	}

	created, err := store.CreateProject(ctx, projectdomain.CreateParams{
		OwnerUserID: owner.ID,
		Name:        "Gateway",
		Slug:        "gateway",
	})
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	newName := "Gateway Control Plane"
	newStatus := projectdomain.StatusDisabled
	updated, err := store.UpdateProject(ctx, owner.ID, created.ID, projectdomain.UpdateParams{
		Name:   &newName,
		Status: &newStatus,
	})
	if err != nil {
		t.Fatalf("update project: %v", err)
	}
	if updated.Name != newName || updated.Status != projectdomain.StatusDisabled || updated.Slug != created.Slug {
		t.Fatalf("updated project = %+v", updated)
	}
	if _, err := store.GetProject(ctx, other.ID, created.ID); !errors.Is(err, projectdomain.ErrNotFound) {
		t.Fatalf("cross-owner lookup error = %v, want ErrNotFound", err)
	}
	if _, err := store.CreateProject(ctx, projectdomain.CreateParams{
		OwnerUserID: owner.ID,
		Name:        "Duplicate",
		Slug:        "gateway",
	}); !errors.Is(err, projectdomain.ErrConflict) {
		t.Fatalf("duplicate slug error = %v, want ErrConflict", err)
	}
	second, err := store.CreateProject(ctx, projectdomain.CreateParams{
		OwnerUserID: owner.ID,
		Name:        "Second Project",
		Slug:        "second-project",
	})
	if err != nil {
		t.Fatalf("create second project: %v", err)
	}
	duplicateSlug := created.Slug
	if _, err := store.UpdateProject(ctx, owner.ID, second.ID, projectdomain.UpdateParams{
		Slug: &duplicateSlug,
	}); !errors.Is(err, projectdomain.ErrConflict) {
		t.Fatalf("update duplicate slug error = %v, want ErrConflict", err)
	}
}

func newMigratedStore(t *testing.T, ctx context.Context) (*Store, func()) {
	t.Helper()

	pool, _, cleanupPool := newIsolatedPool(t, ctx)
	applyMigration(t, ctx, pool, "000001_control_plane_foundation.up.sql")

	store := &Store{
		pool:    pool,
		queries: New(pool),
	}

	return store, func() {
		store.Close()
		cleanupPool()
	}
}

func newIsolatedPool(t *testing.T, ctx context.Context) (*pgxpool.Pool, string, func()) {
	t.Helper()

	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Fatal("DATABASE_URL is required for integration tests")
	}

	adminPool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatalf("connect admin pool: %v", err)
	}
	defer adminPool.Close()

	schemaName := fmt.Sprintf("test_%s_%d", strings.ToLower(sanitizeIdentifierPart(t.Name())), time.Now().UnixNano())
	schemaIdent := pgx.Identifier{schemaName}.Sanitize()
	if _, err := adminPool.Exec(ctx, "CREATE SCHEMA "+schemaIdent); err != nil {
		t.Fatalf("create schema: %v", err)
	}

	config, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		t.Fatalf("parse database url: %v", err)
	}
	config.ConnConfig.RuntimeParams["search_path"] = schemaName
	config.ConnConfig.DefaultQueryExecMode = pgx.QueryExecModeSimpleProtocol

	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatalf("connect isolated pool: %v", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		t.Fatalf("ping isolated pool: %v", err)
	}

	cleanup := func() {
		pool.Close()

		cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		cleanupPool, err := pgxpool.New(cleanupCtx, databaseURL)
		if err != nil {
			t.Logf("connect cleanup pool: %v", err)
			return
		}
		defer cleanupPool.Close()

		if _, err := cleanupPool.Exec(cleanupCtx, "DROP SCHEMA "+schemaIdent+" CASCADE"); err != nil {
			t.Logf("drop schema %s: %v", schemaName, err)
		}
	}

	return pool, schemaName, cleanup
}

func applyMigration(t *testing.T, ctx context.Context, pool *pgxpool.Pool, name string) {
	t.Helper()

	sql, err := os.ReadFile(filepath.Join(repoRoot(t), "db", "migrations", name))
	if err != nil {
		t.Fatalf("read migration %s: %v", name, err)
	}

	if _, err := pool.Exec(ctx, string(sql)); err != nil {
		t.Fatalf("apply migration %s: %v", name, err)
	}
}

func tableExists(t *testing.T, ctx context.Context, pool *pgxpool.Pool, table string) bool {
	t.Helper()

	var exists bool
	err := pool.QueryRow(ctx, "SELECT to_regclass($1) IS NOT NULL", table).Scan(&exists)
	if err != nil {
		t.Fatalf("check table %s: %v", table, err)
	}
	return exists
}

func insertUser(t *testing.T, ctx context.Context, queries *Queries, id pgtype.UUID, githubID int64, login string) User {
	t.Helper()

	user, err := queries.UpsertUserFromGitHub(ctx, UpsertUserFromGitHubParams{
		ID:          id,
		GithubID:    githubID,
		GithubLogin: login,
	})
	if err != nil {
		t.Fatalf("insert user %s: %v", login, err)
	}
	return user
}

func newTestUUID(t *testing.T) pgtype.UUID {
	t.Helper()

	var id pgtype.UUID
	if _, err := rand.Read(id.Bytes[:]); err != nil {
		t.Fatalf("generate uuid: %v", err)
	}
	id.Bytes[6] = (id.Bytes[6] & 0x0f) | 0x40
	id.Bytes[8] = (id.Bytes[8] & 0x3f) | 0x80
	id.Valid = true
	return id
}

func assertPgCode(t *testing.T, err error, code string) {
	t.Helper()

	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		t.Fatalf("error = %v, want PostgreSQL code %s", err, code)
	}
	if pgErr.Code != code {
		t.Fatalf("PostgreSQL code = %s, want %s", pgErr.Code, code)
	}
}

func repoRoot(t *testing.T) string {
	t.Helper()

	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve caller")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", "..", ".."))
}

func sanitizeIdentifierPart(value string) string {
	var builder strings.Builder
	for _, char := range value {
		if char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' || char >= '0' && char <= '9' {
			builder.WriteRune(char)
			continue
		}
		builder.WriteByte('_')
	}
	return builder.String()
}
