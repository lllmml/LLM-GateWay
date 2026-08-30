package credential

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
)

func TestServiceCreateValidatesBeforeSealingOrStore(t *testing.T) {
	store := &fakeStore{}
	sealCalls := 0
	service := newTestService(t, store, func([]byte) (SealedSecret, error) {
		sealCalls++
		return SealedSecret{}, nil
	})

	_, err := service.Create(context.Background(), "owner-1", "project-1", "invalid", "local", "secret")
	var validationErr *ValidationError
	if !errors.As(err, &validationErr) {
		t.Fatalf("Create error = %v, want validation error", err)
	}
	if sealCalls != 0 || store.createCalls != 0 {
		t.Fatalf("seal calls = %d, create calls = %d; want neither called", sealCalls, store.createCalls)
	}
}

func TestServiceCreateSealsSecretAndStoresOnlyEncryptedMaterial(t *testing.T) {
	store := &fakeStore{}
	service := newTestService(t, store, func(plaintext []byte) (SealedSecret, error) {
		if string(plaintext) != " provider-secret " {
			t.Fatalf("plaintext = %q, want exact provider secret bytes", string(plaintext))
		}
		return SealedSecret{
			Ciphertext: []byte("ciphertext"),
			Nonce:      []byte("nonce-12-byte"),
			KeyVersion: 1,
		}, nil
	})

	created, err := service.Create(context.Background(), "owner-1", "project-1", " OpenAI ", " local key ", " provider-secret ")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if created.Provider != ProviderOpenAI || created.Label != "local key" {
		t.Fatalf("created credential = %+v", created)
	}
	if store.created.OwnerUserID != "owner-1" || store.created.ProjectID != "project-1" {
		t.Fatalf("stored scope = (%q, %q), want owner/project", store.created.OwnerUserID, store.created.ProjectID)
	}
	if store.created.Provider != ProviderOpenAI || store.created.Label != "local key" {
		t.Fatalf("stored metadata = %+v", store.created)
	}
	if !bytes.Equal(store.created.SecretCiphertext, []byte("ciphertext")) || !bytes.Equal(store.created.SecretNonce, []byte("nonce-12-byte")) || store.created.KeyVersion != 1 {
		t.Fatalf("stored encrypted material = (%q, %q, %d)", store.created.SecretCiphertext, store.created.SecretNonce, store.created.KeyVersion)
	}
	if bytes.Contains(store.created.SecretCiphertext, []byte("provider-secret")) {
		t.Fatal("store received plaintext in ciphertext field")
	}
}

func TestServiceDoesNotLeakSecretOnSealOrStoreErrors(t *testing.T) {
	for _, test := range []struct {
		name string
		seal SealSecret
		err  error
	}{
		{
			name: "seal failure",
			seal: func([]byte) (SealedSecret, error) {
				return SealedSecret{}, errors.New("seal failed")
			},
		},
		{
			name: "store failure",
			seal: func([]byte) (SealedSecret, error) {
				return SealedSecret{Ciphertext: []byte("ciphertext"), Nonce: []byte("nonce-12-byte"), KeyVersion: 1}, nil
			},
			err: errors.New("persist failed"),
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			store := &fakeStore{err: test.err}
			service := newTestService(t, store, test.seal)

			_, err := service.Create(context.Background(), "owner-1", "project-1", "openai", "local", "secret-value")
			if err == nil {
				t.Fatal("Create succeeded unexpectedly")
			}
			if strings.Contains(err.Error(), "secret-value") {
				t.Fatalf("error leaked secret material: %v", err)
			}
		})
	}
}

func TestServiceRotateSealsNewSecretAndDelegatesScope(t *testing.T) {
	store := &fakeStore{}
	service := newTestService(t, store, func(plaintext []byte) (SealedSecret, error) {
		if string(plaintext) != " rotated-secret " {
			t.Fatalf("rotate plaintext = %q, want exact rotated secret bytes", string(plaintext))
		}
		return SealedSecret{Ciphertext: []byte("new-ciphertext"), Nonce: []byte("new-nonce-12"), KeyVersion: 2}, nil
	})

	_, err := service.Rotate(context.Background(), "owner-1", "project-1", "credential-1", " rotated-secret ")
	if err != nil {
		t.Fatalf("Rotate: %v", err)
	}
	if store.rotated.OwnerUserID != "owner-1" || store.rotated.ProjectID != "project-1" || store.rotated.CredentialID != "credential-1" {
		t.Fatalf("rotate scope = %+v", store.rotated)
	}
	if !bytes.Equal(store.rotated.SecretCiphertext, []byte("new-ciphertext")) || store.rotated.KeyVersion != 2 {
		t.Fatalf("rotate encrypted material = %+v", store.rotated)
	}
}

func TestServiceDelegatesListAndDisable(t *testing.T) {
	store := &fakeStore{}
	service := newTestService(t, store, testSeal)

	if _, err := service.List(context.Background(), "owner-1", "project-1"); err != nil {
		t.Fatalf("List: %v", err)
	}
	if _, err := service.Disable(context.Background(), "owner-1", "project-1", "credential-1"); err != nil {
		t.Fatalf("Disable: %v", err)
	}
	if store.lastOwner != "owner-1" || store.lastProject != "project-1" || store.lastCredential != "credential-1" {
		t.Fatalf("delegated scope = (%q, %q, %q)", store.lastOwner, store.lastProject, store.lastCredential)
	}
}

func newTestService(t *testing.T, store Store, seal SealSecret) *Service {
	t.Helper()

	service, err := NewService(store, seal)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	return service
}

func testSeal([]byte) (SealedSecret, error) {
	return SealedSecret{Ciphertext: []byte("ciphertext"), Nonce: []byte("nonce-12-byte"), KeyVersion: 1}, nil
}

type fakeStore struct {
	createCalls    int
	created        CreateParams
	rotated        RotateParams
	lastOwner      string
	lastProject    string
	lastCredential string
	err            error
}

func (s *fakeStore) CreateCredential(_ context.Context, params CreateParams) (Credential, error) {
	s.createCalls++
	s.created = params
	if s.err != nil {
		return Credential{}, s.err
	}
	return Credential{
		ID:         "credential-1",
		ProjectID:  params.ProjectID,
		Provider:   params.Provider,
		Label:      params.Label,
		Status:     StatusActive,
		KeyVersion: params.KeyVersion,
	}, nil
}

func (s *fakeStore) ListCredentials(_ context.Context, ownerUserID, projectID string) ([]Credential, error) {
	s.lastOwner = ownerUserID
	s.lastProject = projectID
	return nil, s.err
}

func (s *fakeStore) RotateCredential(_ context.Context, params RotateParams) (Credential, error) {
	s.rotated = params
	if s.err != nil {
		return Credential{}, s.err
	}
	return Credential{ID: params.CredentialID, ProjectID: params.ProjectID, Status: StatusActive, KeyVersion: params.KeyVersion}, nil
}

func (s *fakeStore) DisableCredential(_ context.Context, ownerUserID, projectID, credentialID string) (Credential, error) {
	s.lastOwner = ownerUserID
	s.lastProject = projectID
	s.lastCredential = credentialID
	if s.err != nil {
		return Credential{}, s.err
	}
	return Credential{ID: credentialID, ProjectID: projectID, Status: StatusDisabled}, nil
}
