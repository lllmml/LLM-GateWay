//go:build integration

package postgres

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	sharedapikey "github.com/lllmml/production-go-llm-gateway/internal/apikey"
	"github.com/lllmml/production-go-llm-gateway/internal/controlplane"
	"github.com/lllmml/production-go-llm-gateway/internal/controlplane/apikey"
	"github.com/lllmml/production-go-llm-gateway/internal/controlplane/credential"
	projectdomain "github.com/lllmml/production-go-llm-gateway/internal/controlplane/project"
	"github.com/lllmml/production-go-llm-gateway/internal/security"
)

func TestOpenAndPing(t *testing.T) {
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Fatal("DATABASE_URL is required for integration tests")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	store, err := Open(ctx, databaseURL)
	if err != nil {
		t.Fatalf("open postgres: %v", err)
	}
	defer store.Close()

	if err := store.Ping(ctx); err != nil {
		t.Fatalf("ping postgres: %v", err)
	}
}

func TestControlPlaneFoundationMigrationUpAndDown(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	pool, _, cleanup := newIsolatedPool(t, ctx)
	defer cleanup()

	applyMigration(t, ctx, pool, "000001_control_plane_foundation.up.sql")

	for _, table := range []string{"users", "web_sessions", "projects"} {
		if !tableExists(t, ctx, pool, table) {
			t.Fatalf("table %s does not exist after up migration", table)
		}
	}

	applyMigration(t, ctx, pool, "000001_control_plane_foundation.down.sql")

	for _, table := range []string{"users", "web_sessions", "projects"} {
		if tableExists(t, ctx, pool, table) {
			t.Fatalf("table %s still exists after down migration", table)
		}
	}
}

func TestVirtualAPIKeyMigrationUpAndDown(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	pool, _, cleanup := newIsolatedPool(t, ctx)
	defer cleanup()

	applyMigration(t, ctx, pool, "000001_control_plane_foundation.up.sql")
	applyMigration(t, ctx, pool, "000002_virtual_api_keys.up.sql")

	if !tableExists(t, ctx, pool, "virtual_api_keys") {
		t.Fatal("virtual_api_keys table does not exist after up migration")
	}

	applyMigration(t, ctx, pool, "000002_virtual_api_keys.down.sql")
	if tableExists(t, ctx, pool, "virtual_api_keys") {
		t.Fatal("virtual_api_keys table still exists after down migration")
	}

	applyMigration(t, ctx, pool, "000001_control_plane_foundation.down.sql")
}

func TestProviderCredentialMigrationUpAndDown(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	pool, _, cleanup := newIsolatedPool(t, ctx)
	defer cleanup()

	applyMigration(t, ctx, pool, "000001_control_plane_foundation.up.sql")
	applyMigration(t, ctx, pool, "000002_virtual_api_keys.up.sql")
	applyMigration(t, ctx, pool, "000003_provider_credentials.up.sql")

	if !tableExists(t, ctx, pool, "provider_credentials") {
		t.Fatal("provider_credentials table does not exist after up migration")
	}

	applyMigration(t, ctx, pool, "000003_provider_credentials.down.sql")
	if tableExists(t, ctx, pool, "provider_credentials") {
		t.Fatal("provider_credentials table still exists after down migration")
	}

	applyMigration(t, ctx, pool, "000002_virtual_api_keys.down.sql")
	applyMigration(t, ctx, pool, "000001_control_plane_foundation.down.sql")
}

