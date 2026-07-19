package external

import (
	"crypto/sha256"
	"net/http"
	"testing"

	"github.com/minio/minio-go/v7"
)

func TestRemoteBundleMustMatchSizeAndSHA256MetadataBeforeReady(t *testing.T) {
	digest := sha256.Sum256([]byte("bundle"))
	valid := minio.ObjectInfo{Size: 6, Metadata: http.Header{"X-Amz-Meta-Sha256": []string{"1e6ed65d77d6364eeaed5a745ba5c4985ae2b700dd85d7cf7f027bdf294a33fc"}}}
	if err := verifyBundleObject(valid, 6, digest); err != nil {
		t.Fatal(err)
	}
	wrongSize := valid
	wrongSize.Size++
	if err := verifyBundleObject(wrongSize, 6, digest); err == nil {
		t.Fatal("remote size mismatch was accepted")
	}
	wrongDigest := valid
	wrongDigest.Metadata = http.Header{"X-Amz-Meta-Sha256": []string{"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}}
	if err := verifyBundleObject(wrongDigest, 6, digest); err == nil {
		t.Fatal("remote SHA-256 metadata mismatch was accepted")
	}
}
