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
	return s.upsert(ctx, ownerUserID, projectID, "openai", credentialID, enabled)
}

func (s *Service) UpsertDeepSeek(ctx context.Context, ownerUserID, projectID, credentialID string, enabled bool) (Config, error) {
	return s.upsert(ctx, ownerUserID, projectID, "deepseek", credentialID, enabled)
}

func (s *Service) UpsertAnthropic(ctx context.Context, ownerUserID, projectID, credentialID string, enabled bool) (Config, error) {
	return s.upsert(ctx, ownerUserID, projectID, "anthropic", credentialID, enabled)
}

func (s *Service) upsert(ctx context.Context, ownerUserID, projectID, provider, credentialID string, enabled bool) (Config, error) {
	cleanProvider := strings.TrimSpace(strings.ToLower(provider))
	if cleanProvider != "openai" && cleanProvider != "deepseek" && cleanProvider != "anthropic" {
		return Config{}, &ValidationError{Field: "provider", Message: "must be openai, anthropic, or deepseek"}
	}
	cleanCredentialID := strings.TrimSpace(credentialID)
	if cleanCredentialID == "" {
		return Config{}, &ValidationError{Field: "credential_id", Message: "is required"}
	}
	return s.store.UpsertProviderConfig(ctx, UpsertParams{
		OwnerUserID:  ownerUserID,
		ProjectID:    projectID,
		Provider:     cleanProvider,
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
