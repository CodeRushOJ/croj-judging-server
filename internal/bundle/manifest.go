package bundle

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"path"
	"regexp"
	"strings"
	"unicode/utf8"
)

type JudgeMode string
type Checker string

const (
	JudgeModeACM JudgeMode = "ACM"
	CheckerExact Checker   = "exact"
	CheckerToken Checker   = "token"
	maxCases               = 10_000
)

var caseIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

type Manifest struct {
	SchemaVersion int       `json:"schemaVersion"`
	JudgeMode     JudgeMode `json:"judgeMode"`
	Checker       Checker   `json:"checker"`
	Limits        Limits    `json:"limits"`
	Cases         []Case    `json:"cases"`
}

type Limits struct {
	TimeLimitMillis int `json:"timeLimitMillis"`
	MemoryLimitMiB  int `json:"memoryLimitMiB"`
}

type Case struct {
	ID     string `json:"id"`
	Input  string `json:"input"`
	Output string `json:"output"`
	Weight int    `json:"weight"`
}

func ParseManifest(data []byte) (Manifest, error) {
	var manifest Manifest
	if !utf8.Valid(data) {
		return manifest, fmt.Errorf("manifest must be valid UTF-8")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&manifest); err != nil {
		return manifest, fmt.Errorf("decode manifest: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return manifest, fmt.Errorf("manifest must contain exactly one JSON document")
	}
	if err := manifest.Validate(); err != nil {
		return manifest, err
	}
	return manifest, nil
}

func (manifest Manifest) Validate() error {
	if manifest.SchemaVersion != 1 {
		return fmt.Errorf("unsupported manifest schemaVersion %d", manifest.SchemaVersion)
	}
	if manifest.JudgeMode != JudgeModeACM {
		return fmt.Errorf("unsupported judgeMode %q", manifest.JudgeMode)
	}
	if manifest.Checker != CheckerExact && manifest.Checker != CheckerToken {
		return fmt.Errorf("unsupported checker %q", manifest.Checker)
	}
	if manifest.Limits.TimeLimitMillis <= 0 || manifest.Limits.MemoryLimitMiB <= 0 {
		return fmt.Errorf("manifest execution limits must be positive")
	}
	if len(manifest.Cases) == 0 || len(manifest.Cases) > maxCases {
		return fmt.Errorf("manifest cases must contain 1..%d entries", maxCases)
	}
	ids := make(map[string]struct{}, len(manifest.Cases))
	paths := make(map[string]struct{}, len(manifest.Cases)*2)
	for index, testCase := range manifest.Cases {
		if !caseIDPattern.MatchString(testCase.ID) {
			return fmt.Errorf("case %d has invalid id", index)
		}
		if _, exists := ids[testCase.ID]; exists {
			return fmt.Errorf("duplicate case id %q", testCase.ID)
		}
		ids[testCase.ID] = struct{}{}
		if testCase.Weight != 1 {
			return fmt.Errorf("ACM case %q weight must equal 1", testCase.ID)
		}
		for _, name := range []string{testCase.Input, testCase.Output} {
			if err := validateArtifactPath(name); err != nil {
				return fmt.Errorf("case %q: %w", testCase.ID, err)
			}
			if name == "manifest.json" {
				return fmt.Errorf("case %q references reserved manifest.json", testCase.ID)
			}
			if _, exists := paths[name]; exists {
				return fmt.Errorf("duplicate case path %q", name)
			}
			paths[name] = struct{}{}
		}
	}
	return nil
}

func validateArtifactPath(name string) error {
	if name == "" || len(name) > 512 || !utf8.ValidString(name) || strings.ContainsRune(name, '\x00') || strings.Contains(name, `\`) || strings.HasPrefix(name, "/") {
		return fmt.Errorf("invalid artifact path")
	}
	cleaned := path.Clean(name)
	if cleaned == "." || cleaned != name || strings.HasPrefix(cleaned, "../") {
		return fmt.Errorf("unsafe artifact path %q", name)
	}
	return nil
}
