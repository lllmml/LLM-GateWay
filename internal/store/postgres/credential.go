package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/lllmml/production-go-llm-gateway/internal/controlplane/credential"
)

func (s *Store) CreateCredential(ctx context.Context, params credential.CreateParams) (credential.Credential, error) {
	ownerID, projectID, err := credentialOwnerAndProjectIDs(params.OwnerUserID, params.ProjectID)
	if err != nil {
		return credential.Credential{}, err
	}
	id, err := newUUID()
	if err != nil {
		return credential.Credential{}, err
	}
	stored, err := s.queries.CreateProviderCredentialForOwner(ctx, CreateProviderCredentialForOwnerParams{
		ID:               id,
		Provider:         string(params.Provider),
		Label:            params.Label,
		SecretCiphertext: params.SecretCiphertext,
		SecretNonce:      params.SecretNonce,
		KeyVersion:       params.KeyVersion,
		ProjectID:        projectID,
		OwnerUserID:      ownerID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return credential.Credential{}, credential.ErrNotFound
	}
	if err != nil {
		return credential.Credential{}, err
	}
	return domainProviderCredential(
		stored.ID, stored.ProjectID, stored.Provider, stored.Label, stored.KeyVersion, stored.Status,
		stored.CreatedAt, stored.RotatedAt,
	), nil
}

func (s *Store) ListCredentials(ctx context.Context, ownerUserID, projectID string) ([]credential.Credential, error) {
	ownerID, selectedProjectID, err := credentialOwnerAndProjectIDs(ownerUserID, projectID)
	if err != nil {
		return nil, err
	}
	if _, err := s.queries.GetProjectForOwner(ctx, GetProjectForOwnerParams{
		ID:          selectedProjectID,
		OwnerUserID: ownerID,
	}); errors.Is(err, pgx.ErrNoRows) {
		return nil, credential.ErrNotFound
	} else if err != nil {
		return nil, err
	}
	stored, err := s.queries.ListProviderCredentialsForOwner(ctx, ListProviderCredentialsForOwnerParams{
		ProjectID:   selectedProjectID,
		OwnerUserID: ownerID,
	})
	if err != nil {
		return nil, err
	}
	credentials := make([]credential.Credential, 0, len(stored))
	for _, current := range stored {
		credentials = append(credentials, domainProviderCredential(
			current.ID, current.ProjectID, current.Provider, current.Label, current.KeyVersion, current.Status,
			current.CreatedAt, current.RotatedAt,
		))
	}
	return credentials, nil
}

func (s *Store) RotateCredential(ctx context.Context, params credential.RotateParams) (credential.Credential, error) {
	ids, err := providerCredentialIDs(params.OwnerUserID, params.ProjectID, params.CredentialID)
	if err != nil {
		return credential.Credential{}, err
	}
	stored, err := s.queries.RotateProviderCredentialForOwner(ctx, RotateProviderCredentialForOwnerParams{
		ID:               ids.ID,
		ProjectID:        ids.ProjectID,
		OwnerUserID:      ids.OwnerUserID,
		SecretCiphertext: params.SecretCiphertext,
		SecretNonce:      params.SecretNonce,
		KeyVersion:       params.KeyVersion,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return credential.Credential{}, credential.ErrNotFound
	}
	if err != nil {
		return credential.Credential{}, err
	}
	return domainProviderCredential(
		stored.ID, stored.ProjectID, stored.Provider, stored.Label, stored.KeyVersion, stored.Status,
		stored.CreatedAt, stored.RotatedAt,
	), nil
}

func (s *Store) DisableCredential(ctx context.Context, ownerUserID, projectID, credentialID string) (credential.Credential, error) {
	ids, err := providerCredentialIDs(ownerUserID, projectID, credentialID)
	if err != nil {
		return credential.Credential{}, err
	}
	stored, err := s.queries.DisableProviderCredentialForOwner(ctx, DisableProviderCredentialForOwnerParams(ids))
	if errors.Is(err, pgx.ErrNoRows) {
		return credential.Credential{}, credential.ErrNotFound
	}
	if err != nil {
		return credential.Credential{}, err
	}
	return domainProviderCredential(
		stored.ID, stored.ProjectID, stored.Provider, stored.Label, stored.KeyVersion, stored.Status,
		stored.CreatedAt, stored.RotatedAt,
	), nil
}

type providerCredentialIDParams struct {
	ID          pgtype.UUID
	ProjectID   pgtype.UUID
	OwnerUserID pgtype.UUID
}

func providerCredentialIDs(ownerUserID, projectID, credentialID string) (providerCredentialIDParams, error) {
	ownerID, selectedProjectID, err := credentialOwnerAndProjectIDs(ownerUserID, projectID)
	if err != nil {
		return providerCredentialIDParams{}, err
	}
	id, err := parseUUID(credentialID)
	if err != nil {
		return providerCredentialIDParams{}, credential.ErrNotFound
	}
	return providerCredentialIDParams{ID: id, ProjectID: selectedProjectID, OwnerUserID: ownerID}, nil
}

func credentialOwnerAndProjectIDs(ownerUserID, projectID string) (pgtype.UUID, pgtype.UUID, error) {
	ownerID, err := parseUUID(ownerUserID)
	if err != nil {
		return pgtype.UUID{}, pgtype.UUID{}, fmt.Errorf("parse credential owner ID: %w", err)
	}
	selectedProjectID, err := parseUUID(projectID)
	if err != nil {
		return pgtype.UUID{}, pgtype.UUID{}, credential.ErrNotFound
	}
	return ownerID, selectedProjectID, nil
}

func domainProviderCredential(
	id, projectID pgtype.UUID,
	provider, label string,
	keyVersion int16,
	status string,
	createdAt, rotatedAt pgtype.Timestamptz,
) credential.Credential {
	return credential.Credential{
		ID:         formatUUID(id),
		ProjectID:  formatUUID(projectID),
		Provider:   credential.Provider(provider),
		Label:      label,
		KeyVersion: keyVersion,
		Status:     credential.Status(status),
		CreatedAt:  createdAt.Time,
		RotatedAt:  timePointer(rotatedAt),
	}
}
