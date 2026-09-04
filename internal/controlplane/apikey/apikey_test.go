package apikey

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"strings"
	"testing"

	sharedapikey "github.com/lllmml/production-go-llm-gateway/internal/apikey"
)

func TestPepperValidation(t *testing.T) {
	store := &fakeStore{}

	_, err := NewService(store, bytes.Repeat([]byte{0x11}, 31))
	if err == nil {
		t.Fatal("NewService succeeded with short pepper")
	}
}

func TestServiceValidatesNameBeforeStore(t *testing.T) {
	store := &fakeStore{}
	service := newTestService(t, store)

	_, err := service.Create(context.Background(), "owner-1", "project-1", strings.Repeat("a", 101))
	var validationErr *ValidationError
	if !errors.As(err, &validationErr) {
		t.Fatalf("Create error = %v, want validation error", err)
	}
	if store.createCalls != 0 {
		t.Fatalf("CreateKey calls = %d, want 0", store.createCalls)
	}
}

func TestServiceCreateStoresDigestAndReturnsRawKeyOnce(t *testing.T) {
	store := &fakeStore{}
	service := newTestService(t, store)

	result, err := service.Create(context.Background(), "owner-1", "project-1", " Gateway Key ")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	if result.RawKey == "" {
		t.Fatal("RawKey is empty")
	}
	if result.Key.Prefix == "" {
		t.Fatal("Key prefix is empty")
	}
	if store.created.Name != "Gateway Key" {
		t.Fatalf("stored name = %q, want trimmed name", store.created.Name)
	}
	if store.created.OwnerUserID != "owner-1" || store.created.ProjectID != "project-1" {
		t.Fatalf("stored scope = (%q, %q), want owner/project", store.created.OwnerUserID, store.created.ProjectID)
	}
	if store.created.Prefix != result.Key.Prefix {
		t.Fatalf("stored prefix = %q, want %q", store.created.Prefix, result.Key.Prefix)
	}
	if len(store.created.KeyHash) != sha256.Size {
		t.Fatalf("stored hash length = %d, want %d", len(store.created.KeyHash), sha256.Size)
	}
	wantHash, err := sharedapikey.HashKey(result.RawKey, bytes.Repeat([]byte{0x11}, 32))
	if err != nil {
		t.Fatalf("HashKey returned raw key: %v", err)
	}
	if !bytes.Equal(store.created.KeyHash, wantHash) {
		t.Fatal("stored hash does not match the shown-once raw key digest")
	}
	if strings.Contains(result.Key.Name, result.RawKey) {
		t.Fatal("safe key metadata contains raw key")
	}
}

func TestServiceCreateDoesNotLeakRawKeyInStoreError(t *testing.T) {
	store := &fakeStore{err: errors.New("persist failed")}
	service := newTestService(t, store)

	result, err := service.Create(context.Background(), "owner-1", "project-1", "Gateway Key")
	if err == nil {
		t.Fatal("Create succeeded with store error")
	}
	if result.RawKey != "" {
		t.Fatal("Create returned raw key after store error")
	}
	if strings.Contains(err.Error(), "pgw_") {
		t.Fatalf("error leaked raw key material: %v", err)
	}
}

func TestServiceDelegatesScopedOperations(t *testing.T) {
	store := &fakeStore{}
	service := newTestService(t, store)

	if _, err := service.List(context.Background(), "owner-1", "project-1"); err != nil {
		t.Fatalf("List: %v", err)
	}
	if _, err := service.Disable(context.Background(), "owner-1", "project-1", "key-1"); err != nil {
		t.Fatalf("Disable: %v", err)
	}
	if _, err := service.Revoke(context.Background(), "owner-1", "project-1", "key-1"); err != nil {
		t.Fatalf("Revoke: %v", err)
	}

	for operation, call := range store.calls {
		if call.ownerID != "owner-1" || call.projectID != "project-1" {
			t.Fatalf("%s scope = (%q, %q), want owner/project", operation, call.ownerID, call.projectID)
		}
	}
	if store.calls["disable"].keyID != "key-1" {
		t.Fatalf("disable key = %q, want key-1", store.calls["disable"].keyID)
	}
	if store.calls["revoke"].keyID != "key-1" {
		t.Fatalf("revoke key = %q, want key-1", store.calls["revoke"].keyID)
	}
}

func TestStatusTransitions(t *testing.T) {
	disableTests := []struct {
		current Status
		want    Status
	}{
		{current: StatusActive, want: StatusDisabled},
		{current: StatusDisabled, want: StatusDisabled},
		{current: StatusRevoked, want: StatusRevoked},
	}
	for _, test := range disableTests {
		if got := DisabledStatus(test.current); got != test.want {
			t.Fatalf("disable %s = %s, want %s", test.current, got, test.want)
		}
	}

	for _, current := range []Status{StatusActive, StatusDisabled, StatusRevoked} {
		if got := RevokedStatus(current); got != StatusRevoked {
			t.Fatalf("revoke %s = %s, want %s", current, got, StatusRevoked)
		}
	}
}

func newTestService(t *testing.T, store Store) *Service {
	t.Helper()

	service, err := NewService(store, bytes.Repeat([]byte{0x11}, 32))
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	return service
}

type storeCall struct {
	ownerID   string
	projectID string
	keyID     string
}

type fakeStore struct {
	createCalls int
	created     CreateParams
	calls       map[string]storeCall
	err         error
}

func (s *fakeStore) record(operation, ownerID, projectID, keyID string) {
	if s.calls == nil {
		s.calls = make(map[string]storeCall)
	}
	s.calls[operation] = storeCall{ownerID: ownerID, projectID: projectID, keyID: keyID}
}

func (s *fakeStore) CreateKey(_ context.Context, params CreateParams) (Key, error) {
	s.createCalls++
	s.created = params
	if s.err != nil {
		return Key{}, s.err
	}
	return Key{
		ID:        "key-1",
		ProjectID: params.ProjectID,
		Name:      params.Name,
		Prefix:    params.Prefix,
		Status:    StatusActive,
	}, nil
}

func (s *fakeStore) ListKeys(_ context.Context, ownerID, projectID string) ([]Key, error) {
	s.record("list", ownerID, projectID, "")
	return nil, nil
}

func (s *fakeStore) DisableKey(_ context.Context, ownerID, projectID, keyID string) (Key, error) {
	s.record("disable", ownerID, projectID, keyID)
	return Key{ID: keyID, ProjectID: projectID, Status: StatusDisabled}, nil
}

func (s *fakeStore) RevokeKey(_ context.Context, ownerID, projectID, keyID string) (Key, error) {
	s.record("revoke", ownerID, projectID, keyID)
	return Key{ID: keyID, ProjectID: projectID, Status: StatusRevoked}, nil
}
