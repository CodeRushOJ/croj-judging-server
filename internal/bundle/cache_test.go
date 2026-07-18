package bundle

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type fakeObjectStore struct {
	objects map[string][]byte
	delay   time.Duration
	calls   atomic.Int32
}

func (store *fakeObjectStore) Open(ctx context.Context, key string) (Object, error) {
	store.calls.Add(1)
	if store.delay > 0 {
		select {
		case <-ctx.Done():
			return Object{}, ctx.Err()
		case <-time.After(store.delay):
		}
	}
	data := store.objects[key]
	return Object{Body: io.NopCloser(bytes.NewReader(data)), Size: int64(len(data))}, nil
}

func TestCacheDownloadsVerifiesAndRepairsCorruption(t *testing.T) {
	data := []byte("deterministic bundle")
	store := &fakeObjectStore{objects: map[string][]byte{"bundles/a.zip": data}}
	cache, err := NewCache(t.TempDir(), 1<<20, 1<<20, time.Hour, store)
	if err != nil {
		t.Fatal(err)
	}
	metadata := cacheMetadata("bundles/a.zip", data)
	path, err := cache.Resolve(context.Background(), metadata)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if store.calls.Load() != 1 {
		t.Fatalf("downloads = %d", store.calls.Load())
	}
	if err := os.WriteFile(path, []byte("corrupt"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := cache.Resolve(context.Background(), metadata); err != nil {
		t.Fatalf("repair Resolve: %v", err)
	}
	if store.calls.Load() != 2 {
		t.Fatalf("downloads after corruption = %d", store.calls.Load())
	}
}

func TestCacheCoalescesConcurrentDownload(t *testing.T) {
	data := []byte("one shared download")
	store := &fakeObjectStore{objects: map[string][]byte{"bundle.zip": data}, delay: 20 * time.Millisecond}
	cache, err := NewCache(t.TempDir(), 1<<20, 1<<20, time.Hour, store)
	if err != nil {
		t.Fatal(err)
	}
	metadata := cacheMetadata("bundle.zip", data)
	var wait sync.WaitGroup
	errorsChannel := make(chan error, 12)
	for range 12 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			_, err := cache.Resolve(context.Background(), metadata)
			errorsChannel <- err
		}()
	}
	wait.Wait()
	close(errorsChannel)
	for err := range errorsChannel {
		if err != nil {
			t.Fatal(err)
		}
	}
	if store.calls.Load() != 1 {
		t.Fatalf("downloads = %d, want 1", store.calls.Load())
	}
}

func TestCacheRejectsSizeAndChecksumMismatchWithoutFinalFile(t *testing.T) {
	data := []byte("bundle")
	for name, mutate := range map[string]func(*Metadata){
		"size":     func(metadata *Metadata) { metadata.SizeBytes++ },
		"checksum": func(metadata *Metadata) { metadata.SHA256 = strings64("0") },
	} {
		t.Run(name, func(t *testing.T) {
			directory := t.TempDir()
			store := &fakeObjectStore{objects: map[string][]byte{"bundle.zip": data}}
			cache, err := NewCache(directory, 1<<20, 1<<20, time.Hour, store)
			if err != nil {
				t.Fatal(err)
			}
			metadata := cacheMetadata("bundle.zip", data)
			mutate(&metadata)
			if _, err := cache.Resolve(context.Background(), metadata); err == nil || !IsInvalid(err) {
				t.Fatalf("expected invalid verification error, got %v", err)
			}
			entries, err := os.ReadDir(directory)
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != 0 {
				t.Fatalf("partial files remain: %v", entries)
			}
		})
	}
}

func TestNewCacheRemovesRestartOrphansWithoutFollowingSymlinks(t *testing.T) {
	directory := t.TempDir()
	external := t.TempDir() + "/external"
	if err := os.WriteFile(external, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	knownBundle := strings64("a") + ".zip"
	for name, body := range map[string]string{knownBundle: "old", ".bundle-download-old": "partial", "unrelated": "keep"} {
		if err := os.WriteFile(directory+"/"+name, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	linkName := directory + "/" + strings64("b") + ".zip"
	if err := os.Symlink(external, linkName); err != nil {
		t.Fatal(err)
	}
	if _, err := NewCache(directory, 1<<20, 1<<20, time.Hour, &fakeObjectStore{}); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{knownBundle, ".bundle-download-old", strings64("b") + ".zip"} {
		if _, err := os.Lstat(directory + "/" + name); !os.IsNotExist(err) {
			t.Fatalf("orphan %q remains: %v", name, err)
		}
	}
	if data, err := os.ReadFile(external); err != nil || string(data) != "keep" {
		t.Fatalf("symlink target changed: %q %v", data, err)
	}
	if _, err := os.Stat(directory + "/unrelated"); err != nil {
		t.Fatal("unrelated file was removed")
	}
}

func TestCacheRejectsUnsafeObjectKeyMetadata(t *testing.T) {
	cache, err := NewCache(t.TempDir(), 1<<20, 1<<20, time.Hour, &fakeObjectStore{})
	if err != nil {
		t.Fatal(err)
	}
	data := []byte("bundle")
	for name, key := range map[string]string{"empty": "", "NUL": "bad\x00key", "long": strings.Repeat("a", 513)} {
		t.Run(name, func(t *testing.T) {
			metadata := cacheMetadata(key, data)
			if _, err := cache.Resolve(context.Background(), metadata); err == nil || !IsInvalid(err) {
				t.Fatalf("expected invalid object key error, got %v", err)
			}
		})
	}
}

func TestCacheEvictsLRUAndExpiredEntries(t *testing.T) {
	first := []byte("first")
	second := []byte("second")
	store := &fakeObjectStore{objects: map[string][]byte{"first": first, "second": second}}
	directory := t.TempDir()
	cache, err := NewCache(directory, int64(len(second)), 1<<20, 15*time.Millisecond, store)
	if err != nil {
		t.Fatal(err)
	}
	firstPath, err := cache.Resolve(context.Background(), cacheMetadata("first", first))
	if err != nil {
		t.Fatal(err)
	}
	secondPath, err := cache.Resolve(context.Background(), cacheMetadata("second", second))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(firstPath); !os.IsNotExist(err) {
		t.Fatalf("LRU entry was not removed: %v", err)
	}
	time.Sleep(20 * time.Millisecond)
	if _, err := cache.Resolve(context.Background(), cacheMetadata("second", second)); err != nil {
		t.Fatal(err)
	}
	if store.calls.Load() != 3 {
		t.Fatalf("expired entry did not redownload, calls=%d", store.calls.Load())
	}
	if _, err := os.Stat(secondPath); err != nil {
		t.Fatal(err)
	}
}

func cacheMetadata(key string, data []byte) Metadata {
	sum := sha256.Sum256(data)
	return Metadata{ObjectKey: key, SHA256: hex.EncodeToString(sum[:]), SizeBytes: int64(len(data))}
}

func strings64(character string) string {
	var buffer bytes.Buffer
	for range 64 {
		buffer.WriteString(character)
	}
	return buffer.String()
}
