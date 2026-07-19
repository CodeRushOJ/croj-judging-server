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
		ManifestJSON:         []byte(`{"schemaVersion":1,"judgeMode":"ACM","checker":"exact","cases":[{"id":"case-1","input":"1.in","output":"1.out","weight":1}]}`),
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
