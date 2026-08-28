package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/lllmml/production-go-llm-gateway/internal/controlplane/apikey"
)

func (s *Store) CreateKey(ctx context.Context, params apikey.CreateParams) (apikey.Key, error) {
	ownerID, projectID, err := ownerAndProjectIDs(params.OwnerUserID, params.ProjectID)
	if err != nil {
		return apikey.Key{}, err
	}
	id, err := newUUID()
	if err != nil {
		return apikey.Key{}, err
	}
	stored, err := s.queries.CreateVirtualAPIKeyForOwner(ctx, CreateVirtualAPIKeyForOwnerParams{
		ID:          id,
		Name:        params.Name,
		KeyPrefix:   params.Prefix,
		KeyHash:     params.KeyHash,
		ProjectID:   projectID,
		OwnerUserID: ownerID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return apikey.Key{}, apikey.ErrNotFound
	}
	if err != nil {
		return apikey.Key{}, err
	}
	return domainAPIKey(stored), nil
}

func (s *Store) ListKeys(ctx context.Context, ownerUserID, projectID string) ([]apikey.Key, error) {
	ownerID, selectedProjectID, err := ownerAndProjectIDs(ownerUserID, projectID)
	if err != nil {
		return nil, err
	}
	if _, err := s.queries.GetProjectForOwner(ctx, GetProjectForOwnerParams{
		ID:          selectedProjectID,
		OwnerUserID: ownerID,
	}); errors.Is(err, pgx.ErrNoRows) {
		return nil, apikey.ErrNotFound
	} else if err != nil {
		return nil, err
	}
	stored, err := s.queries.ListVirtualAPIKeysForOwner(ctx, ListVirtualAPIKeysForOwnerParams{
		ProjectID:   selectedProjectID,
		OwnerUserID: ownerID,
	})
	if err != nil {
		return nil, err
	}
	keys := make([]apikey.Key, 0, len(stored))
	for _, current := range stored {
		keys = append(keys, domainAPIKey(current))
	}
	return keys, nil
}

func (s *Store) DisableKey(ctx context.Context, ownerUserID, projectID, keyID string) (apikey.Key, error) {
	ids, err := virtualKeyIDs(ownerUserID, projectID, keyID)
	if err != nil {
		return apikey.Key{}, err
	}
	stored, err := s.queries.DisableVirtualAPIKeyForOwner(ctx, DisableVirtualAPIKeyForOwnerParams(ids))
	return apiKeyMutationResult(stored, err)
}

func (s *Store) RevokeKey(ctx context.Context, ownerUserID, projectID, keyID string) (apikey.Key, error) {
	ids, err := virtualKeyIDs(ownerUserID, projectID, keyID)
	if err != nil {
		return apikey.Key{}, err
	}
	stored, err := s.queries.RevokeVirtualAPIKeyForOwner(ctx, RevokeVirtualAPIKeyForOwnerParams(ids))
	return apiKeyMutationResult(stored, err)
}

type virtualKeyIDParams struct {
	ID          pgtype.UUID
	ProjectID   pgtype.UUID
	OwnerUserID pgtype.UUID
}

func virtualKeyIDs(ownerUserID, projectID, keyID string) (virtualKeyIDParams, error) {
	ownerID, selectedProjectID, err := ownerAndProjectIDs(ownerUserID, projectID)
	if err != nil {
		return virtualKeyIDParams{}, err
	}
	id, err := parseUUID(keyID)
	if err != nil {
		return virtualKeyIDParams{}, apikey.ErrNotFound
	}
	return virtualKeyIDParams{ID: id, ProjectID: selectedProjectID, OwnerUserID: ownerID}, nil
}

func ownerAndProjectIDs(ownerUserID, projectID string) (pgtype.UUID, pgtype.UUID, error) {
	ownerID, err := parseUUID(ownerUserID)
	if err != nil {
		return pgtype.UUID{}, pgtype.UUID{}, fmt.Errorf("parse API key owner ID: %w", err)
	}
	selectedProjectID, err := parseUUID(projectID)
	if err != nil {
		return pgtype.UUID{}, pgtype.UUID{}, apikey.ErrNotFound
	}
	return ownerID, selectedProjectID, nil
}

func apiKeyMutationResult(stored VirtualApiKey, err error) (apikey.Key, error) {
	if errors.Is(err, pgx.ErrNoRows) {
		return apikey.Key{}, apikey.ErrNotFound
	}
	if err != nil {
		return apikey.Key{}, err
	}
	return domainAPIKey(stored), nil
}

func domainAPIKey(key VirtualApiKey) apikey.Key {
	return apikey.Key{
		ID:         formatUUID(key.ID),
		ProjectID:  formatUUID(key.ProjectID),
		Name:       key.Name,
		Prefix:     key.KeyPrefix,
		Status:     apikey.Status(key.Status),
		CreatedAt:  key.CreatedAt.Time,
		LastUsedAt: timePointer(key.LastUsedAt),
		RevokedAt:  timePointer(key.RevokedAt),
	}
}

func timePointer(value pgtype.Timestamptz) *time.Time {
	if !value.Valid {
		return nil
	}
	t := value.Time
	return &t
}
