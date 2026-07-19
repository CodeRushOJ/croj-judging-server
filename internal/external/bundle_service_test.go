package external

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/CodeRushOJ/croj-judging-server/internal/bundle"
)

const testTenantID = "aaaaaaaaaaaaaaaaaaaaaaaaaa"

type memoryBundleRepository struct {
	mu             sync.Mutex
	idempotency    map[string]bundleUploadRecord
	bundles        map[string]BundleMetadata
	logicalCreates int
}

type bundleUploadRecord struct {
	requestHash [sha256.Size]byte
	metadata    BundleMetadata
}

func newMemoryBundleRepository() *memoryBundleRepository {
	return &memoryBundleRepository{idempotency: map[string]bundleUploadRecord{}, bundles: map[string]BundleMetadata{}}
}

func (repository *memoryBundleRepository) FindBundleUpload(_ context.Context, tenantID string, keyDigest [sha256.Size]byte) (BundleUploadLookup, error) {
	repository.mu.Lock()
	defer repository.mu.Unlock()
	record, found := repository.idempotency[tenantID+":"+hex.EncodeToString(keyDigest[:])]
	return BundleUploadLookup{Found: found, RequestHash: record.requestHash, Metadata: record.metadata}, nil
}

func (repository *memoryBundleRepository) CommitBundleUpload(_ context.Context, input BundleCommitInput) (BundleCommitResult, error) {
	repository.mu.Lock()
	defer repository.mu.Unlock()
	idempotencyKey := input.TenantID + ":" + hex.EncodeToString(input.IdempotencyDigest[:])
	if record, found := repository.idempotency[idempotencyKey]; found {
		if record.requestHash != input.RequestHash {
			return BundleCommitResult{}, ErrIdempotencyConflict
		}
		return BundleCommitResult{Metadata: record.metadata, Replay: true}, nil
	}
	bundleKey := input.TenantID + ":" + hex.EncodeToString(input.RequestHash[:])
	metadata, found := repository.bundles[bundleKey]
	if !found {
		metadata = input.Metadata
		repository.bundles[bundleKey] = metadata
		repository.logicalCreates++
	}
	repository.idempotency[idempotencyKey] = bundleUploadRecord{requestHash: input.RequestHash, metadata: metadata}
	return BundleCommitResult{Metadata: metadata, Replay: found}, nil
}

func (repository *memoryBundleRepository) FindBundle(_ context.Context, tenantID, bundleID string) (BundleMetadata, error) {
	repository.mu.Lock()
	defer repository.mu.Unlock()
	for key, metadata := range repository.bundles {
		if strings.HasPrefix(key, tenantID+":") && metadata.BundleID == bundleID {
			return metadata, nil
		}
	}
	return BundleMetadata{}, ErrBundleNotFound
}

type atomicMemoryObjectStore struct {
	mu        sync.Mutex
	objects   map[string][]byte
	publishes int
}

