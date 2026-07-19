package judgecontract_test

import (
	"reflect"
	"testing"

	"github.com/CodeRushOJ/croj-judging-server/internal/judgecontract"
)

func TestCanonicalV1RegistryPublishesTheExactLanguageAndCheckerSets(t *testing.T) {
	languages := judgecontract.CanonicalLanguages()
	gotLanguageIDs := make([]string, 0, len(languages))
	for _, language := range languages {
		gotLanguageIDs = append(gotLanguageIDs, language.PublicID)
		if language.PublicID != language.SandboxID {
			t.Errorf("public language %q maps to Sandbox language %q", language.PublicID, language.SandboxID)
		}
	}
	if want := []string{"go", "cpp", "python", "java", "javascript"}; !reflect.DeepEqual(gotLanguageIDs, want) {
		t.Fatalf("canonical language IDs = %v, want %v", gotLanguageIDs, want)
	}
	if got, want := judgecontract.CanonicalCheckers(), []judgecontract.Checker{judgecontract.CheckerExact, judgecontract.CheckerToken}; !reflect.DeepEqual(got, want) {
		t.Fatalf("canonical checkers = %v, want %v", got, want)
	}

	// Callers receive copies and cannot mutate the immutable protocol registry.
	languages[0].PublicID = "mutated"
	checkers := judgecontract.CanonicalCheckers()
	checkers[0] = "mutated"
	if resolved, ok := judgecontract.ResolveLanguage("go"); !ok || resolved.PublicID != "go" {
		t.Fatalf("language registry was mutated: %+v available=%v", resolved, ok)
	}
	if !judgecontract.IsCanonicalChecker(judgecontract.CheckerExact) {
		t.Fatal("checker registry was mutated")
	}
}
