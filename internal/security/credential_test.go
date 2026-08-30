package security

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

func TestCredentialCipherEncryptDecrypt(t *testing.T) {
	cipher := newTestCipher(t, bytes.Repeat([]byte{1}, 32))

	encrypted, err := cipher.Encrypt([]byte("sk-test-secret"))
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

	plaintext, err := cipher.Decrypt(encrypted)
	if err != nil {
		t.Fatalf("decrypt: %v", err)
	}
	if !bytes.Equal(plaintext, []byte("sk-test-secret")) {
		t.Fatalf("plaintext = %q, want original secret", plaintext)
	}
}

func TestCredentialCipherUsesFreshNonce(t *testing.T) {
	cipher := newTestCipher(t, bytes.Repeat([]byte{2}, 32))

	first, err := cipher.Encrypt([]byte("same-secret"))
	if err != nil {
		t.Fatalf("first encrypt: %v", err)
	}
	second, err := cipher.Encrypt([]byte("same-secret"))
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
	encrypted, err := cipher.Encrypt([]byte("sk-test-secret"))
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}

	tampered := encrypted
	tampered.Ciphertext = append([]byte(nil), encrypted.Ciphertext...)
	tampered.Ciphertext[0] ^= 0xff
	if _, err := cipher.Decrypt(tampered); !errors.Is(err, ErrInvalidCiphertext) {
		t.Fatalf("tamper decrypt error = %v, want ErrInvalidCiphertext", err)
	}

	wrongKey := newTestCipher(t, bytes.Repeat([]byte{4}, 32))
	if _, err := wrongKey.Decrypt(encrypted); !errors.Is(err, ErrInvalidCiphertext) {
		t.Fatalf("wrong-key decrypt error = %v, want ErrInvalidCiphertext", err)
	}

	wrongVersion := encrypted
	wrongVersion.KeyVersion = CredentialKeyVersion + 1
	if _, err := cipher.Decrypt(wrongVersion); !errors.Is(err, ErrUnsupportedKeyVersion) {
		t.Fatalf("wrong-version decrypt error = %v, want ErrUnsupportedKeyVersion", err)
	}
}

func TestCredentialCipherDoesNotLeakSecretInErrors(t *testing.T) {
	cipher := newTestCipher(t, bytes.Repeat([]byte{5}, 32))
	secret := "sk-secret-that-must-not-leak"
	encrypted, err := cipher.Encrypt([]byte(secret))
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	encrypted.Nonce = encrypted.Nonce[:len(encrypted.Nonce)-1]

	_, err = cipher.Decrypt(encrypted)
	if err == nil {
		t.Fatal("decrypt succeeded with invalid nonce")
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("error leaked secret: %v", err)
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