func (store *atomicMemoryObjectStore) Publish(_ context.Context, key, filename string, size int64, digest [sha256.Size]byte) error {
	data, err := os.ReadFile(filename)
	if err != nil {
		return err
	}
	actual := sha256.Sum256(data)
	if int64(len(data)) != size || actual != digest {
		return errors.New("publish metadata mismatch")
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	store.publishes++
	if store.objects == nil {
		store.objects = map[string][]byte{}
	}
	if _, exists := store.objects[key]; !exists {
		store.objects[key] = append([]byte(nil), data...)
	}
	return nil
}

func TestBundleServiceStreamsValidUploadToTenantContentAddress(t *testing.T) {
	repository := newMemoryBundleRepository()
	store := &atomicMemoryObjectStore{}
	tempDir := t.TempDir()
	service := newTestBundleService(t, repository, store, tempDir, 1<<20, bundle.DefaultArchiveLimits())
	body := validExternalBundle(t, "input")

	result, replay, err := service.Upload(context.Background(), testTenantID, "upload-key-00001", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(body)
	wantKey := "external/" + testTenantID + "/sha256/" + hex.EncodeToString(digest[:]) + ".zip"
	if replay || result.SHA256 != hex.EncodeToString(digest[:]) || result.SizeBytes != int64(len(body)) || result.CaseCount != 1 || result.ManifestVersion != 1 || result.BundleID == "" {
		t.Fatalf("result=%+v replay=%v", result, replay)
	}
	if len(store.objects) != 1 || !bytes.Equal(store.objects[wantKey], body) || repository.logicalCreates != 1 {
		t.Fatalf("objects=%v logicalCreates=%d", mapsKeys(store.objects), repository.logicalCreates)
	}
	assertDirectoryEmpty(t, tempDir)
}

func TestBundleServiceRejectsOversizeAndCancelledStreamsWithoutArtifacts(t *testing.T) {
	for name, test := range map[string]struct {
		context context.Context
		reader  io.Reader
		limit   int64
		want    error
	}{
		"oversize":  {context: context.Background(), reader: strings.NewReader(strings.Repeat("x", 65)), limit: 64, want: ErrBundleTooLarge},
		"cancelled": {context: cancelledContext(), reader: strings.NewReader(strings.Repeat("x", 32)), limit: 64, want: context.Canceled},
	} {
		t.Run(name, func(t *testing.T) {
			repository := newMemoryBundleRepository()
			store := &atomicMemoryObjectStore{}
			tempDir := t.TempDir()
			service := newTestBundleService(t, repository, store, tempDir, test.limit, bundle.DefaultArchiveLimits())
			_, _, err := service.Upload(test.context, testTenantID, "upload-key-00001", test.reader)
			if !errors.Is(err, test.want) {
				t.Fatalf("error=%v want=%v", err, test.want)
			}
			if len(store.objects) != 0 || repository.logicalCreates != 0 {
				t.Fatalf("partial artifact became visible: objects=%d rows=%d", len(store.objects), repository.logicalCreates)
			}
			assertDirectoryEmpty(t, tempDir)
		})
	}
}

func TestBundleServiceReusesHardenedArchiveValidation(t *testing.T) {
	validManifest := manifestJSON(t, "cases/1.in", "cases/1.out")
	tests := map[string]struct {
		entries map[string]zipEntry
		limits  bundle.ArchiveLimits
	}{
		"traversal":          {entries: map[string]zipEntry{"manifest.json": {body: validManifest}, "cases/1.in": {body: []byte("in")}, "cases/1.out": {body: []byte("out")}, "../escape": {body: []byte("x")}}, limits: bundle.DefaultArchiveLimits()},
		"symlink":            {entries: map[string]zipEntry{"manifest.json": {body: validManifest}, "cases/1.in": {body: []byte("in")}, "cases/1.out": {body: []byte("out"), mode: os.ModeSymlink | 0o777}}, limits: bundle.DefaultArchiveLimits()},
		"missing pair":       {entries: map[string]zipEntry{"manifest.json": {body: validManifest}, "cases/1.in": {body: []byte("in")}}, limits: bundle.DefaultArchiveLimits()},
		"file count":         {entries: map[string]zipEntry{"manifest.json": {body: validManifest}, "cases/1.in": {body: []byte("in")}, "cases/1.out": {body: []byte("out")}}, limits: archiveLimits(2, 1<<20, 64<<20, 512<<20, 200)},
		"compression ratio":  {entries: map[string]zipEntry{"manifest.json": {body: validManifest}, "cases/1.in": {body: bytes.Repeat([]byte("a"), 4096)}, "cases/1.out": {body: []byte("out")}}, limits: archiveLimits(10, 1<<20, 64<<20, 512<<20, 1)},
		"uncompressed total": {entries: map[string]zipEntry{"manifest.json": {body: validManifest}, "cases/1.in": {body: []byte("12345678")}, "cases/1.out": {body: []byte("out")}}, limits: archiveLimits(10, 1<<20, 64<<20, int64(len(validManifest)+10), 200)},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			repository := newMemoryBundleRepository()
			store := &atomicMemoryObjectStore{}
			tempDir := t.TempDir()
			service := newTestBundleService(t, repository, store, tempDir, 1<<20, test.limits)
			_, _, err := service.Upload(context.Background(), testTenantID, "upload-key-00001", bytes.NewReader(zipBytes(t, test.entries)))
			if !errors.Is(err, ErrInvalidBundle) {
				t.Fatalf("error=%v want ErrInvalidBundle", err)
			}
			if len(store.objects) != 0 || repository.logicalCreates != 0 {
				t.Fatal("invalid bundle was published")
			}
			assertDirectoryEmpty(t, tempDir)
		})
	}
}

