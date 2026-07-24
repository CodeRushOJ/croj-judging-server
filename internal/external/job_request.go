package external

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"unicode/utf8"
)

var languageIDPattern = regexp.MustCompile(`^[a-z][a-z0-9._-]{1,31}$`)

type JudgeJobRequest struct {
	BundleID        string
	Language        string
	SourceCode      []byte
	StopOnFailure   bool
	CallbackID      string
	ClientReference string
}

func ValidateIdempotencyKey(key string) error {
	if len(key) < 16 || len(key) > 128 {
		return fmt.Errorf("idempotency key must contain 16 to 128 visible ASCII characters")
	}
	for index := 0; index < len(key); index++ {
		if key[index] < 0x21 || key[index] > 0x7e {
			return fmt.Errorf("idempotency key must contain visible ASCII without whitespace")
		}
	}
	return nil
}

func DigestIdempotencyKey(key string, pepper []byte) ([]byte, error) {
	if err := ValidateIdempotencyKey(key); err != nil {
		return nil, err
	}
	if len(pepper) < sha256.Size {
		return nil, fmt.Errorf("idempotency pepper must contain at least 256 bits")
	}
	digest := hmac.New(sha256.New, pepper)
	_, _ = digest.Write([]byte(key))
	return digest.Sum(nil), nil
}

func CanonicalJobRequestHash(request JudgeJobRequest, maxSourceBytes int64) ([]byte, error) {
	if !externalIDPattern.MatchString(request.BundleID) {
		return nil, fmt.Errorf("bundle ID is invalid")
	}
	if !languageIDPattern.MatchString(request.Language) {
		return nil, fmt.Errorf("language ID is invalid")
	}
	if maxSourceBytes <= 0 || len(request.SourceCode) == 0 || int64(len(request.SourceCode)) > maxSourceBytes || !utf8.Valid(request.SourceCode) {
		return nil, fmt.Errorf("source code is empty, oversized, or not UTF-8")
	}
	if request.CallbackID != "" && !externalIDPattern.MatchString(request.CallbackID) {
		return nil, fmt.Errorf("callback ID is invalid")
	}
	if len(request.ClientReference) > 255 || !utf8.ValidString(request.ClientReference) {
		return nil, fmt.Errorf("client reference is oversized or not UTF-8")
	}
	sourceDigest := sha256.Sum256(request.SourceCode)
	canonical := struct {
		BundleID        string `json:"bundleId"`
		Language        string `json:"language"`
		SourceSHA256    string `json:"sourceSha256"`
		SourceSizeBytes int    `json:"sourceSizeBytes"`
		StopOnFailure   bool   `json:"stopOnFailure"`
		CallbackID      string `json:"callbackId,omitempty"`
		ClientReference string `json:"clientReference,omitempty"`
	}{
		BundleID:        request.BundleID,
		Language:        request.Language,
		SourceSHA256:    hex.EncodeToString(sourceDigest[:]),
		SourceSizeBytes: len(request.SourceCode),
		StopOnFailure:   request.StopOnFailure,
		CallbackID:      request.CallbackID,
		ClientReference: request.ClientReference,
	}
	encoded, err := json.Marshal(canonical)
	if err != nil {
		return nil, fmt.Errorf("encode canonical job request: %w", err)
	}
	digest := sha256.Sum256(encoded)
	return append([]byte(nil), digest[:]...), nil
}
