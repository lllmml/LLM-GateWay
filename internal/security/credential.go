package security

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"
)

const (
	CredentialKeyVersion = int16(1)
	credentialKeyBytes   = 32
	credentialNonceBytes = 12
	credentialAADDomain  = "pgw-provider-credential"
	credentialAADVersion = uint16(1)
)

var (
	ErrUnsupportedKeyVersion    = errors.New("unsupported credential key version")
	ErrInvalidCiphertext        = errors.New("invalid credential ciphertext")
	ErrInvalidCredentialContext = errors.New("invalid credential encryption context")
)

type CredentialCipher struct {
	aead cipher.AEAD
}

type EncryptedCredential struct {
	Ciphertext []byte
	Nonce      []byte
	KeyVersion int16
}

// CredentialIdentity is the immutable database identity bound to a provider
// credential envelope. Both Control Plane encryption and Data Plane decryption
// must construct it from the same persisted row.
type CredentialIdentity struct {
	CredentialID string
	ProjectID    string
	Provider     string
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

func (c *CredentialCipher) Encrypt(plaintext []byte, identity CredentialIdentity) (EncryptedCredential, error) {
	aad, err := credentialAdditionalData(identity, CredentialKeyVersion)
	if err != nil {
		return EncryptedCredential{}, err
	}
	nonce := make([]byte, credentialNonceBytes)
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return EncryptedCredential{}, fmt.Errorf("generate credential nonce: %w", err)
	}
	ciphertext := c.aead.Seal(nil, nonce, plaintext, aad)
	return EncryptedCredential{
		Ciphertext: ciphertext,
		Nonce:      nonce,
		KeyVersion: CredentialKeyVersion,
	}, nil
}

func (c *CredentialCipher) Decrypt(encrypted EncryptedCredential, identity CredentialIdentity) ([]byte, error) {
	if encrypted.KeyVersion != CredentialKeyVersion {
		return nil, ErrUnsupportedKeyVersion
	}
	if len(encrypted.Nonce) != credentialNonceBytes {
		return nil, ErrInvalidCiphertext
	}
	aad, err := credentialAdditionalData(identity, encrypted.KeyVersion)
	if err != nil {
		return nil, err
	}
	plaintext, err := c.aead.Open(nil, encrypted.Nonce, encrypted.Ciphertext, aad)
	if err != nil {
		return nil, ErrInvalidCiphertext
	}
	return plaintext, nil
}

func credentialAdditionalData(identity CredentialIdentity, keyVersion int16) ([]byte, error) {
	credentialID, ok := credentialUUIDBytes(identity.CredentialID)
	if !ok {
		return nil, ErrInvalidCredentialContext
	}
	projectID, ok := credentialUUIDBytes(identity.ProjectID)
	if !ok || identity.Provider == "" || len(identity.Provider) > int(^uint16(0)) || keyVersion <= 0 {
		return nil, ErrInvalidCredentialContext
	}

	aad := make([]byte, 0, len(credentialAADDomain)+2+16+16+2+len(identity.Provider)+2)
	aad = append(aad, credentialAADDomain...)
	aad = binary.BigEndian.AppendUint16(aad, credentialAADVersion)
	aad = append(aad, credentialID...)
	aad = append(aad, projectID...)
	aad = binary.BigEndian.AppendUint16(aad, uint16(len(identity.Provider)))
	aad = append(aad, identity.Provider...)
	aad = binary.BigEndian.AppendUint16(aad, uint16(keyVersion))
	return aad, nil
}

func credentialUUIDBytes(value string) ([]byte, bool) {
	if len(value) != 36 || value[8] != '-' || value[13] != '-' || value[18] != '-' || value[23] != '-' {
		return nil, false
	}
	decoded, err := hex.DecodeString(strings.ReplaceAll(value, "-", ""))
	if err != nil || len(decoded) != 16 {
		return nil, false
	}
	return decoded, true
}
