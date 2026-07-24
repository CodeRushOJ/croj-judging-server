package external

import (
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"net/url"
	"path"
	"sort"
	"strconv"
	"strings"
)

const callbackSecretNonceBytes = 12

var ErrCallbackEncryption = errors.New("callback secret encryption failed")

type CallbackDestination struct {
	URL  string
	Host string
	Port uint16
}

type EncryptedCallbackSecret struct {
	Ciphertext []byte
	Nonce      []byte
	KeyVersion uint16
}

func (EncryptedCallbackSecret) String() string   { return "[REDACTED ENCRYPTED CALLBACK SECRET]" }
func (EncryptedCallbackSecret) GoString() string { return "[REDACTED ENCRYPTED CALLBACK SECRET]" }

type CallbackMaterial struct {
	CallbackID string
	Secret     string
}

func (CallbackMaterial) String() string   { return "[REDACTED CALLBACK MATERIAL]" }
func (CallbackMaterial) GoString() string { return "[REDACTED CALLBACK MATERIAL]" }

type CallbackCipher struct {
	activeVersion uint16
	keys          map[uint16][]byte
	random        io.Reader
}

func CanonicalCallbackDestination(raw string) (CallbackDestination, error) {
	if raw == "" || strings.TrimSpace(raw) != raw {
		return CallbackDestination{}, fmt.Errorf("callback destination is empty or has surrounding whitespace")
	}
	parsed, err := url.Parse(raw)
	if err != nil || !strings.EqualFold(parsed.Scheme, "https") || parsed.Host == "" || parsed.User != nil || parsed.Fragment != "" || parsed.Opaque != "" {
		return CallbackDestination{}, fmt.Errorf("callback destination must be an absolute HTTPS URL without userinfo or fragment")
	}
	host := strings.ToLower(parsed.Hostname())
	if !validDNSName(host) {
		return CallbackDestination{}, fmt.Errorf("callback destination host must be an ASCII DNS name")
	}
	if _, err := netip.ParseAddr(host); err == nil {
		return CallbackDestination{}, fmt.Errorf("callback destination must not use an IP literal")
	}
	port := uint64(443)
	if parsed.Port() != "" {
		port, err = strconv.ParseUint(parsed.Port(), 10, 16)
		if err != nil || port == 0 {
			return CallbackDestination{}, fmt.Errorf("callback destination port is invalid")
		}
	}
	escapedPath := parsed.EscapedPath()
	if escapedPath == "" {
		escapedPath = "/"
	}
	escapedPath = path.Clean(escapedPath)
	if !strings.HasPrefix(escapedPath, "/") {
		escapedPath = "/" + escapedPath
	}
	decodedPath, err := url.PathUnescape(escapedPath)
	if err != nil {
		return CallbackDestination{}, fmt.Errorf("callback destination path is invalid")
	}
	query, err := url.ParseQuery(parsed.RawQuery)
	if err != nil {
		return CallbackDestination{}, fmt.Errorf("callback destination query is invalid")
	}
	for key := range query {
		sort.Strings(query[key])
	}
	canonical := &url.URL{
		Scheme:   "https",
		Host:     net.JoinHostPort(host, strconv.FormatUint(port, 10)),
		Path:     decodedPath,
		RawPath:  escapedPath,
		RawQuery: query.Encode(),
	}
	return CallbackDestination{URL: canonical.String(), Host: host, Port: uint16(port)}, nil
}

func NewCallbackCipher(activeVersion uint16, keys map[uint16][]byte, random io.Reader) (*CallbackCipher, error) {
	if activeVersion == 0 || random == nil || len(keys) == 0 {
		return nil, fmt.Errorf("active callback key version, key ring, and cryptographic random source are required")
	}
	copied := make(map[uint16][]byte, len(keys))
	for version, key := range keys {
		if version == 0 || len(key) != 32 {
			return nil, fmt.Errorf("callback key version %d must be an AES-256 key", version)
		}
		copied[version] = append([]byte(nil), key...)
	}
	if _, exists := copied[activeVersion]; !exists {
		return nil, fmt.Errorf("active callback key version %d is missing", activeVersion)
	}
	return &CallbackCipher{activeVersion: activeVersion, keys: copied, random: random}, nil
}