func TestProviderCredentialServicePersistsOnlyRecoverableCiphertext(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	store, cleanup := newMigratedStore(t, ctx)
	defer cleanup()

	owner, err := store.UpsertGitHubUser(ctx, controlplane.GitHubUser{
		GitHubID:    5003,
		GitHubLogin: "encrypted-credential-owner",
	})
	if err != nil {
		t.Fatalf("upsert owner: %v", err)
	}
	project, err := store.CreateProject(ctx, projectdomain.CreateParams{
		OwnerUserID: owner.ID,
		Name:        "Encrypted Credential Gateway",
		Slug:        "encrypted-credential-gateway",
	})
	if err != nil {
		t.Fatalf("create project: %v", err)
	}

	cipher, err := security.NewCredentialCipher(bytes.Repeat([]byte{9}, 32))
	if err != nil {
		t.Fatalf("create credential cipher: %v", err)
	}
	service, err := credential.NewService(store, func(secret []byte, context credential.SealContext) (credential.SealedSecret, error) {
		encrypted, err := cipher.Encrypt(secret, security.CredentialIdentity{
			CredentialID: context.CredentialID,
			ProjectID:    context.ProjectID,
			Provider:     string(context.Provider),
		})
		if err != nil {
			return credential.SealedSecret{}, err
		}
		return credential.SealedSecret{
			Ciphertext: encrypted.Ciphertext,
			Nonce:      encrypted.Nonce,
			KeyVersion: encrypted.KeyVersion,
		}, nil
	})
	if err != nil {
		t.Fatalf("create credential service: %v", err)
	}

	rawSecret := []byte("provider-secret-not-stored-in-plaintext")
	created, err := service.Create(ctx, owner.ID, project.ID, "openai", "prod", string(rawSecret))
	if err != nil {
		t.Fatalf("create encrypted credential: %v", err)
	}
	credentialID, err := parseUUID(created.ID)
	if err != nil {
		t.Fatalf("parse credential ID: %v", err)
	}

	var ciphertext, nonce []byte
	var keyVersion int16
	if err := store.pool.QueryRow(ctx, `
		SELECT secret_ciphertext, secret_nonce, key_version
		FROM provider_credentials
		WHERE id = $1
	`, credentialID).Scan(&ciphertext, &nonce, &keyVersion); err != nil {
		t.Fatalf("read encrypted credential envelope: %v", err)
	}
	if bytes.Contains(ciphertext, rawSecret) || bytes.Equal(ciphertext, rawSecret) {
		t.Fatal("database credential ciphertext contains plaintext material")
	}
	decrypted, err := cipher.Decrypt(security.EncryptedCredential{
		Ciphertext: ciphertext,
		Nonce:      nonce,
		KeyVersion: keyVersion,
	}, security.CredentialIdentity{
		CredentialID: created.ID,
		ProjectID:    created.ProjectID,
		Provider:     string(created.Provider),
	})
	if err != nil {
		t.Fatalf("decrypt persisted credential: %v", err)
	}
	if !bytes.Equal(decrypted, rawSecret) {
		t.Fatal("decrypted credential does not match the submitted secret")
	}

	rotatedSecret := []byte("rotated-provider-secret-not-stored-in-plaintext")
	rotated, err := service.Rotate(ctx, owner.ID, project.ID, created.ID, string(rotatedSecret))
	if err != nil {
		t.Fatalf("rotate encrypted credential: %v", err)
	}
	if rotated.RotatedAt == nil || rotated.Provider != created.Provider {
		t.Fatalf("rotated credential metadata = %+v", rotated)
	}
	if err := store.pool.QueryRow(ctx, `
		SELECT secret_ciphertext, secret_nonce, key_version
		FROM provider_credentials
		WHERE id = $1
	`, credentialID).Scan(&ciphertext, &nonce, &keyVersion); err != nil {
		t.Fatalf("read rotated credential envelope: %v", err)
	}
	decrypted, err = cipher.Decrypt(security.EncryptedCredential{
		Ciphertext: ciphertext,
		Nonce:      nonce,
		KeyVersion: keyVersion,
	}, security.CredentialIdentity{
		CredentialID: rotated.ID,
		ProjectID:    rotated.ProjectID,
		Provider:     string(rotated.Provider),
	})
	if err != nil {
		t.Fatalf("decrypt rotated credential: %v", err)
	}
	if !bytes.Equal(decrypted, rotatedSecret) {
		t.Fatal("decrypted rotated credential does not match the submitted secret")
	}
}

func TestControlPlaneFoundationConstraints(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	store, cleanup := newMigratedStore(t, ctx)
	defer cleanup()

	queries := store.queries
	userID := newTestUUID(t)
	_, err := queries.UpsertUserFromGitHub(ctx, UpsertUserFromGitHubParams{
		ID:          userID,
		GithubID:    1001,
		GithubLogin: "owner",
	})
	if err != nil {
		t.Fatalf("insert user: %v", err)
	}

	err = queries.CreateWebSession(ctx, CreateWebSessionParams{
		ID:        newTestUUID(t),
		UserID:    userID,
		TokenHash: []byte("too-short"),
		ExpiresAt: pgtype.Timestamptz{
			Time:  time.Now().Add(time.Hour),
			Valid: true,
		},
		CreatedAt: pgtype.Timestamptz{Time: time.Now(), Valid: true},
	})
	assertPgCode(t, err, "23514")

	projectID := newTestUUID(t)
	_, err = queries.CreateProject(ctx, CreateProjectParams{
		ID:          projectID,
		OwnerUserID: userID,
		Name:        "Gateway",
		Slug:        "gateway",
	})
	if err != nil {
		t.Fatalf("insert project: %v", err)
	}

	_, err = queries.CreateProject(ctx, CreateProjectParams{
		ID:          newTestUUID(t),
		OwnerUserID: userID,
		Name:        "Gateway Duplicate",
		Slug:        "gateway",
	})
	assertPgCode(t, err, "23505")

	_, err = queries.CreateProject(ctx, CreateProjectParams{
		ID:          newTestUUID(t),
		OwnerUserID: userID,
		Name:        "Invalid Slug",
		Slug:        "invalid--slug",
	})
	assertPgCode(t, err, "23514")

	_, err = queries.UpdateProjectForOwner(ctx, UpdateProjectForOwnerParams{
		ID:          projectID,
		OwnerUserID: userID,
		Status:      pgtype.Text{String: "archived", Valid: true},
	})
	assertPgCode(t, err, "23514")
}

