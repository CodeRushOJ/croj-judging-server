package external

// LanguageDefinition is the immutable external-to-Sandbox language contract.
// PublicID is intentionally identical to SandboxID for v1 so stored jobs can be
// executed without a second, drifting translation table.
type LanguageDefinition struct {
	PublicID    string
	SandboxID   string
	DisplayName string
	Runtime     string
}

var canonicalLanguages = [...]LanguageDefinition{
	{PublicID: "go", SandboxID: "go", DisplayName: "Go", Runtime: "go"},
	{PublicID: "cpp", SandboxID: "cpp", DisplayName: "C++ 20", Runtime: "gcc"},
	{PublicID: "python", SandboxID: "python", DisplayName: "Python 3", Runtime: "python3"},
	{PublicID: "java", SandboxID: "java", DisplayName: "Java", Runtime: "java"},
	{PublicID: "javascript", SandboxID: "javascript", DisplayName: "JavaScript", Runtime: "node"},
}

var canonicalCheckers = [...]string{"exact", "token"}

// CanonicalLanguages returns a defensive copy of the language registry.
func CanonicalLanguages() []LanguageDefinition {
	return append([]LanguageDefinition(nil), canonicalLanguages[:]...)
}

// ResolveLanguage validates an external v1 identifier and returns the exact
// identifier accepted by the Sandbox compile-once API.
func ResolveLanguage(publicID string) (LanguageDefinition, bool) {
	for _, language := range canonicalLanguages {
		if language.PublicID == publicID {
			return language, true
		}
	}
	return LanguageDefinition{}, false
}

func CanonicalCheckers() []string {
	return append([]string(nil), canonicalCheckers[:]...)
}

func IsCanonicalChecker(value string) bool {
	for _, checker := range canonicalCheckers {
		if checker == value {
			return true
		}
	}
	return false
}
