package httpapi

import (
	"fmt"
	"math"
	"regexp"

	"github.com/CodeRushOJ/croj-judging-server/internal/judgecontract"
)

const (
	maximumV1CaseCount                 = 256
	maximumJobRequestEncodingExpansion = int64(6)
	maximumJobRequestEnvelopeBytes     = int64(64 << 10)
	maximumV1SourceBytes               = (math.MaxInt64 - maximumJobRequestEnvelopeBytes) / maximumJobRequestEncodingExpansion
)

var capabilityLanguageIDPattern = regexp.MustCompile(`^[a-z][a-z0-9._-]{1,31}$`)

type LanguageCapability struct {
	ID          string `json:"id"`
	DisplayName string `json:"displayName"`
	Runtime     string `json:"runtime"`
}

type CapabilityLimits struct {
	MaxSourceBytes     int64 `json:"maxSourceBytes"`
	MaxBundleBytes     int64 `json:"maxBundleBytes"`
	MaxCaseBytes       int64 `json:"maxCaseBytes"`
	MaxCaseCount       int   `json:"maxCaseCount"`
	MaxTimeLimitMillis int   `json:"maxTimeLimitMillis"`
	MaxMemoryLimitMiB  int   `json:"maxMemoryLimitMiB"`
}

type Capabilities struct {
	APIVersion string               `json:"apiVersion"`
	Languages  []LanguageCapability `json:"languages"`
	JudgeModes []string             `json:"judgeModes"`
	Checkers   []string             `json:"checkers"`
	Limits     CapabilityLimits     `json:"limits"`
}

func normalizeCapabilities(value Capabilities) (Capabilities, error) {
	if value.APIVersion != "v1" || len(value.Languages) == 0 ||
		value.Limits.MaxSourceBytes <= 0 || value.Limits.MaxSourceBytes > maximumV1SourceBytes ||
		value.Limits.MaxBundleBytes <= 0 || value.Limits.MaxCaseBytes <= 0 ||
		value.Limits.MaxCaseCount <= 0 || value.Limits.MaxCaseCount > maximumV1CaseCount ||
		value.Limits.MaxTimeLimitMillis <= 0 || value.Limits.MaxMemoryLimitMiB <= 0 {
		return Capabilities{}, fmt.Errorf("complete v1 capabilities and at least one language are required")
	}
	for _, language := range value.Languages {
		if !capabilityLanguageIDPattern.MatchString(language.ID) || language.DisplayName == "" || language.Runtime == "" {
			return Capabilities{}, fmt.Errorf("language capabilities must contain a valid ID, display name, and runtime")
		}
		definition, ok := judgecontract.ResolveLanguage(language.ID)
		if !ok || definition.DisplayName != language.DisplayName || definition.Runtime != language.Runtime {
			return Capabilities{}, fmt.Errorf("language capabilities must match the canonical Sandbox registry")
		}
	}
	for _, checker := range value.Checkers {
		if !judgecontract.IsCanonicalChecker(judgecontract.Checker(checker)) {
			return Capabilities{}, fmt.Errorf("checker capabilities must match bundle manifest identifiers")
		}
	}
	value.Languages = append([]LanguageCapability(nil), value.Languages...)
	value.JudgeModes = append([]string{}, value.JudgeModes...)
	value.Checkers = append([]string{}, value.Checkers...)
	return value, nil
}