func TestProviderCredentialStoreAdapterAndConstraints(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	store, cleanup := newMigratedStore(t, ctx)
	defer cleanup()

	owner, err := store.UpsertGitHubUser(ctx, controlplane.GitHubUser{
		GitHubID:    5001,
		GitHubLogin: "credential-owner",
	})
	if err != nil {
		t.Fatalf("upsert owner: %v", err)
	}
	other, err := store.UpsertGitHubUser(ctx, controlplane.GitHubUser{
		GitHubID:    5002,
		GitHubLogin: "credential-other",
	})
	if err != nil {
		t.Fatalf("upsert other: %v", err)
	}
	project, err := store.CreateProject(ctx, projectdomain.CreateParams{
		OwnerUserID: owner.ID,
		Name:        "Credential Gateway",
		Slug:        "credential-gateway",
	})
	if err != nil {
		t.Fatalf("create owner project: %v", err)
	}
	otherProject, err := store.CreateProject(ctx, projectdomain.CreateParams{
		OwnerUserID: other.ID,
		Name:        "Other Credential Gateway",
		Slug:        "other-credential-gateway",
	})
	if err != nil {
		t.Fatalf("create other project: %v", err)
	}

	missingProjectID := formatUUID(newTestUUID(t))
	for _, test := range []struct {
		name        string
		ownerUserID string
		projectID   string
	}{
		{name: "missing project", ownerUserID: owner.ID, projectID: missingProjectID},
		{name: "cross-owner project", ownerUserID: other.ID, projectID: project.ID},
	} {
		t.Run("create credential under "+test.name, func(t *testing.T) {
			_, err := store.CreateCredential(ctx, credential.CreateParams{
				ID:               formatUUID(newTestUUID(t)),
				OwnerUserID:      test.ownerUserID,
				ProjectID:        test.projectID,
				Provider:         credential.ProviderOpenAI,
				Label:            "not-created",
				SecretCiphertext: []byte("ciphertext"),
				SecretNonce:      bytes.Repeat([]byte{1}, 12),
				KeyVersion:       1,
			})
			if !errors.Is(err, credential.ErrNotFound) {
				t.Fatalf("create credential error = %v, want ErrNotFound", err)
			}
		})
	}
	if credentials, err := store.ListCredentials(ctx, owner.ID, missingProjectID); !errors.Is(err, credential.ErrNotFound) || credentials != nil {
		t.Fatalf("missing-project list = (%+v, %v), want nil, ErrNotFound", credentials, err)
	}

	created, err := store.CreateCredential(ctx, credential.CreateParams{
		ID:               formatUUID(newTestUUID(t)),
		OwnerUserID:      owner.ID,
		ProjectID:        project.ID,
		Provider:         credential.ProviderOpenAI,
		Label:            "prod",
		SecretCiphertext: []byte("ciphertext-v1"),
		SecretNonce:      bytes.Repeat([]byte{2}, 12),
		KeyVersion:       1,
	})
	if err != nil {
		t.Fatalf("create credential: %v", err)
	}
	if created.ProjectID != project.ID || created.Provider != credential.ProviderOpenAI || created.Label != "prod" || created.Status != credential.StatusActive {
		t.Fatalf("created credential metadata = %+v", created)
	}

	credentialID, err := parseUUID(created.ID)
	if err != nil {
		t.Fatalf("parse created credential ID: %v", err)
	}
	var persistedCiphertext []byte
	var persistedNonce []byte
	var persistedKeyVersion int16
	if err := store.pool.QueryRow(ctx, "SELECT secret_ciphertext, secret_nonce, key_version FROM provider_credentials WHERE id = $1", credentialID).Scan(&persistedCiphertext, &persistedNonce, &persistedKeyVersion); err != nil {
		t.Fatalf("read persisted credential secret envelope: %v", err)
	}
	if !bytes.Equal(persistedCiphertext, []byte("ciphertext-v1")) || !bytes.Equal(persistedNonce, bytes.Repeat([]byte{2}, 12)) || persistedKeyVersion != 1 {
		t.Fatalf("persisted envelope mismatch: ciphertext=%q nonce_len=%d version=%d", persistedCiphertext, len(persistedNonce), persistedKeyVersion)
	}

	_, err = store.CreateCredential(ctx, credential.CreateParams{
		ID:               formatUUID(newTestUUID(t)),
		OwnerUserID:      owner.ID,
		ProjectID:        project.ID,
		Provider:         "invalid",
		Label:            "bad-provider",
		SecretCiphertext: []byte("ciphertext"),
		SecretNonce:      bytes.Repeat([]byte{3}, 12),
		KeyVersion:       1,
	})
	assertPgCode(t, err, "23514")

	_, err = store.CreateCredential(ctx, credential.CreateParams{
		ID:               formatUUID(newTestUUID(t)),
		OwnerUserID:      owner.ID,
		ProjectID:        project.ID,
		Provider:         credential.ProviderAnthropic,
		Label:            "bad-nonce",
		SecretCiphertext: []byte("ciphertext"),
		SecretNonce:      bytes.Repeat([]byte{4}, 11),
		KeyVersion:       1,
	})
	assertPgCode(t, err, "23514")

	otherCredential, err := store.CreateCredential(ctx, credential.CreateParams{
		ID:               formatUUID(newTestUUID(t)),
		OwnerUserID:      other.ID,
		ProjectID:        otherProject.ID,
		Provider:         credential.ProviderDeepSeek,
		Label:            "other-prod",
		SecretCiphertext: []byte("other-ciphertext"),
		SecretNonce:      bytes.Repeat([]byte{5}, 12),
		KeyVersion:       1,
	})
	if err != nil {
		t.Fatalf("create other credential: %v", err)
	}

	credentials, err := store.ListCredentials(ctx, owner.ID, project.ID)
	if err != nil {
		t.Fatalf("list owner credentials: %v", err)
	}
	if len(credentials) != 1 || credentials[0].ID != created.ID {
		t.Fatalf("owner list = %+v, want only created credential", credentials)
	}
	rows, err := store.queries.ListProviderCredentialsForOwner(ctx, ListProviderCredentialsForOwnerParams{
		ProjectID:   credentialIDParamsProject(t, project.ID),
		OwnerUserID: credentialIDParamsOwner(t, owner.ID),
	})
	if err != nil {
		t.Fatalf("list metadata rows: %v", err)
	}
	rowJSON := fmt.Sprintf("%+v", rows[0])
	if strings.Contains(rowJSON, "ciphertext-v1") || strings.Contains(rowJSON, "SecretNonce") {
		t.Fatalf("metadata list row exposed secret envelope: %s", rowJSON)
	}

	if credentials, err := store.ListCredentials(ctx, other.ID, project.ID); !errors.Is(err, credential.ErrNotFound) || credentials != nil {
		t.Fatalf("cross-owner list = (%+v, %v), want nil, ErrNotFound", credentials, err)
	}
	if _, err := store.RotateCredential(ctx, credential.RotateParams{
		OwnerUserID:      other.ID,
		ProjectID:        project.ID,
		CredentialID:     created.ID,
		SecretCiphertext: []byte("attacker-ciphertext"),
		SecretNonce:      bytes.Repeat([]byte{6}, 12),
		KeyVersion:       1,
	}); !errors.Is(err, credential.ErrNotFound) {
		t.Fatalf("cross-owner rotate error = %v, want ErrNotFound", err)
	}
	if err := store.pool.QueryRow(ctx, "SELECT secret_ciphertext FROM provider_credentials WHERE id = $1", credentialID).Scan(&persistedCiphertext); err != nil {
		t.Fatalf("read credential after cross-owner rotate: %v", err)
	}
	if !bytes.Equal(persistedCiphertext, []byte("ciphertext-v1")) {
		t.Fatalf("cross-owner rotate changed ciphertext to %q", persistedCiphertext)
	}

	rotated, err := store.RotateCredential(ctx, credential.RotateParams{
		OwnerUserID:      owner.ID,
		ProjectID:        project.ID,
		CredentialID:     created.ID,
		SecretCiphertext: []byte("ciphertext-v2"),
		SecretNonce:      bytes.Repeat([]byte{7}, 12),
		KeyVersion:       1,
	})
	if err != nil {
		t.Fatalf("rotate credential: %v", err)
	}
	if rotated.RotatedAt == nil || rotated.Status != credential.StatusActive {
		t.Fatalf("rotated credential = %+v, want active with rotated_at", rotated)
	}
	if err := store.pool.QueryRow(ctx, "SELECT secret_ciphertext, secret_nonce FROM provider_credentials WHERE id = $1", credentialID).Scan(&persistedCiphertext, &persistedNonce); err != nil {
		t.Fatalf("read rotated credential envelope: %v", err)
	}
	if !bytes.Equal(persistedCiphertext, []byte("ciphertext-v2")) || !bytes.Equal(persistedNonce, bytes.Repeat([]byte{7}, 12)) {
		t.Fatalf("rotated envelope mismatch: ciphertext=%q nonce=%v", persistedCiphertext, persistedNonce)
	}

	if _, err := store.DisableCredential(ctx, owner.ID, otherProject.ID, otherCredential.ID); !errors.Is(err, credential.ErrNotFound) {
		t.Fatalf("wrong-project disable error = %v, want ErrNotFound", err)
	}
	if _, err := store.DisableCredential(ctx, other.ID, project.ID, created.ID); !errors.Is(err, credential.ErrNotFound) {
		t.Fatalf("cross-owner disable error = %v, want ErrNotFound", err)
	}
	disabled, err := store.DisableCredential(ctx, owner.ID, project.ID, created.ID)
	if err != nil {
		t.Fatalf("disable credential: %v", err)
	}
	if disabled.Status != credential.StatusDisabled {
		t.Fatalf("disabled status = %q, want disabled", disabled.Status)
	}
	disabledAgain, err := store.DisableCredential(ctx, owner.ID, project.ID, created.ID)
	if err != nil {
		t.Fatalf("disable credential again: %v", err)
	}
	if disabledAgain.Status != credential.StatusDisabled {
		t.Fatalf("second disable status = %q, want disabled", disabledAgain.Status)
	}
}

