package external

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base32"
	"encoding/base64"
	"strings"
	"testing"
)

func TestGenerateAPIKeyProducesLookupPrefixSecretAndOnlyAPepperedDigest(t *testing.T) {
	pepper := []byte("0123456789abcdef0123456789abcdef")
	random := bytes.NewReader(bytes.Repeat([]byte{0x5a}, apiKeyRandomBytes))
	material, err := GenerateAPIKey(random, pepper)
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(material.Plaintext, "_")
	if len(parts) != 3 || parts[0] != "croj" || material.LookupPrefix != parts[1] {
		t.Fatalf("material = %+v", material)
	}
	prefix, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(strings.ToUpper(parts[1]))
	if err != nil || len(prefix) != apiKeyPrefixBytes {
		t.Fatalf("prefix = %q decoded=%d err=%v", parts[1], len(prefix), err)
	}
	secret, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil || len(secret) != apiKeySecretBytes {
		t.Fatalf("secret bytes=%d err=%v", len(secret), err)
	}
	digest := hmac.New(sha256.New, pepper)
	_, _ = digest.Write([]byte(material.Plaintext))
	if !hmac.Equal(material.Digest, digest.Sum(nil)) || len(material.Digest) != sha256.Size {
		t.Fatalf("digest is not HMAC-SHA256: %x", material.Digest)
	}
}

func TestGenerateAPIKeyRejectsWeakConfigurationAndEntropyFailure(t *testing.T) {
	if _, err := GenerateAPIKey(bytes.NewReader(make([]byte, apiKeyRandomBytes)), []byte("short")); err == nil {
		t.Fatal("expected short pepper rejection")
	}
	if _, err := GenerateAPIKey(bytes.NewReader(make([]byte, apiKeyRandomBytes-1)), make([]byte, sha256.Size)); err == nil {
		t.Fatal("expected entropy exhaustion")
	}
}
