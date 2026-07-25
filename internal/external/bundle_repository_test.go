package external

import (
	"crypto/sha256"
	"encoding/hex"
	"math"
	"testing"
	"time"

	"github.com/CodeRushOJ/croj-judging-server/internal/bundle"
)

func TestBundleCommitInputRequiresServerDerivedTenantContentAddress(t *testing.T) {
	digest := sha256.Sum256([]byte("bundle"))
	createdAt := time.Date(2026, 7, 19, 1, 2, 3, 0, time.UTC)
	valid := BundleCommitInput{
		TenantID: testTenantID, RequestHash: digest,
		ObjectKey:            "external/" + testTenantID + "/sha256/" + hex.EncodeToString(digest[:]) + ".zip",
		StagingObjectKey:     "external-staging/" + testTenantID + "/cccccccccccccccccccccccccc/" + hex.EncodeToString(digest[:]) + ".zip",
		ManifestJSON:         []byte(`{"schemaVersion":1,"judgeMode":"ACM","checker":"exact","limits":{"timeLimitMillis":1000,"memoryLimitMiB":256},"cases":[{"id":"case-1","input":"1.in","output":"1.out","weight":1}]}`),
		TimeLimitMillis:      1000,
		MemoryLimitMiB:       256,
		Metadata:             BundleMetadata{BundleID: "bbbbbbbbbbbbbbbbbbbbbbbbbb", SHA256: hex.EncodeToString(digest[:]), SizeBytes: 1, CaseCount: 1, ManifestVersion: 1, CreatedAt: createdAt},
		IdempotencyExpiresAt: createdAt.Add(time.Hour),
	}
	if !validBundleCommitInput(valid) {
		t.Fatal("valid content-addressed commit was rejected")
	}
	callerKey := valid
	callerKey.ObjectKey = "external/attacker-selected.zip"
	if validBundleCommitInput(callerKey) {
		t.Fatal("caller-selected object key was accepted")
	}
	mismatchedDigest := valid
	otherDigest := sha256.Sum256([]byte("other"))
	mismatchedDigest.Metadata.SHA256 = hex.EncodeToString(otherDigest[:])
	if validBundleCommitInput(mismatchedDigest) {
		t.Fatal("metadata digest was not bound to the content address")
	}
}

func TestBundleCommitInputMustStayWithinTenantExecutionCeilings(t *testing.T) {
	policy := TenantPolicy{MaxTimeLimitMillis: 2000, MaxMemoryLimitMiB: 512}
	if !bundleWithinTenantPolicy(BundleCommitInput{TimeLimitMillis: 2000, MemoryLimitMiB: 512}, policy) {
		t.Fatal("limits at tenant ceilings were rejected")
	}
	if bundleWithinTenantPolicy(BundleCommitInput{TimeLimitMillis: 2001, MemoryLimitMiB: 512}, policy) ||
		bundleWithinTenantPolicy(BundleCommitInput{TimeLimitMillis: 2000, MemoryLimitMiB: 513}, policy) {
		t.Fatal("bundle limits above tenant ceilings were accepted")
	}
}

func TestManifestExecutionCeilingsIncludeSpecialJudgeLimits(t *testing.T) {
	manifest := bundle.Manifest{
		Limits: bundle.Limits{TimeLimitMillis: 1000, MemoryLimitMiB: 128},
		SpecialJudge: &bundle.SpecialJudge{
			TimeLimitMillis: 2001,
			MemoryLimitMiB:  513,
		},
	}
	if manifestWithinExecutionCeilings(manifest, 2000, 512) {
		t.Fatal("special judge limits above tenant ceilings were accepted")
	}
	manifest.SpecialJudge.TimeLimitMillis = 2000
	manifest.SpecialJudge.MemoryLimitMiB = 512
	if !manifestWithinExecutionCeilings(manifest, 2000, 512) {
		t.Fatal("special judge limits at tenant ceilings were rejected")
	}
}

func TestBundleTenantPolicyUsesAuthoritativeManifestSpecialJudgeLimits(t *testing.T) {
	policy := TenantPolicy{MaxTimeLimitMillis: 2000, MaxMemoryLimitMiB: 512}
	input := BundleCommitInput{
		TimeLimitMillis: 1000,
		MemoryLimitMiB:  128,
		ManifestJSON: []byte(
			`{"schemaVersion":2,"judgeMode":"ACM","checker":"special","limits":{"timeLimitMillis":1000,"memoryLimitMiB":128},` +
				`"specialJudge":{"language":"go","source":"checker.go","sourceSha256":"0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef","timeLimitMillis":2001,"memoryLimitMiB":512},` +
				`"cases":[{"id":"case-1","input":"1.in","output":"1.out","weight":1}]}`,
		),
	}
	if bundleWithinTenantPolicy(input, policy) {
		t.Fatal("authoritative special judge limit above tenant policy was accepted")
	}
}

func TestMaximumManifestExecutionMillisIncludesSpecialJudgePerCase(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		manifest bundle.Manifest
		want     int64
		ok       bool
	}{
		{
			name: "contestant only",
			manifest: bundle.Manifest{
				Limits: bundle.Limits{TimeLimitMillis: 1000},
				Cases:  []bundle.Case{{}, {}},
			},
			want: 2000,
			ok:   true,
		},
		{
			name: "contestant and special judge",
			manifest: bundle.Manifest{
				Limits:       bundle.Limits{TimeLimitMillis: 1000},
				SpecialJudge: &bundle.SpecialJudge{TimeLimitMillis: 2000},
				Cases:        []bundle.Case{{}, {}},
			},
			want: 6000,
			ok:   true,
		},
		{
			name:     "missing cases",
			manifest: bundle.Manifest{Limits: bundle.Limits{TimeLimitMillis: 1000}},
		},
		{
			name: "invalid special judge limit",
			manifest: bundle.Manifest{
				Limits:       bundle.Limits{TimeLimitMillis: 1000},
				SpecialJudge: &bundle.SpecialJudge{},
				Cases:        []bundle.Case{{}},
			},
		},
		{
			name: "per case overflow",
			manifest: bundle.Manifest{
				Limits:       bundle.Limits{TimeLimitMillis: math.MaxInt},
				SpecialJudge: &bundle.SpecialJudge{TimeLimitMillis: 1},
				Cases:        []bundle.Case{{}},
			},
		},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got, ok := maximumManifestExecutionMillis(test.manifest)
			if got != test.want || ok != test.ok {
				t.Fatalf("maximumManifestExecutionMillis() = (%d, %v), want (%d, %v)", got, ok, test.want, test.ok)
			}
		})
	}
}
