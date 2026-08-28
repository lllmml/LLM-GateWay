package postgres

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/lllmml/production-go-llm-gateway/internal/controlplane"
	projectdomain "github.com/lllmml/production-go-llm-gateway/internal/controlplane/project"
)

const projectOwnerSlugConstraint = "projects_owner_user_id_slug_idx"

func (s *Store) UpsertGitHubUser(ctx context.Context, githubUser controlplane.GitHubUser) (controlplane.User, error) {
	id, err := newUUID()
	if err != nil {
		return controlplane.User{}, err
	}
	stored, err := s.queries.UpsertUserFromGitHub(ctx, UpsertUserFromGitHubParams{
		ID:          id,
		GithubID:    githubUser.GitHubID,
		GithubLogin: githubUser.GitHubLogin,
		AvatarUrl:   optionalText(githubUser.AvatarURL),
	})
	if err != nil {
		return controlplane.User{}, err
	}
	return controlUser(stored), nil
}

func (s *Store) CreateSession(ctx context.Context, session controlplane.NewSession) error {
	id, err := newUUID()
	if err != nil {
		return err
	}
	userID, err := parseUUID(session.UserID)
	if err != nil {
		return fmt.Errorf("parse session user ID: %w", err)
	}
	err = s.queries.CreateWebSession(ctx, CreateWebSessionParams{
		ID:        id,
		UserID:    userID,
		TokenHash: session.TokenHash,
		ExpiresAt: timestamptz(session.ExpiresAt),
		CreatedAt: timestamptz(session.Now),
	})
	return err
}

