package release

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestKinOpenAPIUsesPublishedSecurityFloor(t *testing.T) {
	goMod, err := os.ReadFile(filepath.Join("..", "..", "go.mod"))
	if err != nil {
		t.Fatalf("read go.mod: %v", err)
	}

	const secureVersion = "github.com/getkin/kin-openapi v0.144.0"
	if !strings.Contains(string(goMod), secureVersion) {
		t.Fatalf("go.mod must pin %q", secureVersion)
	}
}
