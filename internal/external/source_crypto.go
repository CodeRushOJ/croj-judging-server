package external

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

const sourceNonceBytes = 12
const sourceCiphertextOverheadBytes = 16

var ErrSourceEncryption = errors.New("source encryption failed")

type EncryptedSource struct {
	Ciphertext []byte
	Nonce      []byte
	KeyVersion uint16
	SHA256     []byte
	SizeBytes  int64
}

func (EncryptedSource) String() string { return "[REDACTED ENCRYPTED SOURCE]" }

type SourceCipher struct {
	activeVersion uint16
	keys          map[uint16][]byte
	random        io.Reader
}

func NewSourceCipher(activeVersion uint16, keys map[uint16][]byte, random io.Reader) (*SourceCipher, error) {
	if activeVersion == 0 || random == nil {
		return nil, fmt.Errorf("active source key version and cryptographic random source are required")
	}
	copied := make(map[uint16][]byte, len(keys))
	for version, key := range keys {
		if version == 0 || len(key) != 32 {
			return nil, fmt.Errorf("source key version %d must be an AES-256 key", version)
		}
		copied[version] = append([]byte(nil), key...)
	}
	if _, exists := copied[activeVersion]; !exists {
		return nil, fmt.Errorf("active source key version %d is missing", activeVersion)
	}
	return &SourceCipher{activeVersion: activeVersion, keys: copied, random: random}, nil
}

func (sourceCipher *SourceCipher) Encrypt(tenantID, sourceObjectID string, plaintext []byte) (EncryptedSource, error) {
	if sourceCipher == nil || len(plaintext) == 0 || tenantID == "" || sourceObjectID == "" {
		return EncryptedSource{}, fmt.Errorf("tenant, source object, and non-empty source are required")
	}
	gcm, err := sourceCipher.gcm(sourceCipher.activeVersion)
	if err != nil {
		return EncryptedSource{}, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(sourceCipher.random, nonce); err != nil {
		return EncryptedSource{}, fmt.Errorf("%w: nonce entropy: %v", ErrSourceEncryption, err)
	}
	digest := sha256.Sum256(plaintext)
	ciphertext := gcm.Seal(nil, nonce, plaintext, sourceAAD(tenantID, sourceObjectID, sourceCipher.activeVersion))
	return EncryptedSource{
		Ciphertext: ciphertext,
		Nonce:      nonce,
		KeyVersion: sourceCipher.activeVersion,
		SHA256:     append([]byte(nil), digest[:]...),
		SizeBytes:  int64(len(plaintext)),
	}, nil
}

func (sourceCipher *SourceCipher) Decrypt(tenantID, sourceObjectID string, encrypted EncryptedSource) ([]byte, error) {
	if sourceCipher == nil || tenantID == "" || sourceObjectID == "" || encrypted.SizeBytes <= 0 || len(encrypted.SHA256) != sha256.Size {
		return nil, fmt.Errorf("encrypted source metadata is invalid")
	}
	gcm, err := sourceCipher.gcm(encrypted.KeyVersion)
	if err != nil {
		return nil, err
	}
	if len(encrypted.Nonce) != gcm.NonceSize() || len(encrypted.Ciphertext) < gcm.Overhead() {
		return nil, fmt.Errorf("encrypted source payload is invalid")
	}
	plaintext, err := gcm.Open(nil, encrypted.Nonce, encrypted.Ciphertext, sourceAAD(tenantID, sourceObjectID, encrypted.KeyVersion))
	if err != nil {
		return nil, fmt.Errorf("decrypt source: authentication failed")
	}
	digest := sha256.Sum256(plaintext)
	if int64(len(plaintext)) != encrypted.SizeBytes || !hmac.Equal(digest[:], encrypted.SHA256) {
		clear(plaintext)
		return nil, fmt.Errorf("decrypt source: metadata verification failed")
	}
	return plaintext, nil
}

func (sourceCipher *SourceCipher) gcm(version uint16) (cipher.AEAD, error) {
	key, exists := sourceCipher.keys[version]
	if !exists {
		return nil, fmt.Errorf("source key version %d is unavailable", version)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("create source cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("create source GCM: %w", err)
	}
	if gcm.NonceSize() != sourceNonceBytes {
		return nil, fmt.Errorf("unexpected source nonce size %d", gcm.NonceSize())
	}
	return gcm, nil
}

func sourceAAD(tenantID, sourceObjectID string, version uint16) []byte {
	buffer := make([]byte, 0, 10+len(tenantID)+len(sourceObjectID))
	buffer = binary.BigEndian.AppendUint32(buffer, uint32(len(tenantID)))
	buffer = append(buffer, tenantID...)
	buffer = binary.BigEndian.AppendUint32(buffer, uint32(len(sourceObjectID)))
	buffer = append(buffer, sourceObjectID...)
	buffer = binary.BigEndian.AppendUint16(buffer, version)
	return buffer
}