func TestVirtualAPIKeyStoreAdapterAndConstraints(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	store, cleanup := newMigratedStore(t, ctx)
	defer cleanup()

	owner, err := store.UpsertGitHubUser(ctx, controlplane.GitHubUser{
		GitHubID:    4001,
		GitHubLogin: "key-owner",
	})
	if err != nil {
		t.Fatalf("upsert owner: %v", err)
	}
	other, err := store.UpsertGitHubUser(ctx, controlplane.GitHubUser{
		GitHubID:    4002,
		GitHubLogin: "other-owner",
	})
	if err != nil {
		t.Fatalf("upsert other: %v", err)
	}

	project, err := store.CreateProject(ctx, projectdomain.CreateParams{
		OwnerUserID: owner.ID,
		Name:        "Gateway",
		Slug:        "gateway-keys",
	})
	if err != nil {
		t.Fatalf("create owner project: %v", err)
	}
	otherProject, err := store.CreateProject(ctx, projectdomain.CreateParams{
		OwnerUserID: other.ID,
		Name:        "Other Gateway",
		Slug:        "other-gateway-keys",
	})
	if err != nil {
		t.Fatalf("create other project: %v", err)
	}
	missingProjectID := formatUUID(newTestUUID(t))
	for _, test := range []struct {
		name        string
		ownerUserID string
		projectID   string
	}{
		{name: "missing project", ownerUserID: owner.ID, projectID: missingProjectID},
		{name: "cross-owner project", ownerUserID: other.ID, projectID: project.ID},
	} {
		t.Run("create key under "+test.name, func(t *testing.T) {
			_, err := store.CreateKey(ctx, apikey.CreateParams{
				OwnerUserID: test.ownerUserID,
				ProjectID:   test.projectID,
				Name:        "not-created",
				Prefix:      "notfound",
				KeyHash:     bytes.Repeat([]byte{8}, 32),
			})
			if !errors.Is(err, apikey.ErrNotFound) {
				t.Fatalf("create key error = %v, want ErrNotFound", err)
			}
		})
	}
	if keys, err := store.ListKeys(ctx, owner.ID, missingProjectID); !errors.Is(err, apikey.ErrNotFound) || keys != nil {
		t.Fatalf("missing-project list = (%+v, %v), want nil, ErrNotFound", keys, err)
	}

	keyHash := bytes.Repeat([]byte{1}, 32)
	created, err := store.CreateKey(ctx, apikey.CreateParams{
		OwnerUserID: owner.ID,
		ProjectID:   project.ID,
		Name:        "local-dev",
		Prefix:      "abcdefgh",
		KeyHash:     keyHash,
	})
	if err != nil {
		t.Fatalf("create key: %v", err)
	}
	if created.ProjectID != project.ID || created.Name != "local-dev" || created.Prefix != "abcdefgh" || created.Status != apikey.StatusActive {
		t.Fatalf("created key metadata = %+v", created)
	}

	keyID, err := parseUUID(created.ID)
	if err != nil {
		t.Fatalf("parse created key ID: %v", err)
	}
	projectID, err := parseUUID(project.ID)
	if err != nil {
		t.Fatalf("parse project ID: %v", err)
	}
	var persistedHash []byte
	var persistedPrefix string
	if err := store.pool.QueryRow(ctx, "SELECT key_hash, key_prefix FROM virtual_api_keys WHERE id = $1", keyID).Scan(&persistedHash, &persistedPrefix); err != nil {
		t.Fatalf("read persisted key material: %v", err)
	}
	if !bytes.Equal(persistedHash, keyHash) || len(persistedHash) != 32 {
		t.Fatalf("persisted digest length/equality mismatch: len=%d", len(persistedHash))
	}
	if persistedPrefix != "abcdefgh" {
		t.Fatalf("persisted prefix = %q, want abcdefgh", persistedPrefix)
	}

	_, err = store.CreateKey(ctx, apikey.CreateParams{
		OwnerUserID: owner.ID,
		ProjectID:   project.ID,
		Name:        "short-hash",
		Prefix:      "shortbad",
		KeyHash:     bytes.Repeat([]byte{2}, 31),
	})
	assertPgCode(t, err, "23514")

	_, err = store.CreateKey(ctx, apikey.CreateParams{
		OwnerUserID: other.ID,
		ProjectID:   otherProject.ID,
		Name:        "duplicate-hash",
		Prefix:      "duphash1",
		KeyHash:     keyHash,
	})
	assertPgCode(t, err, "23505")

	otherKey, err := store.CreateKey(ctx, apikey.CreateParams{
		OwnerUserID: other.ID,
		ProjectID:   otherProject.ID,
		Name:        "other-dev",
		Prefix:      "ijklmnop",
		KeyHash:     bytes.Repeat([]byte{3}, 32),
	})
	if err != nil {
		t.Fatalf("create other key: %v", err)
	}

	keys, err := store.ListKeys(ctx, owner.ID, project.ID)
	if err != nil {
		t.Fatalf("list owner keys: %v", err)
	}
	if len(keys) != 1 || keys[0].ID != created.ID {
		t.Fatalf("owner list = %+v, want only created key", keys)
	}
	keys, err = store.ListKeys(ctx, other.ID, project.ID)
	if !errors.Is(err, apikey.ErrNotFound) {
		t.Fatalf("cross-owner list error = %v, want ErrNotFound", err)
	}
	if keys != nil {
		t.Fatalf("cross-owner list returned keys: %+v", keys)
	}

	pepper := bytes.Repeat([]byte{9}, 32)
	keyService, err := apikey.NewService(store, pepper)
	if err != nil {
		t.Fatalf("create key service: %v", err)
	}
	shownOnce, err := keyService.Create(ctx, owner.ID, project.ID, "shown-once")
	if err != nil {
		t.Fatalf("create shown-once key: %v", err)
	}
	shownOnceID, err := parseUUID(shownOnce.Key.ID)
	if err != nil {
		t.Fatalf("parse shown-once key ID: %v", err)
	}
	expectedDigest, err := sharedapikey.HashKey(shownOnce.RawKey, pepper)
	if err != nil {
		t.Fatalf("hash shown-once key: %v", err)
	}
	var storedDigest []byte
	var storedRow string
	if err := store.pool.QueryRow(ctx, "SELECT key_hash, row_to_json(keys)::text FROM virtual_api_keys AS keys WHERE id = $1", shownOnceID).Scan(&storedDigest, &storedRow); err != nil {
		t.Fatalf("read shown-once key row: %v", err)
	}
	if !bytes.Equal(storedDigest, expectedDigest) || len(storedDigest) != 32 {
		t.Fatalf("shown-once key persisted an invalid digest length: %d", len(storedDigest))
	}
	if strings.Contains(storedRow, shownOnce.RawKey) {
		t.Fatal("database row contains the raw virtual API key")
	}

	if _, err := store.DisableKey(ctx, other.ID, project.ID, created.ID); !errors.Is(err, apikey.ErrNotFound) {
		t.Fatalf("cross-owner disable error = %v, want ErrNotFound", err)
	}
	current := readVirtualAPIKey(t, ctx, store, keyID)
	if current.Status != string(apikey.StatusActive) {
		t.Fatalf("cross-owner disable changed status to %q", current.Status)
	}

	disabled, err := store.DisableKey(ctx, owner.ID, project.ID, created.ID)
	if err != nil {
		t.Fatalf("disable key: %v", err)
	}
	if disabled.Status != apikey.StatusDisabled {
		t.Fatalf("disabled status = %q, want disabled", disabled.Status)
	}
	disabledAgain, err := store.DisableKey(ctx, owner.ID, project.ID, created.ID)
	if err != nil {
		t.Fatalf("disable key again: %v", err)
	}
	if disabledAgain.Status != apikey.StatusDisabled {
		t.Fatalf("second disable status = %q, want disabled", disabledAgain.Status)
	}

	if _, err := store.RevokeKey(ctx, owner.ID, otherProject.ID, otherKey.ID); !errors.Is(err, apikey.ErrNotFound) {
		t.Fatalf("wrong-project revoke error = %v, want ErrNotFound", err)
	}
	if _, err := store.RevokeKey(ctx, other.ID, project.ID, created.ID); !errors.Is(err, apikey.ErrNotFound) {
		t.Fatalf("cross-owner revoke error = %v, want ErrNotFound", err)
	}

	revoked, err := store.RevokeKey(ctx, owner.ID, project.ID, created.ID)
	if err != nil {
		t.Fatalf("revoke key: %v", err)
	}
	if revoked.Status != apikey.StatusRevoked || revoked.RevokedAt == nil {
		t.Fatalf("revoked key = %+v, want revoked status and timestamp", revoked)
	}
	firstRevokedAt := *revoked.RevokedAt
	revokedAgain, err := store.RevokeKey(ctx, owner.ID, project.ID, created.ID)
	if err != nil {
		t.Fatalf("revoke key again: %v", err)
	}
	if revokedAgain.Status != apikey.StatusRevoked || revokedAgain.RevokedAt == nil || !revokedAgain.RevokedAt.Equal(firstRevokedAt) {
		t.Fatalf("second revoke key = %+v, want same revoked timestamp", revokedAgain)
	}
	stillRevoked, err := store.DisableKey(ctx, owner.ID, project.ID, created.ID)
	if err != nil {
		t.Fatalf("disable revoked key: %v", err)
	}
	if stillRevoked.Status != apikey.StatusRevoked {
		t.Fatalf("disable changed revoked status to %q", stillRevoked.Status)
	}

	if _, err := store.pool.Exec(ctx, "DELETE FROM projects WHERE id = $1", projectID); err != nil {
		t.Fatalf("delete project: %v", err)
	}
	var remaining int
	if err := store.pool.QueryRow(ctx, "SELECT count(*) FROM virtual_api_keys WHERE project_id = $1", projectID).Scan(&remaining); err != nil {
		t.Fatalf("count project keys after project delete: %v", err)
	}
	if remaining != 0 {
		t.Fatalf("remaining keys after project delete = %d, want 0", remaining)
	}
}

