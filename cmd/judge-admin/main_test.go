package main

import (
	"bytes"
	"encoding/base64"
	"testing"
)

func TestCallbackProvisionerOptionsRequireKeysOnlyForCallbackCreate(t *testing.T) {
	getenv := func(string) string { return "" }
	options, err := callbackProvisionerOptions([]string{"tenant", "create"}, getenv, bytes.NewReader(make([]byte, 32)))
	if err != nil || len(options) != 0 {
		t.Fatalf("tenant options=%d error=%v", len(options), err)
	}
	if _, err := callbackProvisionerOptions([]string{"callback", "create"}, getenv, bytes.NewReader(make([]byte, 32))); err == nil {
		t.Fatal("callback create accepted missing key configuration")
	}
}

func TestCallbackProvisionerOptionsDecodeTheActiveKeyRing(t *testing.T) {
	key := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x44}, 32))
	values := map[string]string{
		"JUDGE_CALLBACK_KEY_VERSION": "3",
		"JUDGE_CALLBACK_KEYS_JSON":   `{"3":"` + key + `"}`,
	}
	options, err := callbackProvisionerOptions([]string{"callback", "create"}, func(name string) string { return values[name] }, bytes.NewReader(bytes.Repeat([]byte{1}, 32)))
	if err != nil || len(options) != 1 {
		t.Fatalf("callback options=%d error=%v", len(options), err)
	}
}

func TestSchemaMigrateIsTheOnlyMigrationOnlyCommand(t *testing.T) {
	if !migrationOnly([]string{"schema", "migrate"}) {
		t.Fatal("schema migrate was not recognized")
	}
	for _, arguments := range [][]string{{"tenant", "create"}, {"schema", "migrate", "extra"}, {"migrate"}} {
		if migrationOnly(arguments) {
			t.Fatalf("unexpected migration-only command: %v", arguments)
		}
	}
}