func (s *Store) GetSession(ctx context.Context, tokenHash []byte, now time.Time) (controlplane.Session, error) {
	row, err := s.queries.GetValidWebSessionByTokenHash(ctx, GetValidWebSessionByTokenHashParams{
		TokenHash: tokenHash,
		ExpiresAt: timestamptz(now),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return controlplane.Session{}, controlplane.ErrNotFound
	}
	if err != nil {
		return controlplane.Session{}, err
	}
	return controlplane.Session{
		User: controlplane.User{
			ID:          formatUUID(row.UserID),
			GitHubID:    row.GithubID,
			GitHubLogin: row.GithubLogin,
			AvatarURL:   textValue(row.AvatarUrl),
		},
		ExpiresAt: row.ExpiresAt.Time,
	}, nil
}

func (s *Store) DeleteSession(ctx context.Context, tokenHash []byte) error {
	return s.queries.DeleteWebSessionByTokenHash(ctx, tokenHash)
}

func (s *Store) CreateProject(ctx context.Context, params projectdomain.CreateParams) (projectdomain.Project, error) {
	ownerID, err := parseUUID(params.OwnerUserID)
	if err != nil {
		return projectdomain.Project{}, fmt.Errorf("parse project owner ID: %w", err)
	}
	id, err := newUUID()
	if err != nil {
		return projectdomain.Project{}, err
	}
	stored, err := s.queries.CreateProject(ctx, CreateProjectParams{
		ID:          id,
		OwnerUserID: ownerID,
		Name:        params.Name,
		Slug:        params.Slug,
	})
	if isProjectSlugConflict(err) {
		return projectdomain.Project{}, projectdomain.ErrConflict
	}
	if err != nil {
		return projectdomain.Project{}, err
	}
	return domainProject(stored), nil
}

func (s *Store) ListProjects(ctx context.Context, ownerUserID string) ([]projectdomain.Project, error) {
	ownerID, err := parseUUID(ownerUserID)
	if err != nil {
		return nil, fmt.Errorf("parse project owner ID: %w", err)
	}
	stored, err := s.queries.ListProjectsForOwner(ctx, ownerID)
	if err != nil {
		return nil, err
	}
	projects := make([]projectdomain.Project, 0, len(stored))
	for _, current := range stored {
		projects = append(projects, domainProject(current))
	}
	return projects, nil
}

func (s *Store) GetProject(ctx context.Context, ownerUserID, projectID string) (projectdomain.Project, error) {
	ids, err := projectIDs(ownerUserID, projectID)
	if err != nil {
		return projectdomain.Project{}, err
	}
	stored, err := s.queries.GetProjectForOwner(ctx, GetProjectForOwnerParams(ids))
	if errors.Is(err, pgx.ErrNoRows) {
		return projectdomain.Project{}, projectdomain.ErrNotFound
	}
	if err != nil {
		return projectdomain.Project{}, err
	}
	return domainProject(stored), nil
}

func (s *Store) UpdateProject(ctx context.Context, ownerUserID, projectID string, params projectdomain.UpdateParams) (projectdomain.Project, error) {
	ids, err := projectIDs(ownerUserID, projectID)
	if err != nil {
		return projectdomain.Project{}, err
	}
	stored, err := s.queries.UpdateProjectForOwner(ctx, UpdateProjectForOwnerParams{
		ID:          ids.ID,
		OwnerUserID: ids.OwnerUserID,
		Name:        optionalTextPointer(params.Name),
		Slug:        optionalTextPointer(params.Slug),
		Status:      optionalTextPointer(params.Status),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return projectdomain.Project{}, projectdomain.ErrNotFound
	}
	if isProjectSlugConflict(err) {
		return projectdomain.Project{}, projectdomain.ErrConflict
	}
	if err != nil {
		return projectdomain.Project{}, err
	}
	return domainProject(stored), nil
}

func projectIDs(ownerUserID, projectID string) (GetProjectForOwnerParams, error) {
	ownerID, err := parseUUID(ownerUserID)
	if err != nil {
		return GetProjectForOwnerParams{}, fmt.Errorf("parse project owner ID: %w", err)
	}
	id, err := parseUUID(projectID)
	if err != nil {
		return GetProjectForOwnerParams{}, projectdomain.ErrNotFound
	}
	return GetProjectForOwnerParams{ID: id, OwnerUserID: ownerID}, nil
}

func controlUser(user User) controlplane.User {
	return controlplane.User{
		ID:          formatUUID(user.ID),
		GitHubID:    user.GithubID,
		GitHubLogin: user.GithubLogin,
		AvatarURL:   textValue(user.AvatarUrl),
	}
}

func domainProject(project Project) projectdomain.Project {
	return projectdomain.Project{
		ID:          formatUUID(project.ID),
		OwnerUserID: formatUUID(project.OwnerUserID),
		Name:        project.Name,
		Slug:        project.Slug,
		Status:      project.Status,
		CreatedAt:   project.CreatedAt.Time,
		UpdatedAt:   project.UpdatedAt.Time,
	}
}

func newUUID() (pgtype.UUID, error) {
	var id pgtype.UUID
	if _, err := rand.Read(id.Bytes[:]); err != nil {
		return pgtype.UUID{}, fmt.Errorf("generate UUID: %w", err)
	}
	id.Bytes[6] = (id.Bytes[6] & 0x0f) | 0x40
	id.Bytes[8] = (id.Bytes[8] & 0x3f) | 0x80
	id.Valid = true
	return id, nil
}

func parseUUID(value string) (pgtype.UUID, error) {
	if len(value) != 36 || value[8] != '-' || value[13] != '-' || value[18] != '-' || value[23] != '-' {
		return pgtype.UUID{}, errors.New("invalid UUID")
	}
	decoded, err := hex.DecodeString(strings.ReplaceAll(value, "-", ""))
	if err != nil || len(decoded) != 16 {
		return pgtype.UUID{}, errors.New("invalid UUID")
	}
	var id pgtype.UUID
	copy(id.Bytes[:], decoded)
	id.Valid = true
	return id, nil
}

func formatUUID(id pgtype.UUID) string {
	if !id.Valid {
		return ""
	}
	bytes := id.Bytes
	return fmt.Sprintf("%x-%x-%x-%x-%x", bytes[0:4], bytes[4:6], bytes[6:8], bytes[8:10], bytes[10:16])
}

func optionalText(value string) pgtype.Text {
	return pgtype.Text{String: value, Valid: value != ""}
}

func optionalTextPointer(value *string) pgtype.Text {
	if value == nil {
		return pgtype.Text{}
	}
	return pgtype.Text{String: *value, Valid: true}
}

func textValue(value pgtype.Text) string {
	if !value.Valid {
		return ""
	}
	return value.String
}

func timestamptz(value time.Time) pgtype.Timestamptz {
	return pgtype.Timestamptz{Time: value, Valid: true}
}

func isProjectSlugConflict(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == projectOwnerSlugConstraint
}