func TestProjectOwnershipAndSessionLookupQueries(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	store, cleanup := newMigratedStore(t, ctx)
	defer cleanup()

	queries := store.queries
	ownerID := newTestUUID(t)
	otherID := newTestUUID(t)

	owner := insertUser(t, ctx, queries, ownerID, 2001, "owner")
	insertUser(t, ctx, queries, otherID, 2002, "other")

	project, err := queries.CreateProject(ctx, CreateProjectParams{
		ID:          newTestUUID(t),
		OwnerUserID: owner.ID,
		Name:        "Control Plane",
		Slug:        "control-plane",
	})
	if err != nil {
		t.Fatalf("create project: %v", err)
	}

	got, err := queries.GetProjectForOwner(ctx, GetProjectForOwnerParams{
		ID:          project.ID,
		OwnerUserID: owner.ID,
	})
	if err != nil {
		t.Fatalf("get project for owner: %v", err)
	}
	if got.Slug != "control-plane" {
		t.Fatalf("project slug = %q, want control-plane", got.Slug)
	}

	_, err = queries.GetProjectForOwner(ctx, GetProjectForOwnerParams{
		ID:          project.ID,
		OwnerUserID: otherID,
	})
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("cross-owner project lookup error = %v, want pgx.ErrNoRows", err)
	}

	tokenHash := make([]byte, 32)
	if _, err := rand.Read(tokenHash); err != nil {
		t.Fatalf("generate token hash: %v", err)
	}
	err = queries.CreateWebSession(ctx, CreateWebSessionParams{
		ID:        newTestUUID(t),
		UserID:    owner.ID,
		TokenHash: tokenHash,
		ExpiresAt: pgtype.Timestamptz{
			Time:  time.Now().Add(time.Hour),
			Valid: true,
		},
		CreatedAt: pgtype.Timestamptz{Time: time.Now(), Valid: true},
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	sessionWithUser, err := queries.GetValidWebSessionByTokenHash(ctx, GetValidWebSessionByTokenHashParams{
		TokenHash: tokenHash,
		ExpiresAt: pgtype.Timestamptz{Time: time.Now(), Valid: true},
	})
	if err != nil {
		t.Fatalf("get valid session: %v", err)
	}
	if sessionWithUser.UserID != owner.ID {
		t.Fatalf("session lookup returned wrong session/user")
	}

	missingHash := make([]byte, 32)
	if _, err := rand.Read(missingHash); err != nil {
		t.Fatalf("generate missing hash: %v", err)
	}
	_, err = queries.GetValidWebSessionByTokenHash(ctx, GetValidWebSessionByTokenHashParams{
		TokenHash: missingHash,
		ExpiresAt: pgtype.Timestamptz{Time: time.Now(), Valid: true},
	})
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("missing session error = %v, want pgx.ErrNoRows", err)
	}
}

