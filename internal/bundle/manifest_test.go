package bundle

import (
	"crypto/sha256"
	"encoding/hex"
	"reflect"
	"testing"
)

const validManifest = `{"schemaVersion":1,"judgeMode":"ACM","checker":"exact","limits":{"timeLimitMillis":1500,"memoryLimitMiB":256},"cases":[{"id":"case-01","input":"cases/01.in","output":"cases/01.out","weight":1}]}`
const validOIManifestV2 = `{"schemaVersion":2,"judgeMode":"OI","checker":"token","limits":{"timeLimitMillis":1500,"memoryLimitMiB":256},"totalScore":100,"cases":[{"id":"case-01","input":"cases/01.in","output":"cases/01.out","weight":30},{"id":"case-02","input":"cases/02.in","output":"cases/02.out","weight":70}]}`

func TestParseManifestV1(t *testing.T) {
	manifest, err := ParseManifest([]byte(validManifest))
	if err != nil {
		t.Fatalf("ParseManifest: %v", err)
	}
	if manifest.SchemaVersion != 1 || manifest.JudgeMode != JudgeModeACM || manifest.Checker != CheckerExact || len(manifest.Cases) != 1 {
		t.Fatalf("manifest = %+v", manifest)
	}
	if manifest.Limits.TimeLimitMillis != 1500 || manifest.Limits.MemoryLimitMiB != 256 {
		t.Fatalf("limits = %+v", manifest.Limits)
	}
}

func TestParseManifestV2OI(t *testing.T) {
	manifest, err := ParseManifest([]byte(validOIManifestV2))
	if err != nil {
		t.Fatalf("ParseManifest: %v", err)
	}
	if manifest.SchemaVersion != 2 || manifest.JudgeMode != JudgeModeOI ||
		manifest.Checker != CheckerToken || manifest.TotalScore == nil || *manifest.TotalScore != 100 {
		t.Fatalf("manifest = %+v", manifest)
	}
	if manifest.Cases[0].Weight != 30 || manifest.Cases[1].Weight != 70 {
		t.Fatalf("case weights = %+v", manifest.Cases)
	}
}

func TestParseManifestV2SpecialJudge(t *testing.T) {
	source := []byte("package main")
	digest := sha256.Sum256(source)
	body := `{"schemaVersion":2,"judgeMode":"ACM","checker":"special","limits":{"timeLimitMillis":1500,"memoryLimitMiB":256},"specialJudge":{"language":"go","source":"checker/main.go","sourceSha256":"` +
		hex.EncodeToString(digest[:]) +
		`","timeLimitMillis":2000,"memoryLimitMiB":128},"cases":[{"id":"case-01","input":"cases/01.in","output":"cases/01.out","weight":1}]}`
	manifest, err := ParseManifest([]byte(body))
	if err != nil {
		t.Fatalf("ParseManifest: %v", err)
	}
	if manifest.SpecialJudge == nil || manifest.SpecialJudge.Language != "go" ||
		manifest.SpecialJudge.Source != "checker/main.go" ||
		manifest.SpecialJudge.SourceSHA256 != hex.EncodeToString(digest[:]) {
		t.Fatalf("special judge = %+v", manifest.SpecialJudge)
	}
}

func TestParseManifestRejectsInvalidContract(t *testing.T) {
	tests := map[string]string{
		"missing limits": `{"schemaVersion":1,"judgeMode":"ACM","checker":"exact","cases":[{"id":"a","input":"a.in","output":"a.out","weight":1}]}`,
		"zero time":      `{"schemaVersion":1,"judgeMode":"ACM","checker":"exact","limits":{"timeLimitMillis":0,"memoryLimitMiB":256},"cases":[{"id":"a","input":"a.in","output":"a.out","weight":1}]}`,
		"zero memory":    `{"schemaVersion":1,"judgeMode":"ACM","checker":"exact","limits":{"timeLimitMillis":1000,"memoryLimitMiB":0},"cases":[{"id":"a","input":"a.in","output":"a.out","weight":1}]}`,
		"unknown field":  `{"schemaVersion":1,"judgeMode":"ACM","checker":"exact","cases":[],"secret":"x"}`,
		"unsupported v3": `{"schemaVersion":3,"judgeMode":"ACM","checker":"exact","cases":[{"id":"a","input":"a.in","output":"a.out","weight":1}]}`,
		"empty cases":    `{"schemaVersion":1,"judgeMode":"ACM","checker":"exact","cases":[]}`,
		"duplicate id":   `{"schemaVersion":1,"judgeMode":"ACM","checker":"exact","cases":[{"id":"a","input":"a.in","output":"a.out","weight":1},{"id":"a","input":"b.in","output":"b.out","weight":1}]}`,
		"duplicate path": `{"schemaVersion":1,"judgeMode":"ACM","checker":"exact","cases":[{"id":"a","input":"a.in","output":"a.out","weight":1},{"id":"b","input":"a.in","output":"b.out","weight":1}]}`,
		"traversal":      `{"schemaVersion":1,"judgeMode":"ACM","checker":"exact","cases":[{"id":"a","input":"../a.in","output":"a.out","weight":1}]}`,
		"backslash":      `{"schemaVersion":1,"judgeMode":"ACM","checker":"exact","cases":[{"id":"a","input":"cases\\a.in","output":"a.out","weight":1}]}`,
		"invalid weight": `{"schemaVersion":1,"judgeMode":"ACM","checker":"exact","cases":[{"id":"a","input":"a.in","output":"a.out","weight":2}]}`,
		"special judge":  `{"schemaVersion":1,"judgeMode":"ACM","checker":"spj","cases":[{"id":"a","input":"a.in","output":"a.out","weight":1}]}`,
		"OI":             `{"schemaVersion":1,"judgeMode":"OI","checker":"exact","cases":[{"id":"a","input":"a.in","output":"a.out","weight":1}]}`,
	}
	for name, body := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseManifest([]byte(body)); err == nil {
				t.Fatal("expected manifest error")
			}
		})
	}
}

