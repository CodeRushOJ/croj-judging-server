package bundle

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

type Object struct {
	Body io.ReadCloser
	Size int64
}

type ObjectStore interface {
	Open(context.Context, string) (Object, error)
}

type Metadata struct {
	ObjectKey string
	SHA256    string
	SizeBytes int64
}

type cacheEntry struct {
	path     string
	size     int64
	lastUsed time.Time
}

type cacheFlight struct {
	done chan struct{}
	path string
	err  error
}

type Cache struct {
	directory      string
	maxBytes       int64
	maxObjectBytes int64
	ttl            time.Duration
	store          ObjectStore

	mu      sync.Mutex
	entries map[string]*cacheEntry
	flights map[string]*cacheFlight
}

func NewCache(directory string, maxBytes, maxObjectBytes int64, ttl time.Duration, store ObjectStore) (*Cache, error) {
	if strings.TrimSpace(directory) == "" || maxBytes <= 0 || maxObjectBytes <= 0 || ttl <= 0 || store == nil {
		return nil, fmt.Errorf("bundle cache configuration must be positive and complete")
	}
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return nil, fmt.Errorf("create bundle cache directory: %w", err)
	}
	info, err := os.Lstat(directory)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("bundle cache path must be a directory")
	}
	if err := os.Chmod(directory, 0o700); err != nil {
		return nil, fmt.Errorf("protect bundle cache directory: %w", err)
	}
	if err := cleanupRestartOrphans(directory); err != nil {
		return nil, err
	}
	return &Cache{
		directory:      directory,
		maxBytes:       maxBytes,
		maxObjectBytes: maxObjectBytes,
		ttl:            ttl,
		store:          store,
		entries:        make(map[string]*cacheEntry),
		flights:        make(map[string]*cacheFlight),
	}, nil
}

func (cache *Cache) Resolve(ctx context.Context, metadata Metadata) (string, error) {
	if err := cache.validateMetadata(metadata); err != nil {
		return "", Invalid(err)
	}
	metadata.SHA256 = strings.ToLower(metadata.SHA256)
	cache.mu.Lock()
	cache.pruneExpiredLocked(time.Now())
	if flight := cache.flights[metadata.SHA256]; flight != nil {
		done := flight.done
		cache.mu.Unlock()
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-done:
			return flight.path, flight.err
		}
	}
	flight := &cacheFlight{done: make(chan struct{})}
	cache.flights[metadata.SHA256] = flight
	cache.mu.Unlock()

	path, err := cache.resolveOne(ctx, metadata)
	cache.mu.Lock()
	flight.path, flight.err = path, err
	delete(cache.flights, metadata.SHA256)
	close(flight.done)
	cache.mu.Unlock()
	return path, err
}

