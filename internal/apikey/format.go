package apikey

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"io"
	"strings"
)

const (
	rawKeyMarker      = "pgw_"
	prefixRandomSize  = 6
	secretRandomSize  = 32
	prefixEncodedSize = 8
	secretEncodedSize = 43
	rawKeySize        = len(rawKeyMarker) + prefixEncodedSize + 1 + secretEncodedSize
)

var ErrInvalidRawKey = errors.New("invalid virtual API key format")

var rawURLEncoding = base64.RawURLEncoding.Strict()

type ParsedKey struct {
	Prefix string
}

func GenerateRawKey() (string, string, error) {
	return generateRawKey(rand.Reader)
}

func ParseRawKey(rawKey string) (ParsedKey, error) {
	if len(rawKey) != rawKeySize || !strings.HasPrefix(rawKey, rawKeyMarker) {
		return ParsedKey{}, ErrInvalidRawKey
	}

	prefixStart := len(rawKeyMarker)
	separator := prefixStart + prefixEncodedSize
	if rawKey[separator] != '_' {
		return ParsedKey{}, ErrInvalidRawKey
	}
	prefix := rawKey[prefixStart:separator]
	secret := rawKey[separator+1:]
	if !isCanonicalComponent(prefix, prefixRandomSize) || !isCanonicalComponent(secret, secretRandomSize) {
		return ParsedKey{}, ErrInvalidRawKey
	}
	return ParsedKey{Prefix: prefix}, nil
}

func HashKey(rawKey string, pepper []byte) ([]byte, error) {
	if err := ValidatePepper(pepper); err != nil {
		return nil, err
	}
	mac := hmac.New(sha256.New, pepper)
	_, _ = mac.Write([]byte(rawKey))
	return mac.Sum(nil), nil
}

func ValidatePepper(pepper []byte) error {
	if len(pepper) < 32 {
		return errors.New("virtual key pepper must be at least 32 bytes")
	}
	return nil
}

func isCanonicalComponent(encoded string, decodedSize int) bool {
	decoded, err := rawURLEncoding.DecodeString(encoded)
	if err != nil || len(decoded) != decodedSize {
		return false
	}
	return rawURLEncoding.EncodeToString(decoded) == encoded
}

func generateRawKey(random io.Reader) (string, string, error) {
	prefixBytes := make([]byte, prefixRandomSize)
	if _, err := io.ReadFull(random, prefixBytes); err != nil {
		return "", "", err
	}
	secretBytes := make([]byte, secretRandomSize)
	if _, err := io.ReadFull(random, secretBytes); err != nil {
		return "", "", err
	}
	prefix := rawURLEncoding.EncodeToString(prefixBytes)
	secret := rawURLEncoding.EncodeToString(secretBytes)
	return rawKeyMarker + prefix + "_" + secret, prefix, nil
}