func TestBundleServiceRejectsManifestAboveExternalCaseLimit(t *testing.T) {
	manifest := bundle.Manifest{SchemaVersion: 1, JudgeMode: bundle.JudgeModeACM, Checker: bundle.CheckerExact}
	entries := map[string]zipEntry{}
	for index := range 257 {
		id := fmt.Sprintf("case-%03d", index)
		input, output := id+".in", id+".out"
		manifest.Cases = append(manifest.Cases, bundle.Case{ID: id, Input: input, Output: output, Weight: 1})
		entries[input] = zipEntry{body: []byte("in")}
		entries[output] = zipEntry{body: []byte("out")}
	}
	manifestBytes, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	entries["manifest.json"] = zipEntry{body: manifestBytes}
	repository := newMemoryBundleRepository()
	store := &atomicMemoryObjectStore{}
	service := newTestBundleService(t, repository, store, t.TempDir(), 1<<20, bundle.DefaultArchiveLimits())
	_, _, err = service.Upload(context.Background(), testTenantID, "upload-key-00001", bytes.NewReader(zipBytes(t, entries)))
	if !errors.Is(err, ErrInvalidBundle) || repository.logicalCreates != 0 || len(store.objects) != 0 {
		t.Fatalf("error=%v rows=%d objects=%d", err, repository.logicalCreates, len(store.objects))
	}
}

func TestBundleServiceIdempotencyAndTenantMetadataIsolation(t *testing.T) {
	repository := newMemoryBundleRepository()
	store := &atomicMemoryObjectStore{}
	service := newTestBundleService(t, repository, store, t.TempDir(), 1<<20, bundle.DefaultArchiveLimits())
	body := validExternalBundle(t, "input")
	first, replay, err := service.Upload(context.Background(), testTenantID, "upload-key-00001", bytes.NewReader(body))
	if err != nil || replay {
		t.Fatalf("first result=%+v replay=%v error=%v", first, replay, err)
	}
	second, replay, err := service.Upload(context.Background(), testTenantID, "upload-key-00001", bytes.NewReader(body))
	if err != nil || !replay || second.BundleID != first.BundleID || store.publishes != 1 {
		t.Fatalf("replay result=%+v replay=%v error=%v publishes=%d", second, replay, err, store.publishes)
	}
	if _, _, err := service.Upload(context.Background(), testTenantID, "upload-key-00001", bytes.NewReader(validExternalBundle(t, "different"))); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("conflicting replay error=%v", err)
	}
	if _, err := service.Get(context.Background(), "bbbbbbbbbbbbbbbbbbbbbbbbbb", first.BundleID); !errors.Is(err, ErrBundleNotFound) {
		t.Fatalf("cross-tenant get error=%v", err)
	}
	metadata, err := service.Get(context.Background(), testTenantID, first.BundleID)
	if err != nil || metadata.BundleID != first.BundleID {
		t.Fatalf("owner metadata=%+v error=%v", metadata, err)
	}
}

