package security

import (
	"bytes"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
)

var testCredentialIdentity = CredentialIdentity{
	CredentialID: "11111111-1111-4111-8111-111111111111",
	ProjectID:    "22222222-2222-4222-8222-222222222222",
	Provider:     "openai",
}

func TestCredentialCipherEncryptDecrypt(t *testing.T) {
	cipher := newTestCipher(t, bytes.Repeat([]byte{1}, 32))

	encrypted, err := cipher.Encrypt([]byte("sk-test-secret"), testCredentialIdentity)
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	if encrypted.KeyVersion != CredentialKeyVersion {
		t.Fatalf("key version = %d, want %d", encrypted.KeyVersion, CredentialKeyVersion)
	}
	if len(encrypted.Nonce) != credentialNonceBytes {
		t.Fatalf("nonce length = %d, want %d", len(encrypted.Nonce), credentialNonceBytes)
	}
	if bytes.Contains(encrypted.Ciphertext, []byte("sk-test-secret")) {
		t.Fatal("ciphertext contains plaintext")
	}

	plaintext, err := cipher.Decrypt(encrypted, testCredentialIdentity)
	if err != nil {
		t.Fatalf("decrypt: %v", err)
	}
	if !bytes.Equal(plaintext, []byte("sk-test-secret")) {
		t.Fatalf("plaintext = %q, want original secret", plaintext)
	}
}

func TestCredentialCipherUsesFreshNonce(t *testing.T) {
	cipher := newTestCipher(t, bytes.Repeat([]byte{2}, 32))

	first, err := cipher.Encrypt([]byte("same-secret"), testCredentialIdentity)
	if err != nil {
		t.Fatalf("first encrypt: %v", err)
	}
	second, err := cipher.Encrypt([]byte("same-secret"), testCredentialIdentity)
	if err != nil {
		t.Fatalf("second encrypt: %v", err)
	}
	if bytes.Equal(first.Nonce, second.Nonce) {
		t.Fatal("two encryptions reused the same nonce")
	}
	if bytes.Equal(first.Ciphertext, second.Ciphertext) {
		t.Fatal("two encryptions produced identical ciphertext")
	}
}

func TestCredentialCipherRejectsInvalidMasterKey(t *testing.T) {
	_, err := NewCredentialCipher(bytes.Repeat([]byte{1}, 31))
	if err == nil || !strings.Contains(err.Error(), "exactly 32 bytes") {
		t.Fatalf("error = %v, want 32-byte validation", err)
	}
}

func TestCredentialCipherRejectsTamperWrongKeyAndVersion(t *testing.T) {
	cipher := newTestCipher(t, bytes.Repeat([]byte{3}, 32))
	encrypted, err := cipher.Encrypt([]byte("sk-test-secret"), testCredentialIdentity)
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}

	tampered := encrypted
	tampered.Ciphertext = append([]byte(nil), encrypted.Ciphertext...)
	tampered.Ciphertext[0] ^= 0xff
	if _, err := cipher.Decrypt(tampered, testCredentialIdentity); !errors.Is(err, ErrInvalidCiphertext) {
		t.Fatalf("tamper decrypt error = %v, want ErrInvalidCiphertext", err)
	}

	wrongKey := newTestCipher(t, bytes.Repeat([]byte{4}, 32))
	if _, err := wrongKey.Decrypt(encrypted, testCredentialIdentity); !errors.Is(err, ErrInvalidCiphertext) {
		t.Fatalf("wrong-key decrypt error = %v, want ErrInvalidCiphertext", err)
	}

	wrongVersion := encrypted
	wrongVersion.KeyVersion = CredentialKeyVersion + 1
	if _, err := cipher.Decrypt(wrongVersion, testCredentialIdentity); !errors.Is(err, ErrUnsupportedKeyVersion) {
		t.Fatalf("wrong-version decrypt error = %v, want ErrUnsupportedKeyVersion", err)
	}
}

