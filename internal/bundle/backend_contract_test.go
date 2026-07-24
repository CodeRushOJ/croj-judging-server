package bundle

import (
	"os"
	"path/filepath"
	"testing"
)

func TestOpenArchiveAcceptsBackendManifestV1GoldenFixture(t *testing.T) {
	archivePath := os.Getenv("CROJ_BACKEND_TEST_BUNDLE_V1")
	var manifest, input, output string
	if archivePath == "" {
		root := filepath.Join("testdata", "backend-v1")
		manifest = readBackendFixture(t, root, "manifest.json")
		input = readBackendFixture(t, root, "cases", "1.in")
		output = readBackendFixture(t, root, "cases", "1.out")
		archivePath = writeZIP(t, []zipEntry{
			{name: "manifest.json", body: manifest},
			{name: "cases/1.in", body: input},
			{name: "cases/1.out", body: output},
		})
	} else {
		if !filepath.IsAbs(archivePath) {
			t.Fatalf("CROJ_BACKEND_TEST_BUNDLE_V1 must be an absolute path, got %q", archivePath)
		}
		_, externalManifest, err := InspectArchive(archivePath, DefaultArchiveLimits())
		if err != nil {
			t.Fatalf("inspect CROJ_BACKEND_TEST_BUNDLE_V1 without rewriting it: %v", err)
		}
		manifest = string(externalManifest)
		input = "1 2"
		output = "3"
	}

	artifact, err := OpenArchive(archivePath, []byte(manifest), DefaultArchiveLimits())
	if err != nil {
		t.Fatalf("OpenArchive backend v1 fixture: %v", err)
	}
	defer artifact.Close()
	parsed := artifact.Manifest()
	if parsed.SchemaVersion != 1 ||
		parsed.JudgeMode != JudgeModeACM ||
		parsed.Checker != CheckerExact ||
		parsed.Limits.TimeLimitMillis != 1000 ||
		parsed.Limits.MemoryLimitMiB != 64 {
		t.Fatalf("backend v1 manifest changed: %+v", parsed)
	}
	if got := parsed.Cases[0].ID; got != "1" {
		t.Fatalf("case ID = %q, want 1", got)
	}
	gotInput, gotOutput, err := artifact.ReadCase(parsed.Cases[0])
	if err != nil {
		t.Fatalf("ReadCase backend v1 fixture: %v", err)
	}
	if gotInput != input || gotOutput != output {
		t.Fatalf("fixture contents changed: input=%q output=%q", gotInput, gotOutput)
	}
}

func readBackendFixture(t *testing.T, elements ...string) string {
	t.Helper()
	value, err := os.ReadFile(filepath.Join(elements...))
	if err != nil {
		t.Fatal(err)
	}
	return string(value)
}