func TestControlPlaneStoreAdapters(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	store, cleanup := newMigratedStore(t, ctx)
	defer cleanup()

	owner, err := store.UpsertGitHubUser(ctx, controlplane.GitHubUser{
		GitHubID:    3001,
		GitHubLogin: "owner",
	})
	if err != nil {
		t.Fatalf("upsert owner: %v", err)
	}
	other, err := store.UpsertGitHubUser(ctx, controlplane.GitHubUser{
		GitHubID:    3002,
		GitHubLogin: "other",
	})
	if err != nil {
		t.Fatalf("upsert other: %v", err)
	}

	now := time.Now().UTC()
	tokenHash := make([]byte, 32)
	if _, err := rand.Read(tokenHash); err != nil {
		t.Fatalf("generate token hash: %v", err)
	}
	if err := store.CreateSession(ctx, controlplane.NewSession{
		UserID:    owner.ID,
		TokenHash: tokenHash,
		ExpiresAt: now.Add(time.Hour),
		Now:       now,
	}); err != nil {
		t.Fatalf("create session: %v", err)
	}
	session, err := store.GetSession(ctx, tokenHash, now)
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	if session.User.ID != owner.ID || session.User.GitHubLogin != owner.GitHubLogin {
		t.Fatalf("session user = %+v, want owner %+v", session.User, owner)
	}
	if err := store.DeleteSession(ctx, tokenHash); err != nil {
		t.Fatalf("delete session: %v", err)
	}
	if _, err := store.GetSession(ctx, tokenHash, now); !errors.Is(err, controlplane.ErrNotFound) {
		t.Fatalf("deleted session error = %v, want ErrNotFound", err)
	}

	created, err := store.CreateProject(ctx, projectdomain.CreateParams{
		OwnerUserID: owner.ID,
		Name:        "Gateway",
		Slug:        "gateway",
	})
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	newName := "Gateway Control Plane"
	newStatus := projectdomain.StatusDisabled
	updated, err := store.UpdateProject(ctx, owner.ID, created.ID, projectdomain.UpdateParams{
		Name:   &newName,
		Status: &newStatus,
	})
	if err != nil {
		t.Fatalf("update project: %v", err)
	}
	if updated.Name != newName || updated.Status != projectdomain.StatusDisabled || updated.Slug != created.Slug {
		t.Fatalf("updated project = %+v", updated)
	}
	if _, err := store.GetProject(ctx, other.ID, created.ID); !errors.Is(err, projectdomain.ErrNotFound) {
		t.Fatalf("cross-owner lookup error = %v, want ErrNotFound", err)
	}
	if _, err := store.CreateProject(ctx, projectdomain.CreateParams{
		OwnerUserID: owner.ID,
		Name:        "Duplicate",
		Slug:        "gateway",
	}); !errors.Is(err, projectdomain.ErrConflict) {
		t.Fatalf("duplicate slug error = %v, want ErrConflict", err)
	}
	second, err := store.CreateProject(ctx, projectdomain.CreateParams{
		OwnerUserID: owner.ID,
		Name:        "Second Project",
		Slug:        "second-project",
	})
	if err != nil {
		t.Fatalf("create second project: %v", err)
	}
	duplicateSlug := created.Slug
	if _, err := store.UpdateProject(ctx, owner.ID, second.ID, projectdomain.UpdateParams{
		Slug: &duplicateSlug,
	}); !errors.Is(err, projectdomain.ErrConflict) {
		t.Fatalf("update duplicate slug error = %v, want ErrConflict", err)
	}
}

