package external

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"strings"
	"testing"
)

func TestCanonicalCallbackDestinationNormalizesTheCompleteURL(t *testing.T) {
	destination, err := CanonicalCallbackDestination("HTTPS://OJ.Example.com:443/a/../hooks?b=2&a=1")
	if err != nil {
		t.Fatal(err)
	}
	if destination.URL != "https://oj.example.com:443/hooks?a=1&b=2" || destination.Host != "oj.example.com" || destination.Port != 443 {
		t.Fatalf("destination = %+v", destination)
	}

	defaultPort, err := CanonicalCallbackDestination("https://oj.example.com")
	if err != nil {
		t.Fatal(err)
	}
	if defaultPort.URL != "https://oj.example.com:443/" || defaultPort.Port != 443 {
		t.Fatalf("default destination = %+v", defaultPort)
	}
}

func TestCanonicalCallbackDestinationPreservesEscapedPathAndCanonicalizesQuery(t *testing.T) {
	destination, err := CanonicalCallbackDestination("https://oj.example.com/a%2Fb/../hook?tag=z&tag=a&message=%E4%B8%AD")
	if err != nil {
		t.Fatal(err)
	}
	if destination.URL != "https://oj.example.com:443/hook?message=%E4%B8%AD&tag=a&tag=z" {
		t.Fatalf("destination URL = %q", destination.URL)
	}
}

func TestCanonicalCallbackDestinationRejectsUnsafeSyntax(t *testing.T) {
	for name, raw := range map[string]string{
		"http":          "http://oj.example.com/hook",
		"userinfo":      "https://user@oj.example.com/hook",
		"fragment":      "https://oj.example.com/hook#token",
		"IPv4":          "https://203.0.113.10/hook",
		"IPv6":          "https://[2001:4860:4860::8888]/hook",
		"invalid port":  "https://oj.example.com:0/hook",
		"invalid query": "https://oj.example.com/hook?a=1;b=2",
		"trailing dot":  "https://oj.example.com./hook",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := CanonicalCallbackDestination(raw); err == nil {
				t.Fatalf("destination %q unexpectedly accepted", raw)
			}
		})
	}
}

func TestCallbackCipherBindsTenantCallbackAndCompleteCanonicalDestination(t *testing.T) {
	tenantID := "ceirceirceirceirceirceirce"
	callbackID := "deirceirceirceirceirceirce"
	key := bytes.Repeat([]byte{0x42}, 32)
	cipher, err := NewCallbackCipher(2, map[uint16][]byte{2: key}, bytes.NewReader(bytes.Repeat([]byte{0x24}, 64)))
	if err != nil {
		t.Fatal(err)
	}
	secret := []byte("croj_whsec_0123456789012345678901234567890123456789012")
	encrypted, err := cipher.Encrypt(tenantID, callbackID, "https://oj.example.com:443/hooks?a=1", secret)
	if err != nil {
		t.Fatal(err)
	}
	if encrypted.KeyVersion != 2 || len(encrypted.Nonce) != 12 || bytes.Contains(encrypted.Ciphertext, secret) {
		t.Fatalf("encrypted metadata = %+v", encrypted)
	}
	plaintext, err := cipher.Decrypt(tenantID, callbackID, "https://oj.example.com:443/hooks?a=1", encrypted)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(plaintext, secret) {
		t.Fatalf("plaintext = %q", plaintext)
	}
	clear(plaintext)

	for name, changed := range map[string]struct {
		tenant, callback, destination string
	}{
		"tenant":        {"eeirceirceirceirceirceirce", callbackID, "https://oj.example.com:443/hooks?a=1"},
		"callback":      {tenantID, "feirceirceirceirceirceirce", "https://oj.example.com:443/hooks?a=1"},
		"path":          {tenantID, callbackID, "https://oj.example.com:443/other?a=1"},
		"query":         {tenantID, callbackID, "https://oj.example.com:443/hooks?a=2"},
		"effectivePort": {tenantID, callbackID, "https://oj.example.com:444/hooks?a=1"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := cipher.Decrypt(changed.tenant, changed.callback, changed.destination, encrypted); err == nil {
				t.Fatal("AAD transplant decrypted")
			}
		})
	}
}

func TestCallbackCipherDecryptsHistoricalKeyVersion(t *testing.T) {
	keys := map[uint16][]byte{1: bytes.Repeat([]byte{1}, 32), 2: bytes.Repeat([]byte{2}, 32)}
	oldCipher, err := NewCallbackCipher(1, keys, bytes.NewReader(bytes.Repeat([]byte{3}, 32)))
	if err != nil {
		t.Fatal(err)
	}
	encrypted, err := oldCipher.Encrypt("ceirceirceirceirceirceirce", "deirceirceirceirceirceirce", "https://oj.example.com:443/hook", []byte(strings.Repeat("s", 32)))
	if err != nil {
		t.Fatal(err)
	}
	rotated, err := NewCallbackCipher(2, keys, bytes.NewReader(bytes.Repeat([]byte{4}, 32)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rotated.Decrypt("ceirceirceirceirceirceirce", "deirceirceirceirceirceirce", "https://oj.example.com:443/hook", encrypted); err != nil {
		t.Fatalf("decrypt historical version: %v", err)
	}
	delete(keys, 1)
	withoutOld, err := NewCallbackCipher(2, keys, bytes.NewReader(bytes.Repeat([]byte{5}, 32)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := withoutOld.Decrypt("ceirceirceirceirceirceirce", "deirceirceirceirceirceirce", "https://oj.example.com:443/hook", encrypted); err == nil {
		t.Fatal("missing historical key unexpectedly decrypted")
	}
}

func TestCallbackSecretTypesAlwaysRedactFormatting(t *testing.T) {
	material := CallbackMaterial{CallbackID: "ceirceirceirceirceirceirce", Secret: "croj_whsec_do-not-print"}
	encrypted := EncryptedCallbackSecret{Ciphertext: []byte("ciphertext"), Nonce: []byte("nonce"), KeyVersion: 1}
	for _, formatted := range []string{fmt.Sprint(material), fmt.Sprintf("%#v", material), fmt.Sprint(encrypted), fmt.Sprintf("%#v", encrypted)} {
		if strings.Contains(formatted, "do-not-print") || strings.Contains(formatted, "ciphertext") || strings.Contains(formatted, "nonce") {
			t.Fatalf("secret formatting leaked: %q", formatted)
		}
	}
}

func TestDecodeCallbackKeyRingRejectsAmbiguousOrInvalidConfiguration(t *testing.T) {
	key := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x77}, 32))
	cipher, err := DecodeCallbackKeyRing("2", `{"1":"`+key+`","2":"`+key+`"}`, bytes.NewReader(bytes.Repeat([]byte{8}, 32)))
	if err != nil || cipher == nil {
		t.Fatalf("decode valid ring: cipher=%v error=%v", cipher, err)
	}
	for name, input := range map[string]struct{ active, ring string }{
		"zero active":       {"0", `{"1":"` + key + `"}`},
		"missing active":    {"2", `{"1":"` + key + `"}`},
		"duplicate version": {"1", `{"1":"` + key + `","1":"` + key + `"}`},
		"short key":         {"1", `{"1":"` + base64.StdEncoding.EncodeToString([]byte("short")) + `"}`},
		"trailing data":     {"1", `{"1":"` + key + `"} true`},
		"array":             {"1", `[]`},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := DecodeCallbackKeyRing(input.active, input.ring, bytes.NewReader(bytes.Repeat([]byte{9}, 32))); err == nil || strings.Contains(err.Error(), key) {
				t.Fatalf("error = %v", err)
			}
		})
	}
}
