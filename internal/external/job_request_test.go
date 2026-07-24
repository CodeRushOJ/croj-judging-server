package external

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"strings"
	"testing"
)

func TestValidateIdempotencyKeyAcceptsOnlyBoundedVisibleASCII(t *testing.T) {
	valid := strings.Repeat("A", 16) + "-submission-42"
	if err := ValidateIdempotencyKey(valid); err != nil {
		t.Fatal(err)
	}
	for name, value := range map[string]string{
		"short":   "short",
		"long":    strings.Repeat("x", 129),
		"space":   strings.Repeat("x", 16) + " key",
		"control": strings.Repeat("x", 16) + "\n",
		"unicode": strings.Repeat("x", 16) + "密钥",
	} {
		t.Run(name, func(t *testing.T) {
			if err := ValidateIdempotencyKey(value); err == nil {
				t.Fatal("expected invalid idempotency key")
			}
		})
	}
}

func TestIdempotencyKeyDigestUsesASeparatePepperedHMAC(t *testing.T) {
	pepper := bytes.Repeat([]byte{0x33}, sha256.Size)
	key := "submission-00000042"
	digest, err := DigestIdempotencyKey(key, pepper)
	if err != nil {
		t.Fatal(err)
	}
	want := hmac.New(sha256.New, pepper)
	_, _ = want.Write([]byte(key))
	if !hmac.Equal(digest, want.Sum(nil)) {
		t.Fatalf("digest = %x", digest)
	}
}

func TestCanonicalJobRequestHashCoversEveryMeaningfulFieldWithoutRetainingSource(t *testing.T) {
	base := JudgeJobRequest{
		BundleID:        "ceirceirceirceirceirceirce",
		Language:        "cpp",
		SourceCode:      []byte("int main() { return 0; }"),
		StopOnFailure:   true,
		CallbackID:      "ceirceirceirceirceirceircf",
		ClientReference: "submission-42",
	}
	want, err := CanonicalJobRequestHash(base, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	mutations := map[string]func(JudgeJobRequest) JudgeJobRequest{
		"bundle": func(value JudgeJobRequest) JudgeJobRequest {
			value.BundleID = "ceirceirceirceirceirceircg"
			return value
		},
		"language": func(value JudgeJobRequest) JudgeJobRequest { value.Language = "java21"; return value },
		"source": func(value JudgeJobRequest) JudgeJobRequest {
			value.SourceCode = []byte("int main() { return 1; }")
			return value
		},
		"stop":      func(value JudgeJobRequest) JudgeJobRequest { value.StopOnFailure = false; return value },
		"callback":  func(value JudgeJobRequest) JudgeJobRequest { value.CallbackID = ""; return value },
		"reference": func(value JudgeJobRequest) JudgeJobRequest { value.ClientReference = "submission-43"; return value },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			got, err := CanonicalJobRequestHash(mutate(base), 1<<20)
			if err != nil {
				t.Fatal(err)
			}
			if hmac.Equal(got, want) {
				t.Fatalf("mutation %s did not change canonical hash", name)
			}
		})
	}
}

func TestCanonicalJobRequestRejectsInvalidOrOversizeFields(t *testing.T) {
	valid := JudgeJobRequest{BundleID: "ceirceirceirceirceirceirce", Language: "cpp", SourceCode: []byte("x")}
	tests := map[string]JudgeJobRequest{
		"bundle":       {Language: "cpp", SourceCode: []byte("x")},
		"language":     {BundleID: valid.BundleID, Language: "../../bin/sh", SourceCode: []byte("x")},
		"empty source": {BundleID: valid.BundleID, Language: "cpp"},
		"large source": {BundleID: valid.BundleID, Language: "cpp", SourceCode: []byte("xx")},
		"callback":     {BundleID: valid.BundleID, Language: "cpp", SourceCode: []byte("x"), CallbackID: "bad"},
		"reference":    {BundleID: valid.BundleID, Language: "cpp", SourceCode: []byte("x"), ClientReference: strings.Repeat("x", 256)},
	}
	for name, request := range tests {
		t.Run(name, func(t *testing.T) {
			limit := int64(1 << 20)
			if name == "large source" {
				limit = 1
			}
			if _, err := CanonicalJobRequestHash(request, limit); err == nil {
				t.Fatal("expected request rejection")
			}
		})
	}
}
