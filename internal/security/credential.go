package security

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
)

const (
	CredentialKeyVersion = int16(1)
	credentialKeyBytes   = 32
	credentialNonceBytes = 12
)

var (
	ErrUnsupportedKeyVersion = errors.New("unsupported credential key version")
	ErrInvalidCiphertext     = errors.New("invalid credential ciphertext")
)

type CredentialCipher struct {
	aead cipher.AEAD
}

type EncryptedCredential struct {
	Ciphertext []byte
	Nonce      []byte
	KeyVersion int16
}

func NewCredentialCipher(masterKey []byte) (*CredentialCipher, error) {
	if len(masterKey) != credentialKeyBytes {
		return nil, fmt.Errorf("credential master key must be exactly %d bytes", credentialKeyBytes)
	}
	block, err := aes.NewCipher(masterKey)
	if err != nil {
		return nil, fmt.Errorf("create credential cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("create credential AEAD: %w", err)
	}
	return &CredentialCipher{aead: aead}, nil
}

func (c *CredentialCipher) Encrypt(plaintext []byte) (EncryptedCredential, error) {
	nonce := make([]byte, credentialNonceBytes)
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return EncryptedCredential{}, fmt.Errorf("generate credential nonce: %w", err)
	}
	ciphertext := c.aead.Seal(nil, nonce, plaintext, nil)
	return EncryptedCredential{
		Ciphertext: ciphertext,
		Nonce:      nonce,
		KeyVersion: CredentialKeyVersion,
	}, nil
}

func (c *CredentialCipher) Decrypt(encrypted EncryptedCredential) ([]byte, error) {
	if encrypted.KeyVersion != CredentialKeyVersion {
		return nil, ErrUnsupportedKeyVersion
	}
	if len(encrypted.Nonce) != credentialNonceBytes {
		return nil, ErrInvalidCiphertext
	}
	plaintext, err := c.aead.Open(nil, encrypted.Nonce, encrypted.Ciphertext, nil)
	if err != nil {
		return nil, ErrInvalidCiphertext
	}
	return plaintext, nil
}
