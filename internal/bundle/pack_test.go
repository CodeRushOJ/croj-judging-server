package bundle

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"testing"
)

func TestWriteDeterministicArchiveProducesStableBytes(t *testing.T) {
	manifest, err := ParseManifest([]byte(validManifest))
	if err != nil {
		t.Fatal(err)
	}
	firstFiles := map[string][]byte{"cases/01.out": []byte("out\n"), "cases/01.in": []byte("in\n")}
	secondFiles := map[string][]byte{"cases/01.in": []byte("in\n"), "cases/01.out": []byte("out\n")}
	var first, second bytes.Buffer
	firstManifest, err := WriteDeterministicArchive(&first, manifest, firstFiles)
	if err != nil {
		t.Fatal(err)
	}
	secondManifest, err := WriteDeterministicArchive(&second, manifest, secondFiles)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first.Bytes(), second.Bytes()) || !bytes.Equal(firstManifest, secondManifest) {
		t.Fatal("archive bytes changed with map insertion order")
	}
	path := t.TempDir() + "/bundle.zip"
	if err := os.WriteFile(path, first.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	artifact, err := OpenArchive(path, firstManifest, DefaultArchiveLimits())
	if err != nil {
		t.Fatalf("generated archive is invalid: %v", err)
	}
	artifact.Close()
}

func TestWriteDeterministicArchiveIncludesSpecialJudgeSource(t *testing.T) {
	source := []byte("package main\n")
	digest := sha256.Sum256(source)
	manifest := Manifest{
		SchemaVersion: 2,
		JudgeMode:     JudgeModeACM,
		Checker:       CheckerSpecial,
		Limits:        Limits{TimeLimitMillis: 1000, MemoryLimitMiB: 64},
		SpecialJudge: &SpecialJudge{
			Language: "go", Source: "checker/main.go", SourceSHA256: hex.EncodeToString(digest[:]),
			TimeLimitMillis: 2000, MemoryLimitMiB: 128,
		},
		Cases: []Case{{ID: "case-1", Input: "case-1.in", Output: "case-1.out", Weight: 1}},
	}
	files := map[string][]byte{
		"case-1.in":       []byte("input\n"),
		"case-1.out":      []byte("expected\n"),
		"checker/main.go": source,
	}
	var archive bytes.Buffer
	canonical, err := WriteDeterministicArchive(&archive, manifest, files)
	if err != nil {
		t.Fatalf("write SPJ bundle: %v", err)
	}
	path := t.TempDir() + "/spj.zip"
	if err := os.WriteFile(path, archive.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	artifact, err := OpenArchive(path, canonical, DefaultArchiveLimits())
	if err != nil {
		t.Fatalf("open generated SPJ bundle: %v", err)
	}
	defer artifact.Close()
	if got, err := artifact.ReadSpecialJudge(); err != nil || got != string(source) {
		t.Fatalf("special judge source = %q, %v", got, err)
	}
}
