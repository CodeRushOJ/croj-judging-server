package bundle

import (
	"reflect"
	"testing"
)

const validManifest = `{"schemaVersion":1,"judgeMode":"ACM","checker":"exact","limits":{"timeLimitMillis":1500,"memoryLimitMiB":256},"cases":[{"id":"case-01","input":"cases/01.in","output":"cases/01.out","weight":1}]}`

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

func TestParseManifestRejectsInvalidContract(t *testing.T) {
	tests := map[string]string{
		"missing limits": `{"schemaVersion":1,"judgeMode":"ACM","checker":"exact","cases":[{"id":"a","input":"a.in","output":"a.out","weight":1}]}`,
		"zero time":      `{"schemaVersion":1,"judgeMode":"ACM","checker":"exact","limits":{"timeLimitMillis":0,"memoryLimitMiB":256},"cases":[{"id":"a","input":"a.in","output":"a.out","weight":1}]}`,
		"zero memory":    `{"schemaVersion":1,"judgeMode":"ACM","checker":"exact","limits":{"timeLimitMillis":1000,"memoryLimitMiB":0},"cases":[{"id":"a","input":"a.in","output":"a.out","weight":1}]}`,
		"unknown field":  `{"schemaVersion":1,"judgeMode":"ACM","checker":"exact","cases":[],"secret":"x"}`,
		"unsupported v2": `{"schemaVersion":2,"judgeMode":"ACM","checker":"exact","cases":[{"id":"a","input":"a.in","output":"a.out","weight":1}]}`,
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
