package bundle

import (
	"bytes"
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
