package apikey

import (
	"context"
	"errors"
	"strings"
	"time"

	sharedapikey "github.com/lllmml/production-go-llm-gateway/internal/apikey"
)

var ErrNotFound = errors.New("api key not found")

type Status string

const (
	StatusActive   Status = "active"
	StatusDisabled Status = "disabled"
	StatusRevoked  Status = "revoked"
)

type Key struct {
	ID         string
	ProjectID  string
	Name       string
	Prefix     string
	Status     Status
	CreatedAt  time.Time
	RevokedAt  *time.Time
	LastUsedAt *time.Time
}

type CreateParams struct {
	OwnerUserID string
	ProjectID   string
	Name        string
	Prefix      string
	KeyHash     []byte
}

type CreateResult struct {
	Key    Key
	RawKey string
}

type Store interface {
	CreateKey(context.Context, CreateParams) (Key, error)
	ListKeys(context.Context, string, string) ([]Key, error)

	// DisableKey is idempotent for active or already disabled keys. Revoked keys
	// remain revoked.
	DisableKey(context.Context, string, string, string) (Key, error)

	// RevokeKey is idempotent and terminal.
	RevokeKey(context.Context, string, string, string) (Key, error)
}

type Service struct {
	store  Store
	pepper []byte
}

func NewService(store Store, pepper []byte) (*Service, error) {
	if store == nil {
		return nil, errors.New("api key store is required")
	}
	if err := sharedapikey.ValidatePepper(pepper); err != nil {
		return nil, err
	}
	return &Service{
		store:  store,
		pepper: append([]byte(nil), pepper...),
	}, nil
}

func (s *Service) Create(ctx context.Context, ownerUserID, projectID, name string) (CreateResult, error) {
	name, err := validateName(name)
	if err != nil {
		return CreateResult{}, err
	}
	rawKey, prefix, err := sharedapikey.GenerateRawKey()
	if err != nil {
		return CreateResult{}, err
	}
	keyHash, err := sharedapikey.HashKey(rawKey, s.pepper)
	if err != nil {
		return CreateResult{}, err
	}

	key, err := s.store.CreateKey(ctx, CreateParams{
		OwnerUserID: ownerUserID,
		ProjectID:   projectID,
		Name:        name,
		Prefix:      prefix,
		KeyHash:     keyHash,
	})
	if err != nil {
		return CreateResult{}, err
	}
	return CreateResult{Key: key, RawKey: rawKey}, nil
}

func (s *Service) List(ctx context.Context, ownerUserID, projectID string) ([]Key, error) {
	return s.store.ListKeys(ctx, ownerUserID, projectID)
}

func (s *Service) Disable(ctx context.Context, ownerUserID, projectID, keyID string) (Key, error) {
	return s.store.DisableKey(ctx, ownerUserID, projectID, keyID)
}

func (s *Service) Revoke(ctx context.Context, ownerUserID, projectID, keyID string) (Key, error) {
	return s.store.RevokeKey(ctx, ownerUserID, projectID, keyID)
}

func DisabledStatus(current Status) Status {
	if current == StatusActive {
		return StatusDisabled
	}
	return current
}

func RevokedStatus(Status) Status {
	return StatusRevoked
}

type ValidationError struct {
	Field   string
	Message string
}

func (e *ValidationError) Error() string {
	return e.Field + " " + e.Message
}

func validateName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" || len(name) > 100 {
		return "", &ValidationError{Field: "name", Message: "must contain 1 to 100 bytes"}
	}
	return name, nil
}