func newMigratedStore(t *testing.T, ctx context.Context) (*Store, func()) {
	t.Helper()

	pool, _, cleanupPool := newIsolatedPool(t, ctx)
	applyMigration(t, ctx, pool, "000001_control_plane_foundation.up.sql")
	applyMigration(t, ctx, pool, "000002_virtual_api_keys.up.sql")
	applyMigration(t, ctx, pool, "000003_provider_credentials.up.sql")

	store := &Store{
		pool:    pool,
		queries: New(pool),
	}

	return store, func() {
		store.Close()
		cleanupPool()
	}
}

func credentialIDParamsProject(t *testing.T, projectID string) pgtype.UUID {
	t.Helper()
	id, err := parseUUID(projectID)
	if err != nil {
		t.Fatalf("parse project ID: %v", err)
	}
	return id
}

func credentialIDParamsOwner(t *testing.T, ownerID string) pgtype.UUID {
	t.Helper()
	id, err := parseUUID(ownerID)
	if err != nil {
		t.Fatalf("parse owner ID: %v", err)
	}
	return id
}

func readVirtualAPIKey(t *testing.T, ctx context.Context, store *Store, id pgtype.UUID) VirtualApiKey {
	t.Helper()

	row, err := store.pool.Query(ctx, "SELECT id, project_id, name, key_prefix, key_hash, status, created_at, last_used_at, revoked_at FROM virtual_api_keys WHERE id = $1", id)
	if err != nil {
		t.Fatalf("query virtual key: %v", err)
	}
	defer row.Close()
	if !row.Next() {
		t.Fatal("virtual key was not found")
	}
	var key VirtualApiKey
	if err := row.Scan(&key.ID, &key.ProjectID, &key.Name, &key.KeyPrefix, &key.KeyHash, &key.Status, &key.CreatedAt, &key.LastUsedAt, &key.RevokedAt); err != nil {
		t.Fatalf("scan virtual key: %v", err)
	}
	if err := row.Err(); err != nil {
		t.Fatalf("read virtual key rows: %v", err)
	}
	return key
}