func TestParseManifestRejectsInvalidV2ScoringAndCheckerContracts(t *testing.T) {
	validSpecial := `"specialJudge":{"language":"go","source":"checker/main.go","sourceSha256":"0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef","timeLimitMillis":2000,"memoryLimitMiB":128}`
	tests := map[string]string{
		"OI missing total":          `{"schemaVersion":2,"judgeMode":"OI","checker":"exact","limits":{"timeLimitMillis":1000,"memoryLimitMiB":64},"cases":[{"id":"a","input":"a.in","output":"a.out","weight":1}]}`,
		"OI score mismatch":         `{"schemaVersion":2,"judgeMode":"OI","checker":"exact","limits":{"timeLimitMillis":1000,"memoryLimitMiB":64},"totalScore":100,"cases":[{"id":"a","input":"a.in","output":"a.out","weight":99}]}`,
		"OI zero weight":            `{"schemaVersion":2,"judgeMode":"OI","checker":"exact","limits":{"timeLimitMillis":1000,"memoryLimitMiB":64},"totalScore":100,"cases":[{"id":"a","input":"a.in","output":"a.out","weight":0},{"id":"b","input":"b.in","output":"b.out","weight":100}]}`,
		"ACM total score":           `{"schemaVersion":2,"judgeMode":"ACM","checker":"exact","limits":{"timeLimitMillis":1000,"memoryLimitMiB":64},"totalScore":100,"cases":[{"id":"a","input":"a.in","output":"a.out","weight":1}]}`,
		"ACM non-unit weight":       `{"schemaVersion":2,"judgeMode":"ACM","checker":"exact","limits":{"timeLimitMillis":1000,"memoryLimitMiB":64},"cases":[{"id":"a","input":"a.in","output":"a.out","weight":2}]}`,
		"v1 OI remains closed":      `{"schemaVersion":1,"judgeMode":"OI","checker":"exact","limits":{"timeLimitMillis":1000,"memoryLimitMiB":64},"totalScore":1,"cases":[{"id":"a","input":"a.in","output":"a.out","weight":1}]}`,
		"v1 special remains closed": `{"schemaVersion":1,"judgeMode":"ACM","checker":"special","limits":{"timeLimitMillis":1000,"memoryLimitMiB":64},` + validSpecial + `,"cases":[{"id":"a","input":"a.in","output":"a.out","weight":1}]}`,
		"special missing config":    `{"schemaVersion":2,"judgeMode":"ACM","checker":"special","limits":{"timeLimitMillis":1000,"memoryLimitMiB":64},"cases":[{"id":"a","input":"a.in","output":"a.out","weight":1}]}`,
		"config for exact":          `{"schemaVersion":2,"judgeMode":"ACM","checker":"exact","limits":{"timeLimitMillis":1000,"memoryLimitMiB":64},` + validSpecial + `,"cases":[{"id":"a","input":"a.in","output":"a.out","weight":1}]}`,
		"unknown language":          `{"schemaVersion":2,"judgeMode":"ACM","checker":"special","limits":{"timeLimitMillis":1000,"memoryLimitMiB":64},"specialJudge":{"language":"ruby","source":"checker/main.rb","sourceSha256":"0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef","timeLimitMillis":2000,"memoryLimitMiB":128},"cases":[{"id":"a","input":"a.in","output":"a.out","weight":1}]}`,
		"bad source digest":         `{"schemaVersion":2,"judgeMode":"ACM","checker":"special","limits":{"timeLimitMillis":1000,"memoryLimitMiB":64},"specialJudge":{"language":"go","source":"checker/main.go","sourceSha256":"abc","timeLimitMillis":2000,"memoryLimitMiB":128},"cases":[{"id":"a","input":"a.in","output":"a.out","weight":1}]}`,
		"source duplicates case":    `{"schemaVersion":2,"judgeMode":"ACM","checker":"special","limits":{"timeLimitMillis":1000,"memoryLimitMiB":64},"specialJudge":{"language":"go","source":"a.in","sourceSha256":"0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef","timeLimitMillis":2000,"memoryLimitMiB":128},"cases":[{"id":"a","input":"a.in","output":"a.out","weight":1}]}`,
		"checker limit too high":    `{"schemaVersion":2,"judgeMode":"ACM","checker":"special","limits":{"timeLimitMillis":1000,"memoryLimitMiB":64},"specialJudge":{"language":"go","source":"checker/main.go","sourceSha256":"0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef","timeLimitMillis":86400001,"memoryLimitMiB":128},"cases":[{"id":"a","input":"a.in","output":"a.out","weight":1}]}`,
	}
	for name, body := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseManifest([]byte(body)); err == nil {
				t.Fatal("expected manifest error")
			}
		})
	}
}

func TestManifestsMustHaveEqualNormalizedStructure(t *testing.T) {
	databaseManifest, err := ParseManifest([]byte("\n " + validManifest + " \n"))
	if err != nil {
		t.Fatal(err)
	}
	artifactManifest, err := ParseManifest([]byte(`{
      "checker":"exact",
	  "limits":{"memoryLimitMiB":256,"timeLimitMillis":1500},
      "cases":[{"weight":1,"output":"cases/01.out","input":"cases/01.in","id":"case-01"}],
      "judgeMode":"ACM",
      "schemaVersion":1
    }`))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(databaseManifest, artifactManifest) {
		t.Fatalf("normalized manifests differ: %+v %+v", databaseManifest, artifactManifest)
	}
}