func (cache *Cache) resolveOne(ctx context.Context, metadata Metadata) (string, error) {
	target := filepath.Join(cache.directory, metadata.SHA256+".zip")
	if err := verifyCachedFile(target, metadata); err == nil {
		cache.record(metadata.SHA256, target, metadata.SizeBytes)
		return target, nil
	}
	if err := os.Remove(target); err != nil && !os.IsNotExist(err) {
		return "", fmt.Errorf("remove corrupt bundle cache entry: %w", err)
	}
	object, err := cache.store.Open(ctx, metadata.ObjectKey)
	if err != nil {
		return "", fmt.Errorf("open bundle object: %w", err)
	}
	if object.Body == nil {
		return "", fmt.Errorf("bundle object returned no body")
	}
	defer object.Body.Close()
	if object.Size >= 0 && object.Size != metadata.SizeBytes {
		return "", Invalid(fmt.Errorf("bundle object metadata size mismatch"))
	}
	temporary, err := os.CreateTemp(cache.directory, ".bundle-download-*")
	if err != nil {
		return "", fmt.Errorf("create bundle cache temporary file: %w", err)
	}
	temporaryName := temporary.Name()
	succeeded := false
	defer func() {
		_ = temporary.Close()
		if !succeeded {
			_ = os.Remove(temporaryName)
		}
	}()
	hasher := sha256.New()
	written, copyErr := io.Copy(io.MultiWriter(temporary, hasher), io.LimitReader(object.Body, metadata.SizeBytes+1))
	if copyErr != nil {
		return "", fmt.Errorf("download bundle object: %w", copyErr)
	}
	if written != metadata.SizeBytes {
		return "", Invalid(fmt.Errorf("bundle object stream size mismatch"))
	}
	if actual := hex.EncodeToString(hasher.Sum(nil)); actual != metadata.SHA256 {
		return "", Invalid(fmt.Errorf("bundle object SHA-256 mismatch"))
	}
	if err := temporary.Sync(); err != nil {
		return "", fmt.Errorf("sync bundle cache file: %w", err)
	}
	if err := temporary.Chmod(0o600); err != nil {
		return "", fmt.Errorf("protect bundle cache file: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return "", fmt.Errorf("close bundle cache file: %w", err)
	}
	if err := os.Rename(temporaryName, target); err != nil {
		return "", fmt.Errorf("publish bundle cache file: %w", err)
	}
	succeeded = true
	cache.record(metadata.SHA256, target, metadata.SizeBytes)
	return target, nil
}

func (cache *Cache) validateMetadata(metadata Metadata) error {
	if strings.TrimSpace(metadata.ObjectKey) == "" || len(metadata.ObjectKey) > 512 || strings.ContainsRune(metadata.ObjectKey, '\x00') || !utf8.ValidString(metadata.ObjectKey) ||
		metadata.SizeBytes <= 0 || metadata.SizeBytes > cache.maxObjectBytes || metadata.SizeBytes > cache.maxBytes {
		return fmt.Errorf("bundle metadata size or object key is invalid")
	}
	checksum, err := hex.DecodeString(metadata.SHA256)
	if err != nil || len(checksum) != sha256.Size {
		return fmt.Errorf("bundle metadata SHA-256 is invalid")
	}
	return nil
}

func cleanupRestartOrphans(directory string) error {
	entries, err := os.ReadDir(directory)
	if err != nil {
		return fmt.Errorf("scan bundle cache directory: %w", err)
	}
	for _, entry := range entries {
		if !isKnownCacheName(entry.Name()) {
			continue
		}
		if entry.IsDir() {
			return fmt.Errorf("bundle cache entry %q is unexpectedly a directory", entry.Name())
		}
		if err := os.Remove(filepath.Join(directory, entry.Name())); err != nil {
			return fmt.Errorf("remove stale bundle cache entry: %w", err)
		}
	}
	return nil
}

func isKnownCacheName(name string) bool {
	if strings.HasPrefix(name, ".bundle-download-") {
		return true
	}
	if len(name) != 68 || !strings.HasSuffix(name, ".zip") {
		return false
	}
	checksum := name[:64]
	decoded, err := hex.DecodeString(checksum)
	return err == nil && len(decoded) == sha256.Size && checksum == strings.ToLower(checksum)
}

func verifyCachedFile(filename string, metadata Metadata) error {
	info, err := os.Lstat(filename)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Size() != metadata.SizeBytes {
		return fmt.Errorf("cached bundle type or size mismatch")
	}
	file, err := os.Open(filename)
	if err != nil {
		return err
	}
	defer file.Close()
	hasher := sha256.New()
	written, err := io.Copy(hasher, io.LimitReader(file, metadata.SizeBytes+1))
	if err != nil || written != metadata.SizeBytes {
		return fmt.Errorf("read cached bundle: %w", err)
	}
	if hex.EncodeToString(hasher.Sum(nil)) != metadata.SHA256 {
		return fmt.Errorf("cached bundle SHA-256 mismatch")
	}
	return nil
}

func (cache *Cache) record(checksum, path string, size int64) {
	cache.mu.Lock()
	defer cache.mu.Unlock()
	now := time.Now()
	cache.entries[checksum] = &cacheEntry{path: path, size: size, lastUsed: now}
	cache.evictLRULocked(checksum)
}

func (cache *Cache) pruneExpiredLocked(now time.Time) {
	for checksum, entry := range cache.entries {
		if now.Sub(entry.lastUsed) >= cache.ttl {
			_ = os.Remove(entry.path)
			delete(cache.entries, checksum)
		}
	}
}

func (cache *Cache) evictLRULocked(protectedChecksum string) {
	for cache.totalBytesLocked() > cache.maxBytes {
		var oldestChecksum string
		var oldest *cacheEntry
		for checksum, entry := range cache.entries {
			if checksum == protectedChecksum {
				continue
			}
			if oldest == nil || entry.lastUsed.Before(oldest.lastUsed) {
				oldestChecksum, oldest = checksum, entry
			}
		}
		if oldest == nil {
			return
		}
		_ = os.Remove(oldest.path)
		delete(cache.entries, oldestChecksum)
	}
}

func (cache *Cache) totalBytesLocked() int64 {
	var total int64
	for _, entry := range cache.entries {
		total += entry.size
	}
	return total
}
