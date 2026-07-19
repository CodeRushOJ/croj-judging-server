package external

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base32"
	"encoding/base64"
	"fmt"
	"io"
	"strings"
)

const (
	apiKeyPrefixBytes = 9
	apiKeySecretBytes = 32
	apiKeyRandomBytes = apiKeyPrefixBytes + apiKeySecretBytes
)

type APIKeyMaterial struct {
	Plaintext    string
	LookupPrefix string
	Digest       []byte
}

func (APIKeyMaterial) String() string { return "[REDACTED API KEY]" }

func GenerateAPIKey(random io.Reader, pepper []byte) (APIKeyMaterial, error) {
	if random == nil {
		return APIKeyMaterial{}, fmt.Errorf("cryptographic random source is required")
	}
	if len(pepper) < sha256.Size {
		return APIKeyMaterial{}, fmt.Errorf("API key pepper must contain at least 256 bits")
	}
	randomBytes := make([]byte, apiKeyRandomBytes)
	if _, err := io.ReadFull(random, randomBytes); err != nil {
		return APIKeyMaterial{}, fmt.Errorf("generate API key entropy: %w", err)
	}
	prefix := strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(randomBytes[:apiKeyPrefixBytes]))
	secret := base64.RawURLEncoding.EncodeToString(randomBytes[apiKeyPrefixBytes:])
	plaintext := "croj_" + prefix + "_" + secret
	digest := hmac.New(sha256.New, pepper)
	_, _ = digest.Write([]byte(plaintext))
	return APIKeyMaterial{
		Plaintext:    plaintext,
		LookupPrefix: prefix,
		Digest:       digest.Sum(nil),
	}, nil
}