func newIsolatedPool(t *testing.T, ctx context.Context) (*pgxpool.Pool, string, func()) {
	t.Helper()

	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Fatal("DATABASE_URL is required for integration tests")
	}

	adminPool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatalf("connect admin pool: %v", err)
	}
	defer adminPool.Close()

	schemaName := fmt.Sprintf("test_%s_%d", strings.ToLower(sanitizeIdentifierPart(t.Name())), time.Now().UnixNano())
	schemaIdent := pgx.Identifier{schemaName}.Sanitize()
	if _, err := adminPool.Exec(ctx, "CREATE SCHEMA "+schemaIdent); err != nil {
		t.Fatalf("create schema: %v", err)
	}

	config, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		t.Fatalf("parse database url: %v", err)
	}
	config.ConnConfig.RuntimeParams["search_path"] = schemaName
	config.ConnConfig.DefaultQueryExecMode = pgx.QueryExecModeSimpleProtocol

	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatalf("connect isolated pool: %v", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		t.Fatalf("ping isolated pool: %v", err)
	}

	cleanup := func() {
		pool.Close()

		cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		cleanupPool, err := pgxpool.New(cleanupCtx, databaseURL)
		if err != nil {
			t.Logf("connect cleanup pool: %v", err)
			return
		}
		defer cleanupPool.Close()

		if _, err := cleanupPool.Exec(cleanupCtx, "DROP SCHEMA "+schemaIdent+" CASCADE"); err != nil {
			t.Logf("drop schema %s: %v", schemaName, err)
		}
	}

	return pool, schemaName, cleanup
}

func applyMigration(t *testing.T, ctx context.Context, pool *pgxpool.Pool, name string) {
	t.Helper()

	sql, err := os.ReadFile(filepath.Join(repoRoot(t), "db", "migrations", name))
	if err != nil {
		t.Fatalf("read migration %s: %v", name, err)
	}

	if _, err := pool.Exec(ctx, string(sql)); err != nil {
		t.Fatalf("apply migration %s: %v", name, err)
	}
}

func tableExists(t *testing.T, ctx context.Context, pool *pgxpool.Pool, table string) bool {
	t.Helper()

	var exists bool
	err := pool.QueryRow(ctx, "SELECT to_regclass($1) IS NOT NULL", table).Scan(&exists)
	if err != nil {
		t.Fatalf("check table %s: %v", table, err)
	}
	return exists
}

func insertUser(t *testing.T, ctx context.Context, queries *Queries, id pgtype.UUID, githubID int64, login string) User {
	t.Helper()

	user, err := queries.UpsertUserFromGitHub(ctx, UpsertUserFromGitHubParams{
		ID:          id,
		GithubID:    githubID,
		GithubLogin: login,
	})
	if err != nil {
		t.Fatalf("insert user %s: %v", login, err)
	}
	return user
}

func newTestUUID(t *testing.T) pgtype.UUID {
	t.Helper()

	var id pgtype.UUID
	if _, err := rand.Read(id.Bytes[:]); err != nil {
		t.Fatalf("generate uuid: %v", err)
	}
	id.Bytes[6] = (id.Bytes[6] & 0x0f) | 0x40
	id.Bytes[8] = (id.Bytes[8] & 0x3f) | 0x80
	id.Valid = true
	return id
}

func assertPgCode(t *testing.T, err error, code string) {
	t.Helper()

	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		t.Fatalf("error = %v, want PostgreSQL code %s", err, code)
	}
	if pgErr.Code != code {
		t.Fatalf("PostgreSQL code = %s, want %s", pgErr.Code, code)
	}
}

func repoRoot(t *testing.T) string {
	t.Helper()

	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve caller")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", "..", ".."))
}

func sanitizeIdentifierPart(value string) string {
	var builder strings.Builder
	for _, char := range value {
		if char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' || char >= '0' && char <= '9' {
			builder.WriteRune(char)
			continue
		}
		builder.WriteByte('_')
	}
	return builder.String()
}
