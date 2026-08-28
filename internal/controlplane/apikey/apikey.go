package apikey

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"io"
	"strings"
	"time"
)

const (
	rawKeyPrefix     = "pgw_"
	prefixRandomSize = 6
	secretRandomSize = 32
)

var (
	ErrNotFound = errors.New("api key not found")
)

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
	random io.Reader
}

func NewService(store Store, pepper []byte) (*Service, error) {
	if store == nil {
		return nil, errors.New("api key store is required")
	}
	if err := validatePepper(pepper); err != nil {
		return nil, err
	}
	return &Service{
		store:  store,
		pepper: append([]byte(nil), pepper...),
		random: rand.Reader,
	}, nil
}

func (s *Service) Create(ctx context.Context, ownerUserID, projectID, name string) (CreateResult, error) {
	name, err := validateName(name)
	if err != nil {
		return CreateResult{}, err
	}
	rawKey, prefix, err := generateRawKey(s.random)
	if err != nil {
		return CreateResult{}, err
	}
	keyHash, err := HashKey(rawKey, s.pepper)
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

func HashKey(rawKey string, pepper []byte) ([]byte, error) {
	if err := validatePepper(pepper); err != nil {
		return nil, err
	}
	mac := hmac.New(sha256.New, pepper)
	_, _ = mac.Write([]byte(rawKey))
	return mac.Sum(nil), nil
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

func validatePepper(pepper []byte) error {
	if len(pepper) < 32 {
		return errors.New("virtual key pepper must be at least 32 bytes")
	}
	return nil
}

func generateRawKey(random io.Reader) (string, string, error) {
	if random == nil {
		random = rand.Reader
	}
	prefixBytes := make([]byte, prefixRandomSize)
	if _, err := io.ReadFull(random, prefixBytes); err != nil {
		return "", "", err
	}
	secretBytes := make([]byte, secretRandomSize)
	if _, err := io.ReadFull(random, secretBytes); err != nil {
		return "", "", err
	}
	prefix := base64.RawURLEncoding.EncodeToString(prefixBytes)
	secret := base64.RawURLEncoding.EncodeToString(secretBytes)
	return rawKeyPrefix + prefix + "_" + secret, prefix, nil
}
