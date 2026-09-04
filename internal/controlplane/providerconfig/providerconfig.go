package providerconfig

import (
	"context"
	"errors"
	"strings"
	"time"
)

var ErrNotFound = errors.New("provider configuration target not found")

type Config struct {
	ProjectID    string
	Provider     string
	CredentialID string
	Enabled      bool
	UpdatedAt    time.Time
}

type UpsertParams struct {
	OwnerUserID  string
	ProjectID    string
	Provider     string
	CredentialID string
	Enabled      bool
}

type Store interface {
	UpsertProviderConfig(context.Context, UpsertParams) (Config, error)
}

type Service struct {
	store Store
}

func NewService(store Store) (*Service, error) {
	if store == nil {
		return nil, errors.New("provider config store is required")
	}
	return &Service{store: store}, nil
}

func (s *Service) UpsertOpenAI(ctx context.Context, ownerUserID, projectID, credentialID string, enabled bool) (Config, error) {
	cleanCredentialID := strings.TrimSpace(credentialID)
	if cleanCredentialID == "" {
		return Config{}, &ValidationError{Field: "credential_id", Message: "is required"}
	}
	return s.store.UpsertProviderConfig(ctx, UpsertParams{
		OwnerUserID:  ownerUserID,
		ProjectID:    projectID,
		Provider:     "openai",
		CredentialID: cleanCredentialID,
		Enabled:      enabled,
	})
}

type ValidationError struct {
	Field   string
	Message string
}

func (e *ValidationError) Error() string {
	return e.Field + " " + e.Message
}
