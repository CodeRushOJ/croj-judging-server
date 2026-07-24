package external

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"
	"time"
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
