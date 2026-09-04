package apikey

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"strings"
	"testing"
)

func TestGenerateRawKeyFormat(t *testing.T) {
	rawKey, prefix, err := GenerateRawKey()
	if err != nil {
		t.Fatalf("GenerateRawKey: %v", err)
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

func TestHashKey(t *testing.T) {
	pepper := bytes.Repeat([]byte{0x11}, 32)
	rawKey := rawKeyMarker + strings.Repeat("A", prefixEncodedSize) + "_" + strings.Repeat("A", secretEncodedSize)
	otherKey := rawKeyMarker + strings.Repeat("B", prefixEncodedSize) + "_" + strings.Repeat("A", secretEncodedSize)

	digest, err := HashKey(rawKey, pepper)
	if err != nil {
		t.Fatalf("HashKey: %v", err)
	}
	sameDigest, err := HashKey(rawKey, pepper)
	if err != nil {
		t.Fatalf("HashKey same input: %v", err)
	}
	otherDigest, err := HashKey(otherKey, pepper)
	if err != nil {
		t.Fatalf("HashKey other input: %v", err)
	}
	otherPepperDigest, err := HashKey(rawKey, bytes.Repeat([]byte{0x22}, 32))
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

func TestHashKeyRejectsShortPepper(t *testing.T) {
	if _, err := HashKey("test-key", bytes.Repeat([]byte{0x11}, 31)); err == nil {
		t.Fatal("HashKey succeeded with short pepper")
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
