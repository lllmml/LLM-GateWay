package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/lllmml/production-go-llm-gateway/internal/controlplane/providerconfig"
)

func (s *Store) UpsertProviderConfig(ctx context.Context, params providerconfig.UpsertParams) (providerconfig.Config, error) {
	ownerID, projectID, err := configOwnerAndProjectIDs(params.OwnerUserID, params.ProjectID)
	if err != nil {
		return providerconfig.Config{}, err
	}
	credentialID, err := parseUUID(params.CredentialID)
	if err != nil {
		return providerconfig.Config{}, providerconfig.ErrNotFound
	}
	stored, err := s.queries.UpsertProjectProviderConfigForOwner(ctx, UpsertProjectProviderConfigForOwnerParams{
		Provider:        params.Provider,
		Enabled:         params.Enabled,
		BaseUrlOverride: pgtype.Text{},
		CredentialID:    credentialID,
		ProjectID:       projectID,
		OwnerUserID:     ownerID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return providerconfig.Config{}, providerconfig.ErrNotFound
	}
	if err != nil {
		return providerconfig.Config{}, err
	}
	return domainProviderConfig(stored), nil
}

func configOwnerAndProjectIDs(ownerUserID, projectID string) (pgtype.UUID, pgtype.UUID, error) {
	ownerID, err := parseUUID(ownerUserID)
	if err != nil {
		return pgtype.UUID{}, pgtype.UUID{}, fmt.Errorf("parse provider config owner ID: %w", err)
	}
	selectedProjectID, err := parseUUID(projectID)
	if err != nil {
		return pgtype.UUID{}, pgtype.UUID{}, providerconfig.ErrNotFound
	}
	return ownerID, selectedProjectID, nil
}

func domainProviderConfig(config ProjectProviderConfig) providerconfig.Config {
	return providerconfig.Config{
		ProjectID:    formatUUID(config.ProjectID),
		Provider:     config.Provider,
		CredentialID: formatUUID(config.CredentialID),
		Enabled:      config.Enabled,
		UpdatedAt:    config.UpdatedAt.Time,
	}
}