func TestCredentialCipherRejectsNonceAndIdentityChanges(t *testing.T) {
	cipher := newTestCipher(t, bytes.Repeat([]byte{6}, 32))
	encrypted, err := cipher.Encrypt([]byte("sk-test-secret"), testCredentialIdentity)
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}

	tests := []struct {
		name      string
		encrypted EncryptedCredential
		identity  CredentialIdentity
	}{
		{
			name: "nonce",
			encrypted: EncryptedCredential{
				Ciphertext: append([]byte(nil), encrypted.Ciphertext...),
				Nonce:      append([]byte(nil), encrypted.Nonce...),
				KeyVersion: encrypted.KeyVersion,
			},
			identity: testCredentialIdentity,
		},
		{name: "credential ID", encrypted: encrypted, identity: CredentialIdentity{CredentialID: "33333333-3333-4333-8333-333333333333", ProjectID: testCredentialIdentity.ProjectID, Provider: testCredentialIdentity.Provider}},
		{name: "project ID", encrypted: encrypted, identity: CredentialIdentity{CredentialID: testCredentialIdentity.CredentialID, ProjectID: "44444444-4444-4444-8444-444444444444", Provider: testCredentialIdentity.Provider}},
		{name: "provider", encrypted: encrypted, identity: CredentialIdentity{CredentialID: testCredentialIdentity.CredentialID, ProjectID: testCredentialIdentity.ProjectID, Provider: "anthropic"}},
	}
	tests[0].encrypted.Nonce[0] ^= 0xff

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := cipher.Decrypt(test.encrypted, test.identity); !errors.Is(err, ErrInvalidCiphertext) {
				t.Fatalf("decrypt error = %v, want ErrInvalidCiphertext", err)
			}
		})
	}
}

func TestCredentialCipherRejectsEnvelopeSubstitution(t *testing.T) {
	cipher := newTestCipher(t, bytes.Repeat([]byte{7}, 32))
	credentialA := testCredentialIdentity
	credentialB := CredentialIdentity{
		CredentialID: "55555555-5555-4555-8555-555555555555",
		ProjectID:    testCredentialIdentity.ProjectID,
		Provider:     testCredentialIdentity.Provider,
	}
	envelopeA, err := cipher.Encrypt([]byte("credential-a-secret"), credentialA)
	if err != nil {
		t.Fatalf("encrypt credential A: %v", err)
	}
	if _, err := cipher.Decrypt(envelopeA, credentialB); !errors.Is(err, ErrInvalidCiphertext) {
		t.Fatalf("decrypt substituted envelope error = %v, want ErrInvalidCiphertext", err)
	}
}

func TestCredentialAdditionalDataIsDeterministicAndVersioned(t *testing.T) {
	first, err := credentialAdditionalData(testCredentialIdentity, CredentialKeyVersion)
	if err != nil {
		t.Fatalf("first AAD: %v", err)
	}
	second, err := credentialAdditionalData(testCredentialIdentity, CredentialKeyVersion)
	if err != nil {
		t.Fatalf("second AAD: %v", err)
	}
	if !bytes.Equal(first, second) {
		t.Fatal("identical credential contexts produced different AAD")
	}
	otherVersion, err := credentialAdditionalData(testCredentialIdentity, CredentialKeyVersion+1)
	if err != nil {
		t.Fatalf("versioned AAD: %v", err)
	}
	if bytes.Equal(first, otherVersion) {
		t.Fatal("key version did not change AAD")
	}
}

func TestCredentialCipherDoesNotLeakSecretInErrors(t *testing.T) {
	cipher := newTestCipher(t, bytes.Repeat([]byte{5}, 32))
	secret := "sk-secret-that-must-not-leak"
	encrypted, err := cipher.Encrypt([]byte(secret), testCredentialIdentity)
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	encrypted.Nonce = encrypted.Nonce[:len(encrypted.Nonce)-1]

	_, err = cipher.Decrypt(encrypted, testCredentialIdentity)
	if err == nil {
		t.Fatal("decrypt succeeded with invalid nonce")
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("error leaked secret: %v", err)
	}
	for _, material := range []string{
		hex.EncodeToString(encrypted.Ciphertext),
		hex.EncodeToString(encrypted.Nonce),
		hex.EncodeToString(bytes.Repeat([]byte{5}, 32)),
	} {
		if strings.Contains(err.Error(), material) {
			t.Fatalf("error leaked cryptographic material: %v", err)
		}
	}
}

func newTestCipher(t *testing.T, key []byte) *CredentialCipher {
	t.Helper()
	cipher, err := NewCredentialCipher(key)
	if err != nil {
		t.Fatalf("NewCredentialCipher: %v", err)
	}
	return cipher
}
