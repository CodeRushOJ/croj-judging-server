package bundle

import (
	"archive/zip"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestOpenArchiveReadsValidatedCases(t *testing.T) {
	path := writeZIP(t, []zipEntry{
		{name: "manifest.json", body: validManifest},
		{name: "cases/01.in", body: "2 3\n"},
		{name: "cases/01.out", body: "5\n"},
	})
	artifact, err := OpenArchive(path, []byte(validManifest), DefaultArchiveLimits())
	if err != nil {
		t.Fatalf("OpenArchive: %v", err)
	}
	defer artifact.Close()
	input, output, err := artifact.ReadCase(artifact.Manifest().Cases[0])
	if err != nil {
		t.Fatal(err)
	}
	if input != "2 3\n" || output != "5\n" {
		t.Fatalf("case contents = %q %q", input, output)
	}
}

func TestOpenArchiveRejectsUnsafeZIPs(t *testing.T) {
	differentManifest := strings.Replace(validManifest, `"checker":"exact"`, `"checker":"token"`, 1)
	tests := map[string][]zipEntry{
		"missing manifest": {
			{name: "cases/01.in", body: "x"}, {name: "cases/01.out", body: "x"},
		},
		"manifest disagreement": {
			{name: "manifest.json", body: differentManifest}, {name: "cases/01.in", body: "x"}, {name: "cases/01.out", body: "x"},
		},
		"traversal": {
			{name: "manifest.json", body: validManifest}, {name: "../01.in", body: "x"}, {name: "cases/01.in", body: "x"}, {name: "cases/01.out", body: "x"},
		},
		"duplicate": {
			{name: "manifest.json", body: validManifest}, {name: "cases/01.in", body: "x"}, {name: "cases/01.in", body: "x"}, {name: "cases/01.out", body: "x"},
		},
		"symlink": {
			{name: "manifest.json", body: validManifest}, {name: "cases/01.in", body: "target", mode: os.ModeSymlink | 0o777}, {name: "cases/01.out", body: "x"},
		},
		"unreferenced file": {
			{name: "manifest.json", body: validManifest}, {name: "cases/01.in", body: "x"}, {name: "cases/01.out", body: "x"}, {name: "secret", body: "x"},
		},
	}
	for name, entries := range tests {
		t.Run(name, func(t *testing.T) {
			path := writeZIP(t, entries)
			if artifact, err := OpenArchive(path, []byte(validManifest), DefaultArchiveLimits()); err == nil {
				artifact.Close()
				t.Fatal("expected unsafe ZIP error")
			}
		})
	}
}

func TestOpenArchiveRejectsZipBombAndSizeLimits(t *testing.T) {
	entries := []zipEntry{
		{name: "manifest.json", body: validManifest},
		{name: "cases/01.in", body: strings.Repeat("a", 20_000)},
		{name: "cases/01.out", body: "x"},
	}
	limits := DefaultArchiveLimits()
	limits.MaxCaseBytes = 10_000
	if artifact, err := OpenArchive(writeZIP(t, entries), []byte(validManifest), limits); err == nil {
		artifact.Close()
		t.Fatal("expected per-case size error")
	}

	limits = DefaultArchiveLimits()
	limits.MaxCompressionRatio = 2
	if artifact, err := OpenArchive(writeZIP(t, entries), []byte(validManifest), limits); err == nil {
		artifact.Close()
		t.Fatal("expected compression ratio error")
	}
}

type zipEntry struct {
	name string
	body string
	mode os.FileMode
}

func writeZIP(t *testing.T, entries []zipEntry) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "bundle.zip")
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	writer := zip.NewWriter(file)
	for _, entry := range entries {
		header := &zip.FileHeader{Name: entry.name, Method: zip.Deflate}
		mode := entry.mode
		if mode == 0 {
			mode = 0o600
		}
		header.SetMode(mode)
		part, err := writer.CreateHeader(header)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := part.Write([]byte(entry.body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}
