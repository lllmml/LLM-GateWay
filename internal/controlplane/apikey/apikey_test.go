package apikey

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"strings"
	"testing"
)

func TestGenerateRawKeyFormat(t *testing.T) {
	random := bytes.NewReader(sequence(prefixRandomSize + secretRandomSize))

	rawKey, prefix, err := generateRawKey(random)
	if err != nil {
		t.Fatalf("generateRawKey: %v", err)
	}

	parsed, err := ParseRawKey(rawKey)
	if err != nil {
		t.Fatalf("ParseRawKey rejected generated key: %v", err)
	}
	if len(rawKey) != rawKeySize {
		t.Fatalf("raw key length = %d, want %d", len(rawKey), rawKeySize)
	}
	if parsed.Prefix != prefix {
		t.Fatalf("parsed prefix = %q, want generated prefix %q", parsed.Prefix, prefix)
	}
}

func TestParseRawKeyAcceptsURLSafeDashAndUnderscore(t *testing.T) {
	prefixBytes := []byte{0xfb, 0xff, 0xff, 0xfb, 0xff, 0xff}
	secretBytes := append(bytes.Repeat([]byte{0xfb, 0xff, 0xff}, 10), 0xfb, 0xff)
	randomBytes := append(append([]byte(nil), prefixBytes...), secretBytes...)

	rawKey, prefix, err := generateRawKey(bytes.NewReader(randomBytes))
	if err != nil {
		t.Fatalf("generateRawKey: %v", err)
	}
	secret := rawKey[len(rawKeyMarker)+prefixEncodedSize+1:]
	if !strings.Contains(prefix, "_") || !strings.Contains(prefix, "-") {
		t.Fatal("deterministic prefix did not exercise both URL-safe symbols")
	}
	if !strings.Contains(secret, "_") || !strings.Contains(secret, "-") {
		t.Fatal("deterministic secret did not exercise both URL-safe symbols")
	}

	parsed, err := ParseRawKey(rawKey)
	if err != nil {
		t.Fatalf("ParseRawKey rejected valid URL-safe key: %v", err)
	}
	if parsed.Prefix != prefix {
		t.Fatalf("parsed prefix = %q, want %q", parsed.Prefix, prefix)
	}
}

func TestParseRawKeyRejectsMalformedValues(t *testing.T) {
	valid := rawKeyMarker + strings.Repeat("A", prefixEncodedSize) + "_" + strings.Repeat("A", secretEncodedSize)
	separator := len(rawKeyMarker) + prefixEncodedSize
	secretStart := separator + 1
	tests := []struct {
		name  string
		value string
	}{
		{name: "empty", value: ""},
		{name: "wrong marker", value: "bad_" + valid[len(rawKeyMarker):]},
		{name: "too short", value: valid[:len(valid)-1]},
		{name: "too long", value: valid + "A"},
		{name: "wrong separator", value: valid[:separator] + "-" + valid[separator+1:]},
		{name: "invalid prefix base64url", value: valid[:len(rawKeyMarker)] + "*" + valid[len(rawKeyMarker)+1:]},
		{name: "invalid secret base64url", value: valid[:secretStart] + "*" + valid[secretStart+1:]},
		{name: "padding rejected", value: valid[:len(valid)-1] + "="},
		{name: "non-canonical trailing bits", value: valid[:len(valid)-1] + "B"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := ParseRawKey(test.value); !errors.Is(err, ErrInvalidRawKey) {
				t.Fatalf("ParseRawKey error = %v, want ErrInvalidRawKey", err)
			}
		})
	}
}

func TestGenerateRawKeyUsesIndependentRandomValues(t *testing.T) {
	first, firstPrefix, err := generateRawKey(bytes.NewReader(sequence(prefixRandomSize + secretRandomSize)))
	if err != nil {
		t.Fatalf("first generateRawKey: %v", err)
	}
	second, secondPrefix, err := generateRawKey(bytes.NewReader(sequenceFrom(prefixRandomSize+secretRandomSize, 40)))
	if err != nil {
		t.Fatalf("second generateRawKey: %v", err)
	}

	if first == second {
		t.Fatal("generated raw keys unexpectedly match")
	}
	if firstPrefix == secondPrefix {
		t.Fatal("generated visible prefixes unexpectedly match")
	}
}

func TestHashKey(t *testing.T) {
	pepper := bytes.Repeat([]byte{0x11}, 32)

	digest, err := HashKey("pgw_prefix_secret", pepper)
	if err != nil {
		t.Fatalf("HashKey: %v", err)
	}
	sameDigest, err := HashKey("pgw_prefix_secret", pepper)
	if err != nil {
		t.Fatalf("HashKey same input: %v", err)
	}
	otherDigest, err := HashKey("pgw_prefix_other", pepper)
	if err != nil {
		t.Fatalf("HashKey other input: %v", err)
	}
	otherPepperDigest, err := HashKey("pgw_prefix_secret", bytes.Repeat([]byte{0x22}, 32))
	if err != nil {
		t.Fatalf("HashKey other pepper: %v", err)
	}

	if len(digest) != sha256.Size {
		t.Fatalf("digest length = %d, want %d", len(digest), sha256.Size)
	}
	if !bytes.Equal(digest, sameDigest) {
		t.Fatal("same raw key and pepper produced different digests")
	}
	if bytes.Equal(digest, otherDigest) {
		t.Fatal("different raw key produced same digest")
	}
	if bytes.Equal(digest, otherPepperDigest) {
		t.Fatal("different pepper produced same digest")
	}
}

func TestPepperValidation(t *testing.T) {
	store := &fakeStore{}

	_, err := NewService(store, bytes.Repeat([]byte{0x11}, 31))
	if err == nil {
		t.Fatal("NewService succeeded with short pepper")
	}
	if _, err := HashKey("pgw_prefix_secret", bytes.Repeat([]byte{0x11}, 31)); err == nil {
		t.Fatal("HashKey succeeded with short pepper")
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
	service.random = bytes.NewReader(sequence(prefixRandomSize + secretRandomSize))

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
	if strings.Contains(result.Key.Name, result.RawKey) {
		t.Fatal("safe key metadata contains raw key")
	}
}

func TestServiceCreateDoesNotLeakRawKeyInStoreError(t *testing.T) {
	store := &fakeStore{err: errors.New("persist failed")}
	service := newTestService(t, store)
	service.random = bytes.NewReader(sequence(prefixRandomSize + secretRandomSize))

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

func sequence(length int) []byte {
	return sequenceFrom(length, 1)
}

func sequenceFrom(length int, start byte) []byte {
	out := make([]byte, length)
	for i := range out {
		out[i] = start + byte(i)
	}
	return out
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
