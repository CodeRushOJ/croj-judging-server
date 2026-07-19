package external

import "github.com/CodeRushOJ/croj-judging-server/internal/judgecontract"

// LanguageDefinition is the immutable external-to-Sandbox language contract.
// PublicID is intentionally identical to SandboxID for v1 so stored jobs can be
// executed without a second, drifting translation table.
type LanguageDefinition = judgecontract.LanguageDefinition

// CanonicalLanguages returns a defensive copy of the language registry.
func CanonicalLanguages() []LanguageDefinition {
	return judgecontract.CanonicalLanguages()
}

// ResolveLanguage validates an external v1 identifier and returns the exact
// identifier accepted by the Sandbox compile-once API.
func ResolveLanguage(publicID string) (LanguageDefinition, bool) {
	return judgecontract.ResolveLanguage(publicID)
}

func CanonicalCheckers() []string {
	checkers := judgecontract.CanonicalCheckers()
	result := make([]string, 0, len(checkers))
	for _, checker := range checkers {
		result = append(result, string(checker))
	}
	return result
}

func IsCanonicalChecker(value string) bool {
	return judgecontract.IsCanonicalChecker(judgecontract.Checker(value))
}