func (callbackCipher *CallbackCipher) Encrypt(tenantID, callbackID, destination string, plaintext []byte) (EncryptedCallbackSecret, error) {
	if callbackCipher == nil || !externalIDPattern.MatchString(tenantID) || !externalIDPattern.MatchString(callbackID) || len(plaintext) < 32 || len(plaintext) > 1024 {
		return EncryptedCallbackSecret{}, fmt.Errorf("callback secret encryption input is invalid")
	}
	canonical, err := CanonicalCallbackDestination(destination)
	if err != nil {
		return EncryptedCallbackSecret{}, err
	}
	gcm, err := callbackCipher.gcm(callbackCipher.activeVersion)
	if err != nil {
		return EncryptedCallbackSecret{}, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(callbackCipher.random, nonce); err != nil {
		return EncryptedCallbackSecret{}, fmt.Errorf("%w: nonce entropy", ErrCallbackEncryption)
	}
	ciphertext := gcm.Seal(nil, nonce, plaintext, callbackAAD(tenantID, callbackID, canonical.URL, callbackCipher.activeVersion))
	return EncryptedCallbackSecret{Ciphertext: ciphertext, Nonce: nonce, KeyVersion: callbackCipher.activeVersion}, nil
}

func (callbackCipher *CallbackCipher) Decrypt(tenantID, callbackID, destination string, encrypted EncryptedCallbackSecret) ([]byte, error) {
	if callbackCipher == nil || !externalIDPattern.MatchString(tenantID) || !externalIDPattern.MatchString(callbackID) {
		return nil, fmt.Errorf("encrypted callback secret metadata is invalid")
	}
	canonical, err := CanonicalCallbackDestination(destination)
	if err != nil {
		return nil, err
	}
	gcm, err := callbackCipher.gcm(encrypted.KeyVersion)
	if err != nil {
		return nil, err
	}
	if len(encrypted.Nonce) != gcm.NonceSize() || len(encrypted.Ciphertext) <= gcm.Overhead() {
		return nil, fmt.Errorf("encrypted callback secret payload is invalid")
	}
	plaintext, err := gcm.Open(nil, encrypted.Nonce, encrypted.Ciphertext, callbackAAD(tenantID, callbackID, canonical.URL, encrypted.KeyVersion))
	if err != nil {
		return nil, fmt.Errorf("%w: authentication failed", ErrCallbackEncryption)
	}
	if len(plaintext) < 32 || len(plaintext) > 1024 {
		clear(plaintext)
		return nil, fmt.Errorf("%w: plaintext length is invalid", ErrCallbackEncryption)
	}
	return plaintext, nil
}

func (callbackCipher *CallbackCipher) gcm(version uint16) (cipher.AEAD, error) {
	key, exists := callbackCipher.keys[version]
	if !exists {
		return nil, fmt.Errorf("callback key version %d is unavailable", version)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("create callback cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("create callback GCM: %w", err)
	}
	if gcm.NonceSize() != callbackSecretNonceBytes {
		return nil, fmt.Errorf("unexpected callback nonce size %d", gcm.NonceSize())
	}
	return gcm, nil
}

func callbackAAD(tenantID, callbackID, destination string, version uint16) []byte {
	buffer := make([]byte, 0, len(tenantID)+len(callbackID)+len(destination)+14)
	for _, value := range []string{tenantID, callbackID, destination} {
		buffer = binary.BigEndian.AppendUint32(buffer, uint32(len(value)))
		buffer = append(buffer, value...)
	}
	return binary.BigEndian.AppendUint16(buffer, version)
}

func DecodeCallbackKeyRing(active, encoded string, random io.Reader) (*CallbackCipher, error) {
	parsedActive, err := strconv.ParseUint(active, 10, 16)
	if err != nil || parsedActive == 0 {
		return nil, fmt.Errorf("callback active key version must be an integer between 1 and 65535")
	}
	decoder := json.NewDecoder(strings.NewReader(encoded))
	first, err := decoder.Token()
	if err != nil || first != json.Delim('{') {
		return nil, fmt.Errorf("callback key ring must be a JSON object")
	}
	keys := make(map[uint16][]byte)
	defer func() {
		for _, key := range keys {
			clear(key)
		}
	}()
	for decoder.More() {
		rawVersion, err := decoder.Token()
		if err != nil {
			return nil, fmt.Errorf("decode callback key version")
		}
		versionText, ok := rawVersion.(string)
		if !ok {
			return nil, fmt.Errorf("callback key version is invalid")
		}
		parsedVersion, err := strconv.ParseUint(versionText, 10, 16)
		if err != nil || parsedVersion == 0 {
			return nil, fmt.Errorf("callback key version must be an integer between 1 and 65535")
		}
		version := uint16(parsedVersion)
		if _, exists := keys[version]; exists {
			return nil, fmt.Errorf("callback key version %d is duplicated", version)
		}
		var encodedKey string
		if err := decoder.Decode(&encodedKey); err != nil {
			return nil, fmt.Errorf("decode callback key version %d", version)
		}
		key, err := base64.StdEncoding.DecodeString(encodedKey)
		if err != nil || len(key) != 32 {
			clear(key)
			return nil, fmt.Errorf("callback key version %d must be 32 bytes encoded as base64", version)
		}
		keys[version] = key
	}
	closing, err := decoder.Token()
	if err != nil || closing != json.Delim('}') {
		return nil, fmt.Errorf("callback key ring object is incomplete")
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("callback key ring contains trailing data")
	}
	return NewCallbackCipher(uint16(parsedActive), keys, random)
}
