package bundle

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/CodeRushOJ/croj-judging-server/pkg/model"
	"gorm.io/datatypes"
)

func TestProviderOpensVerifiedArtifact(t *testing.T) {
	zipPath := writeZIP(t, []zipEntry{{name: "manifest.json", body: validManifest}, {name: "cases/01.in", body: "in"}, {name: "cases/01.out", body: "out"}})
	data, err := os.ReadFile(zipPath)
	if err != nil {
		t.Fatal(err)
	}
	store := &fakeObjectStore{objects: map[string][]byte{"bundle.zip": data}}
	cache, err := NewCache(t.TempDir(), 1<<20, 1<<20, time.Hour, store)
	if err != nil {
		t.Fatal(err)
	}
	provider := NewProvider(cache, DefaultArchiveLimits())
	metadata := modelMetadata("bundle.zip", data, validManifest)
	artifact, err := provider.Open(context.Background(), metadata)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer artifact.Close()
	if artifact.Manifest().Cases[0].ID != "case-01" {
		t.Fatalf("manifest = %+v", artifact.Manifest())
	}
}

func TestProviderClassifiesBadBundleAsInvalid(t *testing.T) {
	data := []byte("not a ZIP")
	store := &fakeObjectStore{objects: map[string][]byte{"bad.zip": data}}
	cache, err := NewCache(t.TempDir(), 1<<20, 1<<20, time.Hour, store)
	if err != nil {
		t.Fatal(err)
	}
	provider := NewProvider(cache, DefaultArchiveLimits())
	_, err = provider.Open(context.Background(), modelMetadata("bad.zip", data, validManifest))
	if err == nil || !IsInvalid(err) {
		t.Fatalf("error=%v invalid=%v", err, IsInvalid(err))
	}
}

func TestProviderOpensExternalMetadataWithoutProblemVersionModel(t *testing.T) {
	zipPath := writeZIP(t, []zipEntry{{name: "manifest.json", body: validManifest}, {name: "cases/01.in", body: "in"}, {name: "cases/01.out", body: "out"}})
	data, err := os.ReadFile(zipPath)
	if err != nil {
		t.Fatal(err)
	}
	store := &fakeObjectStore{objects: map[string][]byte{"external.zip": data}}
	cache, err := NewCache(t.TempDir(), 1<<20, 1<<20, time.Hour, store)
	if err != nil {
		t.Fatal(err)
	}
	artifact, err := NewProvider(cache, DefaultArchiveLimits()).OpenMetadata(context.Background(), cacheMetadata("external.zip", data), []byte(validManifest))
	if err != nil {
		t.Fatal(err)
	}
	defer artifact.Close()
	if artifact.Manifest().Limits.TimeLimitMillis != 1500 || artifact.Manifest().Limits.MemoryLimitMiB != 256 {
		t.Fatalf("manifest limits = %+v", artifact.Manifest().Limits)
	}
}

func modelMetadata(key string, data []byte, manifest string) *model.TestBundle {
	metadata := cacheMetadata(key, data)
	return &model.TestBundle{
		ProblemVersionID: 7,
		ObjectKey:        key,
		SHA256:           metadata.SHA256,
		SizeBytes:        metadata.SizeBytes,
		ManifestJSON:     datatypes.JSON([]byte(manifest)),
	}
}