func TestBundleServiceConcurrentIdenticalUploadsCreateOneLogicalRecord(t *testing.T) {
	repository := newMemoryBundleRepository()
	store := &atomicMemoryObjectStore{}
	service := newTestBundleService(t, repository, store, t.TempDir(), 1<<20, bundle.DefaultArchiveLimits())
	body := validExternalBundle(t, "input")
	const uploads = 32
	results := make(chan BundleMetadata, uploads)
	errorsChannel := make(chan error, uploads)
	var wait sync.WaitGroup
	for index := range uploads {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			metadata, _, err := service.Upload(context.Background(), testTenantID, fmt.Sprintf("upload-key-%05d", index), bytes.NewReader(body))
			if err != nil {
				errorsChannel <- err
				return
			}
			results <- metadata
		}(index)
	}
	wait.Wait()
	close(results)
	close(errorsChannel)
	for err := range errorsChannel {
		t.Error(err)
	}
	var bundleID string
	for result := range results {
		if bundleID == "" {
			bundleID = result.BundleID
		}
		if result.BundleID != bundleID {
			t.Fatalf("concurrent uploads returned different logical IDs: %q and %q", bundleID, result.BundleID)
		}
	}
	if repository.logicalCreates != 1 || len(store.objects) != 1 {
		t.Fatalf("logical rows=%d visible objects=%d", repository.logicalCreates, len(store.objects))
	}
}

func newTestBundleService(t *testing.T, repository BundleRepository, store BundleObjectStore, tempDir string, maxUploadBytes int64, limits bundle.ArchiveLimits) *BundleService {
	t.Helper()
	service, err := NewBundleService(repository, store, BundleServiceConfig{
		TempDir: tempDir, MaxUploadBytes: maxUploadBytes, ArchiveLimits: limits,
		IdempotencyTTL: 24 * time.Hour, Random: rand.Reader,
	})
	if err != nil {
		t.Fatal(err)
	}
	service.now = func() time.Time { return time.Date(2026, 7, 19, 1, 2, 3, 0, time.UTC) }
	return service
}

type zipEntry struct {
	body []byte
	mode os.FileMode
}

func validExternalBundle(t *testing.T, input string) []byte {
	t.Helper()
	manifest := manifestJSON(t, "cases/1.in", "cases/1.out")
	return zipBytes(t, map[string]zipEntry{
		"manifest.json": {body: manifest}, "cases/1.in": {body: []byte(input)}, "cases/1.out": {body: []byte("answer")},
	})
}

func manifestJSON(t *testing.T, input, output string) []byte {
	t.Helper()
	data, err := json.Marshal(bundle.Manifest{SchemaVersion: 1, JudgeMode: bundle.JudgeModeACM, Checker: bundle.CheckerExact, Cases: []bundle.Case{{ID: "case-1", Input: input, Output: output, Weight: 1}}})
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func zipBytes(t *testing.T, entries map[string]zipEntry) []byte {
	t.Helper()
	var output bytes.Buffer
	writer := zip.NewWriter(&output)
	for name, entry := range entries {
		header := &zip.FileHeader{Name: name, Method: zip.Deflate}
		if entry.mode != 0 {
			header.SetMode(entry.mode)
		} else {
			header.SetMode(0o600)
		}
		file, err := writer.CreateHeader(header)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := file.Write(entry.body); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return output.Bytes()
}

func archiveLimits(files int, manifest, caseBytes, total int64, ratio uint64) bundle.ArchiveLimits {
	return bundle.ArchiveLimits{MaxFiles: files, MaxManifestBytes: manifest, MaxCaseBytes: caseBytes, MaxTotalBytes: total, MaxCompressionRatio: ratio}
}

func cancelledContext() context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return ctx
}

func assertDirectoryEmpty(t *testing.T, directory string) {
	t.Helper()
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		names := make([]string, 0, len(entries))
		for _, entry := range entries {
			names = append(names, filepath.Join(directory, entry.Name()))
		}
		t.Fatalf("temporary files were not cleaned: %v", names)
	}
}

func mapsKeys(values map[string][]byte) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	return keys
}
