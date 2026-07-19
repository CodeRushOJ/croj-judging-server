package external

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"strings"
	"testing"
)

func TestSourceCipherEncryptsWithVersionedAESGCMAndTenantBoundAAD(t *testing.T) {
	key := bytes.Repeat([]byte{0x44}, 32)
	cipher, err := NewSourceCipher(7, map[uint16][]byte{7: key}, bytes.NewReader(bytes.Repeat([]byte{0x22}, sourceNonceBytes)))
	if err != nil {
		t.Fatal(err)
	}
	plaintext := []byte("package main\nfunc main() {}")
	encrypted, err := cipher.Encrypt("tenant-7", "src_01", plaintext)
	if err != nil {
		t.Fatal(err)
	}
	if encrypted.KeyVersion != 7 || len(encrypted.Nonce) != sourceNonceBytes || bytes.Contains(encrypted.Ciphertext, plaintext) || encrypted.SizeBytes != int64(len(plaintext)) {
		t.Fatalf("encrypted metadata = %+v", encrypted)
	}
	digest := sha256.Sum256(plaintext)
	if !bytes.Equal(encrypted.SHA256, digest[:]) {
		t.Fatalf("digest = %x", encrypted.SHA256)
	}
	recovered, err := cipher.Decrypt("tenant-7", "src_01", encrypted)
	if err != nil || !bytes.Equal(recovered, plaintext) {
		t.Fatalf("recovered=%q err=%v", recovered, err)
	}
}

func TestSourceCipherRejectsCrossTenantReplayTamperingAndUnknownKeys(t *testing.T) {
	key := bytes.Repeat([]byte{0x44}, 32)
	cipher, err := NewSourceCipher(7, map[uint16][]byte{7: key}, bytes.NewReader(bytes.Repeat([]byte{0x22}, sourceNonceBytes*3)))
	if err != nil {
		t.Fatal(err)
	}
	encrypted, err := cipher.Encrypt("tenant-7", "src_01", []byte("secret source"))
	if err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(EncryptedSource) EncryptedSource{
		"other tenant": func(value EncryptedSource) EncryptedSource { return value },
		"ciphertext": func(value EncryptedSource) EncryptedSource {
			value.Ciphertext = append([]byte(nil), value.Ciphertext...)
			value.Ciphertext[0] ^= 0xff
			return value
		},
		"digest": func(value EncryptedSource) EncryptedSource {
			value.SHA256 = append([]byte(nil), value.SHA256...)
			value.SHA256[0] ^= 0xff
			return value
		},
		"size":        func(value EncryptedSource) EncryptedSource { value.SizeBytes++; return value },
		"unknown key": func(value EncryptedSource) EncryptedSource { value.KeyVersion = 8; return value },
	} {
		t.Run(name, func(t *testing.T) {
			value := mutate(encrypted)
			tenant := "tenant-7"
			if name == "other tenant" {
				tenant = "tenant-8"
			}
			if _, err := cipher.Decrypt(tenant, "src_01", value); err == nil {
				t.Fatal("expected decryption rejection")
			}
		})
	}
}

func TestSourceCipherRejectsUnsafeKeysEmptySourceAndEntropyFailure(t *testing.T) {
	if _, err := NewSourceCipher(1, map[uint16][]byte{1: bytes.Repeat([]byte{1}, 16)}, bytes.NewReader(nil)); err == nil {
		t.Fatal("expected non-AES-256 key rejection")
	}
	if _, err := NewSourceCipher(2, map[uint16][]byte{1: bytes.Repeat([]byte{1}, 32)}, bytes.NewReader(nil)); err == nil {
		t.Fatal("expected missing active key rejection")
	}
	cipher, err := NewSourceCipher(1, map[uint16][]byte{1: bytes.Repeat([]byte{1}, 32)}, bytes.NewReader(nil))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cipher.Encrypt("tenant-7", "src_01", nil); err == nil {
		t.Fatal("expected empty source rejection")
	}
	if _, err := cipher.Encrypt("tenant-7", "src_01", []byte("x")); !errors.Is(err, ErrSourceEncryption) {
		t.Fatalf("entropy error = %v", err)
	}
}

func TestDecodeSourceKeyRingRotatesWithoutOrphaningHistoricalCiphertext(t *testing.T) {
	keyV1 := bytes.Repeat([]byte{0x31}, 32)
	keyV2 := bytes.Repeat([]byte{0x32}, 32)
	legacy, err := NewSourceCipher(1, map[uint16][]byte{1: keyV1}, bytes.NewReader(bytes.Repeat([]byte{0x41}, sourceNonceBytes)))
	if err != nil {
		t.Fatal(err)
	}
	historical, err := legacy.Encrypt("tenant-a", "source-a", []byte("historical"))
	if err != nil {
		t.Fatal(err)
	}
	ring := `{"1":"` + base64.StdEncoding.EncodeToString(keyV1) + `","2":"` + base64.StdEncoding.EncodeToString(keyV2) + `"}`
	rotated, err := DecodeSourceKeyRing("2", ring, bytes.NewReader(bytes.Repeat([]byte{0x42}, sourceNonceBytes)))
	if err != nil {
		t.Fatal(err)
	}
	current, err := rotated.Encrypt("tenant-a", "source-b", []byte("current"))
	if err != nil || current.KeyVersion != 2 {
		t.Fatalf("current=%+v error=%v", current, err)
	}
	recovered, err := rotated.Decrypt("tenant-a", "source-a", historical)
	if err != nil || string(recovered) != "historical" {
		t.Fatalf("historical=%q error=%v", recovered, err)
	}
}

func TestDecodeSourceKeyRingRejectsAmbiguousOrInvalidConfiguration(t *testing.T) {
	key := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x51}, 32))
	for name, input := range map[string]struct{ active, ring string }{
		"missing active": {active: "2", ring: `{"1":"` + key + `"}`},
		"duplicate":      {active: "1", ring: `{"1":"` + key + `","1":"` + key + `"}`},
		"short key":      {active: "1", ring: `{"1":"` + base64.StdEncoding.EncodeToString(make([]byte, 16)) + `"}`},
		"trailing":       {active: "1", ring: `{"1":"` + key + `"} true`},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := DecodeSourceKeyRing(input.active, input.ring, bytes.NewReader(make([]byte, sourceNonceBytes))); err == nil || strings.Contains(err.Error(), key) {
				t.Fatalf("error=%v", err)
			}
		})
	}
}
