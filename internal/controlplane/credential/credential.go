package credential

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"strings"
	"time"
)

var ErrNotFound = errors.New("provider credential not found")

const maxSecretBytes = 64 << 10

type Provider string

const (
	ProviderOpenAI    Provider = "openai"
	ProviderAnthropic Provider = "anthropic"
	ProviderDeepSeek  Provider = "deepseek"
)

type Status string

const (
	StatusActive   Status = "active"
	StatusDisabled Status = "disabled"
)

type Credential struct {
	ID         string
	ProjectID  string
	Provider   Provider
	Label      string
	KeyVersion int16
	Status     Status
	CreatedAt  time.Time
	RotatedAt  *time.Time
}

type CreateParams struct {
	ID               string
	OwnerUserID      string
	ProjectID        string
	Provider         Provider
	Label            string
	SecretCiphertext []byte
	SecretNonce      []byte
	KeyVersion       int16
}

type RotateParams struct {
	OwnerUserID      string
	ProjectID        string
	CredentialID     string
	SecretCiphertext []byte
	SecretNonce      []byte
	KeyVersion       int16
}

type Store interface {
	ResolveProjectID(context.Context, string, string) (string, error)
	CreateCredential(context.Context, CreateParams) (Credential, error)
	GetCredential(context.Context, string, string, string) (Credential, error)
	ListCredentials(context.Context, string, string) ([]Credential, error)
	RotateCredential(context.Context, RotateParams) (Credential, error)
	DisableCredential(context.Context, string, string, string) (Credential, error)
}

type SealedSecret struct {
	Ciphertext []byte
	Nonce      []byte
	KeyVersion int16
}

type SealContext struct {
	CredentialID string
	ProjectID    string
	Provider     Provider
}

type SealSecret func([]byte, SealContext) (SealedSecret, error)

type Service struct {
	store Store
	seal  SealSecret
}

func NewService(store Store, seal SealSecret) (*Service, error) {
	if store == nil {
		return nil, errors.New("provider credential store is required")
	}
	if seal == nil {
		return nil, errors.New("provider credential sealer is required")
	}
	return &Service{store: store, seal: seal}, nil
}

func (s *Service) Create(ctx context.Context, ownerUserID, projectID, provider, label, secret string) (Credential, error) {
	parsedProvider, label, secretBytes, err := validateCredentialInput(provider, label, secret)
	if err != nil {
		return Credential{}, err
	}
	defer clear(secretBytes)
	projectID, err = s.store.ResolveProjectID(ctx, ownerUserID, projectID)
	if err != nil {
		return Credential{}, err
	}
	credentialID, err := newCredentialID()
	if err != nil {
		return Credential{}, err
	}
	encrypted, err := s.seal(secretBytes, SealContext{
		CredentialID: credentialID,
		ProjectID:    projectID,
		Provider:     parsedProvider,
	})
	if err != nil {
		return Credential{}, err
	}
	return s.store.CreateCredential(ctx, CreateParams{
		ID:               credentialID,
		OwnerUserID:      ownerUserID,
		ProjectID:        projectID,
		Provider:         parsedProvider,
		Label:            label,
		SecretCiphertext: encrypted.Ciphertext,
		SecretNonce:      encrypted.Nonce,
		KeyVersion:       encrypted.KeyVersion,
	})
}

func (s *Service) List(ctx context.Context, ownerUserID, projectID string) ([]Credential, error) {
	return s.store.ListCredentials(ctx, ownerUserID, projectID)
}

func (s *Service) Rotate(ctx context.Context, ownerUserID, projectID, credentialID, secret string) (Credential, error) {
	secretBytes, err := validateSecret(secret)
	if err != nil {
		return Credential{}, err
	}
	defer clear(secretBytes)
	current, err := s.store.GetCredential(ctx, ownerUserID, projectID, credentialID)
	if err != nil {
		return Credential{}, err
	}
	encrypted, err := s.seal(secretBytes, SealContext{
		CredentialID: current.ID,
		ProjectID:    current.ProjectID,
		Provider:     current.Provider,
	})
	if err != nil {
		return Credential{}, err
	}
	return s.store.RotateCredential(ctx, RotateParams{
		OwnerUserID:      ownerUserID,
		ProjectID:        current.ProjectID,
		CredentialID:     current.ID,
		SecretCiphertext: encrypted.Ciphertext,
		SecretNonce:      encrypted.Nonce,
		KeyVersion:       encrypted.KeyVersion,
	})
}

func newCredentialID() (string, error) {
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return "", fmt.Errorf("generate provider credential ID: %w", err)
	}
	id[6] = (id[6] & 0x0f) | 0x40
	id[8] = (id[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", id[0:4], id[4:6], id[6:8], id[8:10], id[10:16]), nil
}

func (s *Service) Disable(ctx context.Context, ownerUserID, projectID, credentialID string) (Credential, error) {
	return s.store.DisableCredential(ctx, ownerUserID, projectID, credentialID)
}

type ValidationError struct {
	Field   string
	Message string
}

func (e *ValidationError) Error() string {
	return e.Field + " " + e.Message
}

func validateCredentialInput(provider, label, secret string) (Provider, string, []byte, error) {
	parsedProvider, err := validateProvider(provider)
	if err != nil {
		return "", "", nil, err
	}
	label, err = validateLabel(label)
	if err != nil {
		return "", "", nil, err
	}
	secretBytes, err := validateSecret(secret)
	if err != nil {
		return "", "", nil, err
	}
	return parsedProvider, label, secretBytes, nil
}

func validateProvider(provider string) (Provider, error) {
	switch Provider(strings.TrimSpace(strings.ToLower(provider))) {
	case ProviderOpenAI:
		return ProviderOpenAI, nil
	case ProviderAnthropic:
		return ProviderAnthropic, nil
	case ProviderDeepSeek:
		return ProviderDeepSeek, nil
	default:
		return "", &ValidationError{Field: "provider", Message: "must be openai, anthropic, or deepseek"}
	}
}

func validateLabel(label string) (string, error) {
	label = strings.TrimSpace(label)
	if label == "" || len(label) > 100 {
		return "", &ValidationError{Field: "label", Message: "must contain 1 to 100 bytes"}
	}
	return label, nil
}

func validateSecret(secret string) ([]byte, error) {
	if strings.TrimSpace(secret) == "" {
		return nil, &ValidationError{Field: "secret", Message: "is required"}
	}
	if len(secret) > maxSecretBytes {
		return nil, &ValidationError{Field: "secret", Message: "must be at most 65536 bytes"}
	}
	return []byte(secret), nil
}
