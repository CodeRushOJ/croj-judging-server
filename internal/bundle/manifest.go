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

	"github.com/CodeRushOJ/croj-judging-server/internal/judgecontract"
)

type JudgeMode string
type Checker = judgecontract.Checker

const (
	JudgeModeACM       JudgeMode = "ACM"
	JudgeModeOI        JudgeMode = "OI"
	CheckerExact                 = judgecontract.CheckerExact
	CheckerToken                 = judgecontract.CheckerToken
	CheckerSpecial               = judgecontract.CheckerSpecial
	maxCases                     = 10_000
	maxTotalScore                = 1_000_000_000
	maxExecutionMillis           = 86_400_000
	maxMemoryMiB                 = 2_147_483_647
)

var (
	caseIDPattern      = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
	lowerSHA256Pattern = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

type Manifest struct {
	SchemaVersion int           `json:"schemaVersion"`
	JudgeMode     JudgeMode     `json:"judgeMode"`
	Checker       Checker       `json:"checker"`
	Limits        Limits        `json:"limits"`
	TotalScore    *int          `json:"totalScore,omitempty"`
	SpecialJudge  *SpecialJudge `json:"specialJudge,omitempty"`
	Cases         []Case        `json:"cases"`
}

type Limits struct {
	TimeLimitMillis int `json:"timeLimitMillis"`
	MemoryLimitMiB  int `json:"memoryLimitMiB"`
}

type SpecialJudge struct {
	Language        string `json:"language"`
	Source          string `json:"source"`
	SourceSHA256    string `json:"sourceSha256"`
	TimeLimitMillis int    `json:"timeLimitMillis"`
	MemoryLimitMiB  int    `json:"memoryLimitMiB"`
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
	if manifest.SchemaVersion != 1 && manifest.SchemaVersion != 2 {
		return fmt.Errorf("unsupported manifest schemaVersion %d", manifest.SchemaVersion)
	}
	if manifest.Limits.TimeLimitMillis <= 0 || manifest.Limits.TimeLimitMillis > maxExecutionMillis ||
		manifest.Limits.MemoryLimitMiB <= 0 || manifest.Limits.MemoryLimitMiB > maxMemoryMiB {
		return fmt.Errorf("manifest execution limits are outside supported bounds")
	}
	if manifest.SchemaVersion == 1 {
		if manifest.JudgeMode != JudgeModeACM || !judgecontract.IsCanonicalChecker(manifest.Checker) ||
			manifest.TotalScore != nil || manifest.SpecialJudge != nil {
			return fmt.Errorf("manifest v1 supports ACM exact/token only")
		}
	} else {
		if manifest.JudgeMode != JudgeModeACM && manifest.JudgeMode != JudgeModeOI {
			return fmt.Errorf("unsupported judgeMode %q", manifest.JudgeMode)
		}
		if !judgecontract.IsCanonicalChecker(manifest.Checker) && manifest.Checker != CheckerSpecial {
			return fmt.Errorf("unsupported checker %q", manifest.Checker)
		}
	}
	if manifest.Checker == CheckerSpecial {
		if manifest.SpecialJudge == nil {
			return fmt.Errorf("special checker requires specialJudge")
		}
		if _, ok := judgecontract.ResolveLanguage(manifest.SpecialJudge.Language); !ok {
			return fmt.Errorf("specialJudge language is unsupported")
		}
		if err := validateArtifactPath(manifest.SpecialJudge.Source); err != nil ||
			manifest.SpecialJudge.Source == "manifest.json" {
			return fmt.Errorf("specialJudge source path is invalid")
		}
		if !lowerSHA256Pattern.MatchString(manifest.SpecialJudge.SourceSHA256) {
			return fmt.Errorf("specialJudge sourceSha256 must be lowercase SHA-256")
		}
		if manifest.SpecialJudge.TimeLimitMillis <= 0 || manifest.SpecialJudge.TimeLimitMillis > maxExecutionMillis ||
			manifest.SpecialJudge.MemoryLimitMiB <= 0 || manifest.SpecialJudge.MemoryLimitMiB > maxMemoryMiB {
			return fmt.Errorf("specialJudge limits are outside supported bounds")
		}
	} else if manifest.SpecialJudge != nil {
		return fmt.Errorf("specialJudge is only valid for the special checker")
	}
	if len(manifest.Cases) == 0 || len(manifest.Cases) > maxCases {
		return fmt.Errorf("manifest cases must contain 1..%d entries", maxCases)
	}
	ids := make(map[string]struct{}, len(manifest.Cases))
	paths := make(map[string]struct{}, len(manifest.Cases)*2)
	if manifest.SpecialJudge != nil {
		paths[manifest.SpecialJudge.Source] = struct{}{}
	}
	var weightSum int64
	for index, testCase := range manifest.Cases {
		if !caseIDPattern.MatchString(testCase.ID) {
			return fmt.Errorf("case %d has invalid id", index)
		}
		if _, exists := ids[testCase.ID]; exists {
			return fmt.Errorf("duplicate case id %q", testCase.ID)
		}
		ids[testCase.ID] = struct{}{}
		if manifest.JudgeMode == JudgeModeACM && testCase.Weight != 1 {
			return fmt.Errorf("ACM case %q weight must equal 1", testCase.ID)
		}
		if manifest.JudgeMode == JudgeModeOI && (testCase.Weight <= 0 || testCase.Weight > maxTotalScore) {
			return fmt.Errorf("OI case %q weight is outside supported bounds", testCase.ID)
		}
		weightSum += int64(testCase.Weight)
		if weightSum > maxTotalScore {
			return fmt.Errorf("OI case weights exceed supported total")
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
	if manifest.JudgeMode == JudgeModeOI {
		if manifest.TotalScore == nil || *manifest.TotalScore <= 0 || *manifest.TotalScore > maxTotalScore ||
			int64(*manifest.TotalScore) != weightSum {
			return fmt.Errorf("OI totalScore must equal the sum of case weights")
		}
	} else if manifest.TotalScore != nil {
		return fmt.Errorf("totalScore is only valid for OI")
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
