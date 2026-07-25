package release

import (
	"os"
	"strings"
	"testing"
)

func TestAnnotatedTagPublishesOidcAttestedMultiArchitectureImage(t *testing.T) {
	workflowBytes, err := os.ReadFile("../../.github/workflows/ci.yml")
	if err != nil {
		t.Fatal(err)
	}
	workflow := string(workflowBytes)
	for _, required := range []string{
		`tags: ["v*"]`,
		`if: ${{ github.ref_type == 'tag' }}`,
		`packages: write`,
		`id-token: write`,
		`test "$(git rev-parse "$GITHUB_REF_NAME^{commit}")" = "$GITHUB_SHA"`,
		`git fetch --no-tags origin main`,
		`test "$GITHUB_SHA" = "$(git rev-parse origin/main)"`,
		`docker/setup-qemu-action@`,
		`actions/attest-build-provenance@`,
		`subject-name: ghcr.io/coderushoj/croj-judging-server`,
		`subject-digest: ${{ steps.push.outputs.digest }}`,
		`push-to-registry: true`,
		`go run golang.org/x/vuln/cmd/govulncheck@v1.6.0 ./...`,
		`image-ref: coderushoj/judging-server:ci`,
		`severity: HIGH,CRITICAL`,
		`exit-code: "1"`,
		`platforms: linux/amd64,linux/arm64`,
		`ghcr.io/coderushoj/croj-judging-server:${{ github.ref_name }}`,
		`ghcr.io/coderushoj/croj-judging-server:sha-${{ github.sha }}`,
		`provenance: mode=max`,
		`sbom: true`,
		`id: push`,
		`IMAGE_DIGEST: ${{ steps.push.outputs.digest }}`,
		`judging-server-image.json`,
		`name: judging-server-image-${{ github.sha }}`,
	} {
		if !strings.Contains(workflow, required) {
			t.Errorf("release workflow is missing %q", required)
		}
	}
	if strings.Contains(workflow, ".verification.verified") {
		t.Error("release workflow must use keyless OIDC provenance instead of requiring a local tag signing key")
	}
}

func TestBuildUsesPatchedPinnedGoToolchain(t *testing.T) {
	workflowBytes, err := os.ReadFile("../../.github/workflows/ci.yml")
	if err != nil {
		t.Fatal(err)
	}
	dockerfileBytes, err := os.ReadFile("../../Dockerfile")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(workflowBytes), "go-version: 1.26.5") {
		t.Error("CI must use the patched Go 1.26.5 standard library")
	}
	if !strings.Contains(
		string(dockerfileBytes),
		"FROM golang:1.26.5-bookworm@sha256:1ecb7edf62a0408027bd5729dfd6b1b8766e578e8df93995b225dfd0944eb651 AS build",
	) {
		t.Error("production builder must pin the patched multi-architecture Go index")
	}
}
